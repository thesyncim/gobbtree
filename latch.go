package gobbtree

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A node's latch, upstream's NodeLatch (tree/node/latch.rs): one word per
// node, in its header. From the low bit: an
// exclusive bit, a count of shared holders (20 bits), and a version (43
// bits). Letting go of an exclusive hold bumps the version, so a reader that
// read the node meanwhile fails its check; shared holds leave it alone.
//
// Internal nodes are read optimistically and taken exclusively by a
// compare-and-swap at the version a descent read, never waiting. Leaves are
// latched, shared or exclusive, waiting with backoff; a lookup reads its
// leaf optimistically first. A node's word stays with its page for life and
// its version only grows (type-stable pages), so a reader's check can't find
// an old version again.
const (
	latchExclusive  uint64 = 1
	latchSharedUnit uint64 = 1 << 1
	latchSharedMask uint64 = ((1 << 20) - 1) << 1
	latchVersion    uint64 = 1 << 21
)

// orderingZero is never written. An address offset by a loaded value masked
// with it carries an address dependency on that value: arm64 performs the
// dependent load after the load it depends on, and amd64 orders all loads.
// The Go memory model does not promise this; it is the hardware's rule, and
// every plain optimistic load is validated by a version check relying on it,
// where upstream issues an acquire fence.
var orderingZero uintptr

func after[T any](p *T, acc uint64) *T {
	return (*T)(unsafe.Add(unsafe.Pointer(p), uintptr(acc)&orderingZero))
}

// nodeLatch is a node's latch word.
type nodeLatch struct{ atomic.Uint64 }

// readVersion is the node's version once no thread holds it exclusively.
func (l *nodeLatch) readVersion() uint64 {
	if word := l.Load(); word&latchExclusive == 0 {
		return word &^ latchSharedMask
	}
	return l.readVersionContended()
}

//go:noinline
func (l *nodeLatch) readVersionContended() uint64 {
	var b backoff
	for {
		b.spin()
		if word := l.Load(); word&latchExclusive == 0 {
			return word &^ latchSharedMask
		}
	}
}

// isUnchanged reports that the node is still at version, read before. The
// caller's plain loads of the node must complete first: on arm64 it passes
// them through after, or orders them with loadFence.
func (l *nodeLatch) isUnchanged(version uint64) bool {
	return l.Load()&^latchSharedMask == version
}

// isUnchangedAfter is isUnchanged for a reader whose plain loads all feed acc:
// the latch word's address depends on them, so it loads after them.
func (l *nodeLatch) isUnchangedAfter(version, acc uint64) bool {
	if !seqlockPlainStores {
		return l.isUnchanged(version)
	}
	return *after((*uint64)(unsafe.Pointer(&l.Uint64)), acc)&^latchSharedMask == version
}

// tryLockAt takes the node exclusively if it is at version, unheld.
func (l *nodeLatch) tryLockAt(version uint64) bool {
	return l.CompareAndSwap(version, version|latchExclusive)
}

// unlockExclusive lets go of a node held from version, at the next version.
func (l *nodeLatch) unlockExclusive(version uint64) {
	if orderedStores {
		storeOrdered(&l.Uint64, version+latchVersion)
		return
	}
	l.Store(version + latchVersion)
}

// tryReadVersion is the node's version, unless a thread holds it exclusively.
func (l *nodeLatch) tryReadVersion() (uint64, bool) {
	word := l.Load()
	return word &^ latchSharedMask, word&latchExclusive == 0
}

// tryLockExclusive takes the node exclusively unless any thread holds it.
func (l *nodeLatch) tryLockExclusive() (uint64, bool) {
	word := l.Load()
	if word&(latchExclusive|latchSharedMask) != 0 || !l.tryLockAt(word) {
		return 0, false
	}
	return word, true
}

// lockExclusive waits for the node and takes it exclusively.
func (l *nodeLatch) lockExclusive() uint64 {
	if word := l.Load(); word&(latchExclusive|latchSharedMask) == 0 && l.tryLockAt(word) {
		return word
	}
	return l.lockExclusiveContended()
}

//go:noinline
func (l *nodeLatch) lockExclusiveContended() uint64 {
	var b backoff
	for {
		b.spin()
		if word := l.Load(); word&(latchExclusive|latchSharedMask) == 0 && l.tryLockAt(word) {
			return word
		}
	}
}

// lockShared waits while a thread holds the node exclusively, then holds it
// shared. The latch has no writer preference.
func (l *nodeLatch) lockShared() {
	if word := l.Load(); word&latchExclusive == 0 && l.CompareAndSwap(word, word+latchSharedUnit) {
		return
	}
	l.lockSharedContended()
}

//go:noinline
func (l *nodeLatch) lockSharedContended() {
	var b backoff
	for {
		if word := l.Load(); word&latchExclusive == 0 && l.CompareAndSwap(word, word+latchSharedUnit) {
			return
		}
		b.spin()
	}
}

func (l *nodeLatch) unlockShared() { l.Add(^(latchSharedUnit - 1)) }

// backoff waits exponentially longer, as upstream's does. Go has no spin
// hint, so past the last round it yields the processor instead.
type backoff struct{ round uint32 }

func (b *backoff) spin() {
	const maxRound = 10
	if b.round >= maxRound {
		runtime.Gosched()
		return
	}
	for i := 0; i < 1<<b.round; i++ {
		spinWait()
	}
	b.round++
}

//go:noinline
func spinWait() {}

// storeLock is a spin lock, for the leaf blocks. Where stores stay ordered
// it is released by a plain store instead of sync.Mutex's locked
// read-modify-write. Waiters yield instead of parking.
type storeLock struct{ state atomic.Uint32 }

func (l *storeLock) TryLock() bool { return l.state.CompareAndSwap(0, 1) }
func (l *storeLock) Lock() {
	for !l.TryLock() {
		runtime.Gosched()
	}
}
func (l *storeLock) Unlock() {
	if orderedStores {
		*(*uint32)(unsafe.Pointer(&l.state)) = 0
		return
	}
	l.state.Store(0)
}

// plainPointer loads a pointer published by an atomic store, plainly: what
// it points to is immutable once published, and reached by an address that
// depends on the load.
func plainPointer[T any](p *atomic.Pointer[T]) *T { return *(**T)(unsafe.Pointer(p)) }

// storeOrdered stores v with a plain store, for orderedStores targets only.
func storeOrdered(p *atomic.Uint64, v uint64) { *(*uint64)(unsafe.Pointer(p)) = v }

// Latch modes of a descent's leaf, upstream's LatchMode.
const (
	optimistic = iota
	shared
	exclusive
)
