package gobbtree

import (
	"runtime"
	"unsafe"
)

// A shared tree's node latch, upstream's NodeLatch (tree/node/latch.rs): one
// word per node, in the node's sequence field. From the low bit: an
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

// readVersion is the node's version once no thread holds it exclusively.
func (n *wordNode) readVersion() uint64 {
	if word := n.sequence.Load(); word&latchExclusive == 0 {
		return word &^ latchSharedMask
	}
	return n.readVersionContended()
}

//go:noinline
func (n *wordNode) readVersionContended() uint64 {
	var b backoff
	for {
		b.spin()
		if word := n.sequence.Load(); word&latchExclusive == 0 {
			return word &^ latchSharedMask
		}
	}
}

// isUnchanged reports that the node is still at version, read before. The
// caller's plain loads of the node must complete first: on arm64 it passes
// them through after, or orders them with loadFence.
func (n *wordNode) isUnchanged(version uint64) bool {
	return n.sequence.Load()&^latchSharedMask == version
}

// isUnchangedAfter is isUnchanged for a reader whose plain loads all feed acc:
// the latch word's address depends on them, so it loads after them.
func (n *wordNode) isUnchangedAfter(version, acc uint64) bool {
	if !seqlockPlainStores {
		return n.isUnchanged(version)
	}
	return *after((*uint64)(unsafe.Pointer(&n.sequence)), acc)&^latchSharedMask == version
}

// tryLockAt takes the node exclusively if it is at version, unheld.
func (n *wordNode) tryLockAt(version uint64) bool {
	return n.sequence.CompareAndSwap(version, version|latchExclusive)
}

// unlockExclusive lets go of a node held from version, at the next version.
func (n *wordNode) unlockExclusive(version uint64) {
	if orderedStores {
		storeOrdered(&n.sequence, version+latchVersion)
		return
	}
	n.sequence.Store(version + latchVersion)
}

// tryReadVersion is the node's version, unless a thread holds it exclusively.
func (n *wordNode) tryReadVersion() (uint64, bool) {
	word := n.sequence.Load()
	return word &^ latchSharedMask, word&latchExclusive == 0
}

// tryLockExclusive takes the node exclusively unless any thread holds it.
func (n *wordNode) tryLockExclusive() (uint64, bool) {
	word := n.sequence.Load()
	if word&(latchExclusive|latchSharedMask) != 0 || !n.tryLockAt(word) {
		return 0, false
	}
	return word, true
}

// lockExclusive waits for the node and takes it exclusively.
func (n *wordNode) lockExclusive() uint64 {
	if word := n.sequence.Load(); word&(latchExclusive|latchSharedMask) == 0 && n.tryLockAt(word) {
		return word
	}
	return n.lockExclusiveContended()
}

//go:noinline
func (n *wordNode) lockExclusiveContended() uint64 {
	var b backoff
	for {
		b.spin()
		if word := n.sequence.Load(); word&(latchExclusive|latchSharedMask) == 0 && n.tryLockAt(word) {
			return word
		}
	}
}

// lockShared waits while a thread holds the node exclusively, then holds it
// shared. The latch has no writer preference.
func (n *wordNode) lockShared() {
	if word := n.sequence.Load(); word&latchExclusive == 0 && n.sequence.CompareAndSwap(word, word+latchSharedUnit) {
		return
	}
	n.lockSharedContended()
}

//go:noinline
func (n *wordNode) lockSharedContended() {
	var b backoff
	for {
		if word := n.sequence.Load(); word&latchExclusive == 0 && n.sequence.CompareAndSwap(word, word+latchSharedUnit) {
			return
		}
		b.spin()
	}
}

func (n *wordNode) unlockShared() { n.sequence.Add(^(latchSharedUnit - 1)) }

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
