package gobbtree

import (
	"sync/atomic"
	"unsafe"
)

const wordLeafID uint32 = 1 << 31

// The prefix uses the two sibling fields only in internal nodes. Both views
// contain integers; the word is naturally aligned.
func (n *wordNode) prefixWord() *uint64  { return (*uint64)(unsafe.Pointer(&n.prev)) }
func (n *wordNode) tagWords() *[4]uint64 { return (*[4]uint64)(unsafe.Pointer(&n.tags)) }

// storeTags publishes a leaf's tags as four atomic words, for the latched
// tree's readers in atomic-store builds. Fixed-width integer views preserve
// native byte order.
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

// storeOrdered stores v with a plain store, for orderedStores targets only.
func storeOrdered(p *atomic.Uint64, v uint64) { *(*uint64)(unsafe.Pointer(p)) = v }

// A Tree's node stores: its one owner writes plainly.
func (t *wordTree) storeEntry(n *wordNode, i int, value wordEntry) { n.entries()[i] = value }
func (t *wordTree) copyEntries(n *wordNode, i int, src []wordEntry) {
	copy(n.entries()[i:], src)
}
func (t *wordTree) moveEntries(n *wordNode, dst, src, count int) {
	copy(n.entries()[dst:dst+count], n.entries()[src:src+count])
}
func (t *wordTree) storeKey(n *wordNode, i int, key uint64)       { n.keys[i] = key }
func (t *wordTree) copyKeys(n *wordNode, i int, src []uint64)     { copy(n.keys[i:], src) }
func (t *wordTree) storeChild(n *wordNode, i int, child uint32)   { n.inner().children[i] = child }
func (t *wordTree) copyChildren(n *wordNode, i int, src []uint32) { copy(n.inner().children[i:], src) }
func (t *wordTree) moveKeys(n *wordNode, dst, src, count int) {
	copy(n.keys[dst:dst+count], n.keys[src:src+count])
}
func (t *wordTree) moveChildren(n *wordNode, dst, src, count int) {
	copy(n.inner().children[dst:dst+count], n.inner().children[src:src+count])
}
func (t *wordTree) clearChildren(n *wordNode, start int) {
	clear(n.inner().children[start:])
}
