package gobbtree

import (
	"math/bits"
	"sync/atomic"
)

// Each bucket names the deepest node, a leaf parent or above, below the root
// whose key range wholly contains it: a leaf parent where keys spread evenly,
// a higher node where they cluster, so that a run of keys narrower than a
// bucket still skips the upper levels. Buckets no such node contains use the
// ordinary descent, so the table never predicts or approximates a key.
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
	// The domain runs to the largest key, not the last separator: keys
	// beyond it, every append's among them, would otherwise all descend.
	largest := root.keys[root.n-1]
	if last := t.node(t.end(true)); last.n > 0 {
		largest = max(largest, last.entries()[last.n-1].key)
	}
	width := bits.Len64(root.keys[0] ^ largest)
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
		ids := t.routes.ids
		if len(ids) != count {
			ids = make([]uint32, count)
			t.routesTop = 0
		}
		// A changed domain rebuilds every cell below. Reuse equal-sized
		// storage; shared readers validate the structural epoch around atomic
		// cell loads, just as they do for an ordinary partial repair.
		t.routes = wordRoutes{base: base, shift: shift, depth: depth, ids: ids}
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
	// Buckets that straddle the subtree's bounds belong to its unchanged
	// ancestors; only those it wholly contains are cleared and refilled.
	// Buckets above the largest key's stay empty: ascending inserts change
	// the rightmost subtree constantly, and refilling every bucket up to the
	// domain's end each time would cost as much as the inserts. Clearing
	// reaches the highest bucket ever filled, which a smaller largest key
	// leaves behind.
	mask := (uint64(1) << shift) - 1
	top := min(int((largest-base)>>shift), count-1)
	if first, last := t.routes.within(low, high); first <= last {
		reached := last
		last = min(last, max(top, t.routesTop))
		for i := first; i <= last; i++ {
			t.storeRoute(i, 0)
		}
		t.fillRoutes(id, int(t.node(id).level)-1, low, min(high, base|uint64(top)<<shift|mask))
		if reached >= t.routesTop {
			t.routesTop = top
		} else {
			t.routesTop = max(t.routesTop, top)
		}
	}
}

// within returns the buckets wholly inside [low, high]; first > last if none.
func (r *wordRoutes) within(low, high uint64) (first, last int) {
	mask := (uint64(1) << r.shift) - 1
	limit := r.base | uint64(len(r.ids)-1)<<r.shift | mask
	low, high = max(low, r.base), min(high, limit)
	if low > high {
		return 1, 0
	}
	first, last = int((low-r.base)>>r.shift), int((high-r.base)>>r.shift)
	if (low-r.base)&mask != 0 {
		first++
	}
	if (high-r.base)&mask != mask {
		last--
	}
	return first, last
}
func (t *wordTree) fillRoutes(id uint32, depth int, low, high uint64) {
	r := &t.routes
	first, last := r.within(low, high)
	if first > last {
		return
	}
	if depth == 0 {
		for i := first; i <= last; i++ {
			t.storeRoute(i, id)
		}
		return
	}
	n := t.node(id)
	if id != t.root {
		// A bucket holding one of the node's separators straddles two of its
		// children, so no deeper node contains it; this node does if the
		// bucket lies within its range.
		mask := (uint64(1) << r.shift) - 1
		for i := 1; i < int(n.n); i++ {
			separator := n.keys[i]
			if separator < r.base || (separator-r.base)&mask == 0 {
				continue
			}
			if bucket := int((separator - r.base) >> r.shift); bucket >= first && bucket <= last {
				t.storeRoute(bucket, id)
			}
		}
	}
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
