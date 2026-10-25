package gobbtree

import "math/bits"

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
func (t *wordTree) rebuildRoutes() {
	t.routesDirty = false
	root := t.rootNode
	if root == nil || root.level < 2 {
		t.routes = wordRoutes{}
		return
	}
	width := 64 - bits.LeadingZeros64(root.keys[0]^root.keys[root.n-1])
	radixBits := min(12, int(root.level)*4, width)
	if radixBits < 4 {
		t.routes = wordRoutes{}
		return
	}
	count := 1 << radixBits
	if len(t.routes.ids) != count {
		t.routes.ids = make([]uint32, count)
	} else {
		clear(t.routes.ids)
	}
	t.routes.base = root.keys[0] & (^uint64(0) << width)
	t.routes.shift = uint8(width - radixBits)
	t.routes.depth = min(2, root.level-1)
	t.fillRoutes(t.root, int(t.routes.depth), 0, ^uint64(0))
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
			r.ids[i] = id
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
