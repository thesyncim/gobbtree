package gobbtree

import (
	"math/bits"
	"sync/atomic"
	"unsafe"
)

const wordLeafID uint32 = 1 << 31
const wordSequenceStep uint64 = 1 << 13

func appendWordBlock(blocks []atomic.Pointer[wordNode], nodes []wordNode) []atomic.Pointer[wordNode] {
	used := len(blocks)
	if used == cap(blocks) {
		grown := make([]atomic.Pointer[wordNode], used, max(1, 2*used))
		for i := range blocks {
			grown[i].Store(blocks[i].Load())
		}
		blocks = grown
	}
	blocks = blocks[:used+1]
	blocks[used].Store(&nodes[0])
	return blocks
}

// The prefix uses the two sibling fields only in internal nodes. Both views
// contain integers; the word is naturally aligned and every shared access is atomic.
func (n *wordNode) prefixWord() *uint64  { return (*uint64)(unsafe.Pointer(&n.prev)) }
func (n *wordNode) tagWords() *[4]uint64 { return (*[4]uint64)(unsafe.Pointer(&n.tags)) }

// Fixed-width integer views preserve native byte order. Only the shared
// source/destination uses atomics; the local vector input is an ordinary copy.
func (n *wordNode) storeTags(tags [nodeSize]uint8) {
	src := (*[4]uint64)(unsafe.Pointer(&tags))
	dst := n.tagWords()
	atomic.StoreUint64(&dst[0], src[0])
	atomic.StoreUint64(&dst[1], src[1])
	atomic.StoreUint64(&dst[2], src[2])
	atomic.StoreUint64(&dst[3], src[3])
}
func loadSharedTags(src *[4]uint64) [nodeSize]uint8 {
	words := [4]uint64{atomic.LoadUint64(&src[0]), atomic.LoadUint64(&src[1]), atomic.LoadUint64(&src[2]), atomic.LoadUint64(&src[3])}
	return *(*[nodeSize]uint8)(unsafe.Pointer(&words))
}
func (n *wordNode) loadTags() [nodeSize]uint8 { return loadSharedTags(n.tagWords()) }
func (n *wordNode) publishNumeric(atomicWrites bool) {
	var tags [nodeSize]uint8
	shift := 0
	if !n.leaf && n.n > 0 {
		shift = max(0, bits.Len64(n.keys[0]^n.keys[n.n-1])-8)
		prefix := n.keys[0] & (^uint64(0) << uint(shift+8))
		if atomicWrites {
			atomic.StoreUint64(n.prefixWord(), prefix)
		} else {
			*n.prefixWord() = prefix
		}
	}
	for i := 0; i < int(n.n); i++ {
		tags[i] = uint8(n.keyAt(i) >> shift)
	}
	if !n.leaf {
		for i := int(n.n); i < nodeSize; i++ {
			tags[i] = 255
		}
	}
	if atomicWrites {
		n.storeTags(tags)
	} else {
		n.tags = tags
	}
	sequence := n.sequence.Load()
	n.sequence.Store(sequence&^(wordSequenceStep-2) | uint64(n.n)<<1 | uint64(shift)<<7)
}

// The leaf or structural lock excludes other writers while tags are copied.
// Atomic publication preserves the optimistic readers' epoch validation.
func (n *wordNode) insertTag(i int, key uint64) {
	tags := n.tags
	copy(tags[i+1:n.n], tags[i:n.n-1])
	tags[i] = uint8(key)
	n.storeTags(tags)
	n.sequence.Store(n.sequence.Load()&^(wordSequenceStep-2) | uint64(n.n)<<1)
}
func (n *wordNode) deleteTag(i int) {
	tags := n.tags
	copy(tags[i:n.n], tags[i+1:n.n+1])
	tags[n.n] = 0
	n.storeTags(tags)
	n.sequence.Store(n.sequence.Load()&^(wordSequenceStep-2) | uint64(n.n)<<1)
}

func (n *wordNode) beginLeafWrite() { n.sequence.Add(1) }
func (n *wordNode) endLeafWrite()   { n.sequence.Add(wordSequenceStep - 1) }

func (t *wordTree) storeEntry(n *wordNode, i int, value wordEntry) {
	if !t.atomicWrites {
		n.entries()[i] = value
		return
	}
	entry := &n.entries()[i]
	atomic.StoreUint64(&entry.key, value.key)
	atomic.StoreUint64(&entry.value, value.value)
}
func (t *wordTree) copyEntries(n *wordNode, i int, src []wordEntry) {
	if !t.atomicWrites {
		copy(n.entries()[i:], src)
		return
	}
	entries := n.entries()
	for j, value := range src {
		entry := &entries[i+j]
		atomic.StoreUint64(&entry.key, value.key)
		atomic.StoreUint64(&entry.value, value.value)
	}
}
func (t *wordTree) moveEntries(n *wordNode, dst, src, count int) {
	if !t.atomicWrites {
		copy(n.entries()[dst:dst+count], n.entries()[src:src+count])
		return
	}
	entries := n.entries()
	if dst > src {
		for i := count - 1; i >= 0; i-- {
			value := entries[src+i]
			entry := &entries[dst+i]
			atomic.StoreUint64(&entry.key, value.key)
			atomic.StoreUint64(&entry.value, value.value)
		}
	} else {
		for i := 0; i < count; i++ {
			value := entries[src+i]
			entry := &entries[dst+i]
			atomic.StoreUint64(&entry.key, value.key)
			atomic.StoreUint64(&entry.value, value.value)
		}
	}
}
func (t *wordTree) storeKey(n *wordNode, i int, key uint64) {
	if t.atomicWrites {
		atomic.StoreUint64(&n.keys[i], key)
	} else {
		n.keys[i] = key
	}
}
func (t *wordTree) copyKeys(n *wordNode, i int, src []uint64) {
	if !t.atomicWrites {
		copy(n.keys[i:], src)
		return
	}
	for j, key := range src {
		t.storeKey(n, i+j, key)
	}
}
func (t *wordTree) moveKeys(n *wordNode, dst, src, count int) {
	if !t.atomicWrites {
		copy(n.keys[dst:dst+count], n.keys[src:src+count])
		return
	}
	if dst > src {
		for i := count - 1; i >= 0; i-- {
			t.storeKey(n, dst+i, n.keys[src+i])
		}
	} else {
		for i := 0; i < count; i++ {
			t.storeKey(n, dst+i, n.keys[src+i])
		}
	}
}
func (t *wordTree) storeChild(n *wordNode, i int, child uint32) {
	if t.atomicWrites {
		atomic.StoreUint32(&n.inner().children[i], child)
	} else {
		n.inner().children[i] = child
	}
}
func (t *wordTree) copyChildren(n *wordNode, i int, src []uint32) {
	if !t.atomicWrites {
		copy(n.inner().children[i:], src)
		return
	}
	for j, child := range src {
		t.storeChild(n, i+j, child)
	}
}
func (t *wordTree) moveChildren(n *wordNode, dst, src, count int) {
	if !t.atomicWrites {
		copy(n.inner().children[dst:dst+count], n.inner().children[src:src+count])
		return
	}
	if dst > src {
		for i := count - 1; i >= 0; i-- {
			t.storeChild(n, dst+i, n.inner().children[src+i])
		}
	} else {
		for i := 0; i < count; i++ {
			t.storeChild(n, dst+i, n.inner().children[src+i])
		}
	}
}
func (t *wordTree) clearChildren(n *wordNode, start int) {
	for i := start; i < nodeSize; i++ {
		t.storeChild(n, i, 0)
	}
}
