package gobbtree

import (
	"math/bits"
	"sync/atomic"
)

// Slots remain live when a child publishes a replacement image. Structural
// changes publish a new table before advancing the same epoch readers check.
type publishedRoutes[K any, V any, O keyOrder[K]] struct {
	base  uint64
	shift uint8
	slots []*atomic.Pointer[searchImage[K, V, O]]
}

func (t *treeCore[K, V, O]) changedRoutes(n *node[K, V, O]) {
	if t.shared && t.root.level >= 2 && n.level+min(2, t.root.level-1) > t.root.level {
		t.routesDirty = true
	}
}
func (m *mapCore[K, V, O]) rebuildRoutes() {
	m.tree.routesDirty = false
	root := m.tree.root
	if root == nil || root.strings || root.level < 2 {
		m.routes.Store(nil)
		return
	}
	width := 64 - bits.LeadingZeros64(root.codes[0]^root.codes[root.n-1])
	radixBits := min(12, int(root.level)*4, width)
	if radixBits < 4 {
		m.routes.Store(nil)
		return
	}
	routes := &publishedRoutes[K, V, O]{
		base:  root.codes[0] & (^uint64(0) << width),
		shift: uint8(width - radixBits),
		slots: make([]*atomic.Pointer[searchImage[K, V, O]], 1<<radixBits),
	}
	routes.fill(root, int(min(2, root.level-1)), 0, ^uint64(0))
	m.routes.Store(routes)
}
func (r *publishedRoutes[K, V, O]) fill(n *node[K, V, O], depth int, low, high uint64) {
	if depth == 0 {
		mask := (uint64(1) << r.shift) - 1
		limit := r.base | uint64(len(r.slots)-1)<<r.shift | mask
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
			r.slots[i] = n.parentSlot
		}
		return
	}
	for i := 0; i < n.n; i++ {
		begin, end := low, high
		if i > 0 {
			begin = n.codes[i]
		}
		if i+1 < n.n {
			end = n.codes[i+1] - 1
		}
		r.fill(n.children[i], depth-1, begin, end)
	}
}
