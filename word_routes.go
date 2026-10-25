package gobbtree

import (
	"math/bits"
	"sync/atomic"
)

// Only buckets wholly contained in a subtree get an entry. Boundary buckets
// use the ordinary descent, so the table never predicts or approximates a key.
type wordRoutes struct {
	base  uint64
	shift uint8
	depth uint8
	ids   []uint32
}

func (r *wordRoutes) lookup(key uint64) uint32 {
	index := (key - r.base) >> r.shift
	if index < uint64(len(r.ids)) {
		return r.ids[index]
	}
	return 0
}
func (r *wordRoutes) lookupAtomic(key uint64) uint32 {
	index := (key - r.base) >> r.shift
	if index < uint64(len(r.ids)) {
		return atomic.LoadUint32(&r.ids[index])
	}
	return 0
}
func (t *wordTree) storeRoute(index int, id uint32) {
	if t.shared {
		atomic.StoreUint32(&t.routes.ids[index], id)
	} else {
		t.routes.ids[index] = id
	}
}

// Cache leaf-parent nodes. A mutation refreshes only its highest changed
// ancestor's interval; a new root or key domain rebuilds the whole directory.
// Shared readers validate the structural epoch around atomic cell loads.
func (t *wordTree) rebuildRoutes() {
	t.routesDirty = false
	dirty := t.routeNode
	t.routeNode = nil
	root := t.rootNode
	if root == nil || root.level < 2 {
		t.routes = wordRoutes{}
		return
	}
	width := bits.Len64(root.keys[0] ^ root.keys[root.n-1])
	radixBits := min(20, int(root.level)*4, width)
	if radixBits < 4 {
		t.routes = wordRoutes{}
		return
	}
	count := 1 << radixBits
	base := root.keys[0] & (^uint64(0) << width)
	shift, depth := uint8(width-radixBits), root.level-1
	same := len(t.routes.ids) == count && t.routes.base == base && t.routes.shift == shift && t.routes.depth == depth
	if !same {
		t.routes = wordRoutes{base: base, shift: shift, depth: depth, ids: make([]uint32, count)}
	}
	id := t.root
	low, high := uint64(0), ^uint64(0)
	if same && dirty != nil {
		for t.node(id) != dirty && t.node(id).level > dirty.level {
			n := t.node(id)
			i := t.route(n, dirty.keys[0])
			if i > 0 {
				low = n.keys[i]
			}
			if i+1 < int(n.n) {
				high = n.keys[i+1] - 1
			}
			id = n.inner().children[i]
		}
		if t.node(id) != dirty {
			id = t.root
			low, high = 0, ^uint64(0)
		}
	}
	mask := (uint64(1) << shift) - 1
	limit := base | uint64(count-1)<<shift | mask
	start, end := max(low, base), min(high, limit)
	if start <= end {
		first, last := int((start-base)>>shift), int((end-base)>>shift)
		for i := first; i <= last; i++ {
			t.storeRoute(i, 0)
		}
		t.fillRoutes(id, int(t.node(id).level)-1, low, high)
	}
}
func (t *wordTree) fillRoutes(id uint32, depth int, low, high uint64) {
	if depth == 0 {
		r := &t.routes
		mask := (uint64(1) << r.shift) - 1
		limit := r.base | uint64(len(r.ids)-1)<<r.shift | mask
		low = max(low, r.base)
		high = min(high, limit)
		if low > high {
			return
		}
		first, last := int((low-r.base)>>r.shift), int((high-r.base)>>r.shift)
		if (low-r.base)&mask != 0 {
			first++
		}
		if (high-r.base)&mask != mask {
			last--
		}
		for i := first; i <= last; i++ {
			t.storeRoute(i, id)
		}
		return
	}
	n := t.node(id)
	for i := 0; i < int(n.n); i++ {
		begin, end := low, high
		if i > 0 {
			begin = n.keys[i]
		}
		if i+1 < int(n.n) {
			end = n.keys[i+1] - 1
		}
		t.fillRoutes(n.inner().children[i], depth-1, begin, end)
	}
}
