package gobbtree

import (
	"slices"
	"sync/atomic"
	"unsafe"
)

// Hint tables for trees of integer keys, the word trees' cells
// (word_latched_routes.go): one per region of the key space, each naming
// the leaves that hold it, up to four, by ID, and where each starts. A hint
// is never trusted. The leaf it names settles a key only where, read at a
// version that holds, it has entries and the key lies between its first
// key and its last: separators are leaves' first keys (D49), so a leaf's
// first key is its low fence, and every key from it to the leaf's last is
// the leaf's. A key past the last, below the next leaf's first, descends.
// So a stale cell, a leaf merged away or an ID given to another leaf costs
// only a descent.
//
// A leaf's ID is its slot in the tree's leaf directory, given as the leaf
// is made and taken back as it is merged away. Descents fill cells from the
// leaves' parents as they pass them, with the fences their routes read, as
// nodes keep none; a new table is filled whole, for a root that has none or
// has doubled its children since. A tree without two levels of internal
// nodes keeps an empty one: no level would be skipped.

// leafDir is a tree's leaves by ID, ID 0 naming none. The leaves live in
// blocks of leafBlock, the word trees' slabs: a leaf's address is its
// block's plus its place in it, so a hint reaches its leaf without loading
// a slot from a table the size of the tree's leaves. A leaf merged away
// gives its slot back, and the next leaf made takes it: its latch's
// version goes on from the old one's, so a reader still on the old leaf
// finds it changed. Writers that make or merge away a leaf take the lock;
// readers load the blocks racing, and check the leaf they reach.
type leafDir struct {
	mu     storeLock
	blocks atomic.Pointer[[]unsafe.Pointer]
	free   []uint32
	next   uint32
	// stride is a leaf's size in its block, and newBlock makes a block of
	// leafBlock leaves of the tree's type, which the collector scans as
	// that type.
	stride   uintptr
	newBlock func() unsafe.Pointer
}

const leafBlock = 256

// alloc is a slot for a new leaf and its ID: a merged-away leaf's, or the
// next; nil once every ID a cell can name is taken.
func (d *leafDir) alloc() (*gheader, uint32) {
	d.mu.Lock()
	id := uint32(0)
	if n := len(d.free); n > 0 {
		id, d.free = d.free[n-1], d.free[:n-1]
	} else if d.next < hintIDMask {
		d.next++
		id = d.next
	}
	if id == 0 {
		d.mu.Unlock()
		return nil, 0
	}
	blocks := d.blocks.Load()
	if blocks == nil || int(id/leafBlock) >= len(*blocks) {
		// A reader of the old blocks misses the leaves made since, which
		// descend.
		var grown []unsafe.Pointer
		if blocks != nil {
			grown = append(grown, *blocks...)
		}
		grown = append(grown, d.newBlock())
		d.blocks.Store(&grown)
		blocks = &grown
	}
	h := (*gheader)(unsafe.Add((*blocks)[id/leafBlock], uintptr(id%leafBlock)*d.stride))
	d.mu.Unlock()
	return h, id
}

// remove gives leaf h's slot back, for a leaf merged away.
func (d *leafDir) remove(h *gheader) {
	if h.id == 0 {
		return
	}
	d.mu.Lock()
	d.free = append(d.free, h.id)
	d.mu.Unlock()
}

// reset forgets every leaf, for a tree cleared.
func (d *leafDir) reset() {
	d.mu.Lock()
	d.blocks.Store(nil)
	d.free, d.next = nil, 0
	d.mu.Unlock()
}

// leaf is the slot of ID id, read racing, or nil: a leaf, or one merged
// away, which holds nothing.
func (d *leafDir) leaf(id uint32) *gheader {
	blocks := plainPointer(&d.blocks)
	if blocks == nil || int(id/leafBlock) >= len(*blocks) {
		return nil
	}
	return (*gheader)(unsafe.Add((*blocks)[id/leafBlock], uintptr(id%leafBlock)*d.stride))
}

// currentHints is the tree's hint table, made anew where its root has
// none or doubled its children since: one goroutine makes it while others
// go on with the old, and a tree whose root is a leaf keeps the old one,
// whose hints are checked where used.
func currentHints(slot *atomic.Pointer[codeHints], making *atomic.Bool, root *gheader) *codeHints {
	r := plainPointer(slot)
	if root == nil || root.leaf {
		return r
	}
	if r == nil || r.top != root || 2*r.topCount <= racingCount(root) {
		if making.CompareAndSwap(false, true) {
			r = refreshCodeHints(slot, root)
			making.Store(false)
		}
	}
	return r
}

// fillFrom fills code's cell from the leaf parent a descent recorded in f.
func (r *codeHints) fillFrom(f *hintFrame, code uint64) {
	if r == nil || f.p == nil {
		return
	}
	if c := r.cell(code); c != nil {
		r.fill(c, code, f, true)
	}
}

// codeHints is a tree's hint table, made for root while it had count
// children.
type codeHints struct {
	latchedRoutes
	top      *gheader
	topCount int
}

// hintFrame is where a descent met a leaf's parent: p, with its fences,
// reached from g, with g's, at edge; g is nil at the root. A high fence of
// 0 is none: the node is on the tree's right border.
type hintFrame struct {
	p, g        *innerRoute
	low, high   uint64
	gLow, gHigh uint64
	edge        int
}

// racingCount is internal node n's count, read racing, at most nodeSize.
func racingCount(h *gheader) int {
	return min(max(int(*(*int32)(unsafe.Pointer(&h.count))), 0), nodeSize)
}

func routeCodes(n *innerRoute) *[nodeSize]uint64 {
	return (*[nodeSize]uint64)(unsafe.Pointer(&n.codes))
}

// leafFor is the leaf the hint for code names, read racing, or nil.
func (r *codeHints) leafFor(d *leafDir, code uint64) *gheader {
	c := r.cell(code)
	if c == nil {
		return nil
	}
	id := r.latchedRoutes.leafFor(c, code)
	if id == 0 {
		return nil
	}
	return d.leaf(id &^ wordLeafID)
}

// hintedCodeBy is hintedCode by calls to its parts.
func hintedCodeBy(r *codeHints, d *leafDir, code uint64, place bool) (h *gheader, i int, found, settled bool) {
	if r == nil {
		return nil, 0, false, false
	}
	if h = d.leaf(r.hintID(code)); h == nil {
		return nil, 0, false, false
	}
	i, found, settled = settleCode(h, code, place)
	return h, i, found, settled
}

// hintID is the ID of the leaf code's hint names, 0 for none, its cell
// read as latchedRoutes.leafFor reads it, written out.
func (r *codeHints) hintID(code uint64) uint32 {
	c := r.cell(code)
	if c == nil {
		return 0
	}
	head, tail := c.head, c.tail
	if head <= hintDescend {
		return 0
	}
	q := r.fraction(code)
	id := head & hintIDMask
	if q >= head>>48&1023 {
		id = head >> 24 & hintIDMask
	}
	if q >= head>>58|(tail>>48&15)<<6 {
		id = tail & hintIDMask
	}
	if q >= tail>>52&1023 {
		id = tail >> 24 & hintIDMask
	}
	return uint32(id)
}

// settleCode is hintedCode's search of the leaf h its hint named.
func settleCode(h *gheader, code uint64, place bool) (i int, found, settled bool) {
	n := (*leafRoute)(unsafe.Pointer(h))
	count := owned(&n.count)
	i = firstTag(&n.tags, count, code)
	if i < count && n.codes[i&(nodeSize-1)] == code {
		return i, true, true
	}
	if count == 0 || code < n.codes[0] || code > n.codes[(count-1)&(nodeSize-1)] {
		return 0, false, false
	}
	if i < count || place {
		// Another code has its tag, or its slot is asked for.
		i = lowerCode(&n.codes, count, code)
		return i, n.codes[i&(nodeSize-1)] == code, true
	}
	return i, false, true
}

// fill names in cell c the leaves that hold key's region, up to four, and
// otherwise, where descend, marks it to descend. They are children of f.p,
// key's leaf's parent, and where the region crosses its fences, of the
// parent beside it under f.g. Its reads race writers.
func (r *codeHints) fill(c *hintCell, key uint64, f *hintFrame, descend bool) {
	start, end := r.regionOf(key)
	var span hintSpan
	if start < f.low && f.g != nil && f.edge > 0 {
		low := f.gLow
		if f.edge > 1 {
			low = routeCodes(f.g)[(f.edge-1)&(nodeSize-1)]
		}
		span.addCodes(innerChild(f.g, f.edge-1), low, f.low, start, f.low-1)
	}
	span.addCodes(f.p, f.low, f.high, max(start, f.low), end)
	if f.high != 0 && end >= f.high && f.g != nil && f.edge+1 < racingCount(&f.g.gheader) {
		high := f.gHigh
		if f.edge+2 < racingCount(&f.g.gheader) {
			high = routeCodes(f.g)[(f.edge+2)&(nodeSize-1)]
		}
		span.addCodes(innerChild(f.g, f.edge+1), f.high, high, f.high, end)
	}
	r.store(c, &span, start, end, descend)
}

// innerChild is internal node g's child at edge where it is internal, read
// racing, or nil.
func innerChild(g *innerRoute, edge int) *innerRoute {
	h := plainPointer(&g.children[edge&(nodeSize-1)])
	if h == nil || h.leaf {
		return nil
	}
	return (*innerRoute)(unsafe.Pointer(h))
}

// addCodes appends leaf parent p's leaves that hold [from, to], which p's
// fences, low and high, hold.
func (s *hintSpan) addCodes(p *innerRoute, low, high, from, to uint64) {
	if p == nil || p.leaf || from > to {
		s.broken = true
		return
	}
	count := racingCount(&p.gheader)
	if count == 0 || !holds(low, high, from) || !holds(low, high, to) {
		s.broken = true
		return
	}
	codes := routeCodes(p)
	first, last := routeCode(codes, count, from), routeCode(codes, count, to)
	if s.n == 0 {
		s.first = from
	}
	s.last = to
	for i := first; i <= last; i++ {
		child := plainPointer(&p.children[i&(nodeSize-1)])
		if child == nil || !child.leaf || child.id == 0 || s.n == len(s.ids) {
			s.broken = true
			return
		}
		start := from
		if i > first {
			start = codes[i&(nodeSize-1)]
		}
		s.ids[s.n], s.starts[s.n] = uint64(child.id), start
		s.n++
	}
}

// refreshCodeHints is the hint table in slot for root, made anew where
// there is none for it or root has doubled its children since, as the
// latched tree's refreshRoutes makes its own; nil where root is a leaf. Its
// reads race writers; a heuristic needs no check, and a hint is checked
// where used.
func refreshCodeHints(slot *atomic.Pointer[codeHints], root *gheader) *codeHints {
	if root == nil || root.leaf {
		return nil
	}
	n := (*innerRoute)(unsafe.Pointer(root))
	count := racingCount(root)
	if count < 2 {
		return nil
	}
	if current := slot.Load(); current != nil && current.top == root && current.topCount*2 > count {
		return current
	}
	r := &codeHints{top: root, topCount: count}
	if first := plainPointer(&n.children[0]); first != nil && !first.leaf {
		span := sampleCodeSpan(n, count)
		if span == 0 {
			return nil
		}
		// The domain runs from the first key to the last, not between the
		// root's separators: a new root has one, and keys beyond them,
		// appends' among them, would find no hint.
		leaves, lo, hi := codeShape(n)
		codes := routeCodes(n)
		r.latchedRoutes = *newHintTable(min(codes[1], lo), max(codes[(count-1)&(nodeSize-1)], hi), span, leaves)
		r.fillAll(n, 0, 0, nil, 0, 0, 0, 0)
	}
	slot.Store(r)
	return r
}

// codeShape estimates how many leaves the tree under root has, from the
// child counts along its leftmost path, and reads its first key and its
// last, at its borders' ends. Its reads race writers.
func codeShape(root *innerRoute) (leaves int, first, last uint64) {
	leaves, first = 1, ^uint64(0)
	h := &root.gheader
	for depth := 0; h != nil && depth < maxDepth; depth++ {
		if h.leaf {
			if racingCount(h) > 0 {
				first = (*leafRoute)(unsafe.Pointer(h)).codes[0]
			}
			break
		}
		leaves *= max(racingCount(h), 1)
		h = plainPointer(&(*innerRoute)(unsafe.Pointer(h)).children[0])
	}
	h = &root.gheader
	for depth := 0; h != nil && depth < maxDepth; depth++ {
		count := racingCount(h)
		if count == 0 {
			break
		}
		if h.leaf {
			last = (*leafRoute)(unsafe.Pointer(h)).codes[(count-1)&(nodeSize-1)]
			break
		}
		h = plainPointer(&(*innerRoute)(unsafe.Pointer(h)).children[(count-1)&(nodeSize-1)])
	}
	return leaves, first, last
}

// sampleCodeSpan is sampleLeafSpan for a generic tree: the median gap
// between neighboring separators of leaf parents under a few of the root's
// children. Its reads race writers.
func sampleCodeSpan(root *innerRoute, count int) uint64 {
	var gaps []uint64
	for j := 0; j < count && len(gaps) < 256; j += max(count/8, 1) {
		h := plainPointer(&root.children[j&(nodeSize-1)])
		for depth := 0; h != nil && !h.leaf && depth < maxDepth; depth++ {
			n := (*innerRoute)(unsafe.Pointer(h))
			child := plainPointer(&n.children[0])
			if child != nil && child.leaf {
				codes := routeCodes(n)
				for i := 1; i+1 < racingCount(h); i++ {
					if codes[i+1] > codes[i] {
						gaps = append(gaps, codes[i+1]-codes[i])
					}
				}
				break
			}
			h = child
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	slices.Sort(gaps)
	return gaps[len(gaps)/2]
}

// fillAll fills every cell the leaves under internal node n hold, n having
// fences low and high and been reached from g, with fences gLow and gHigh,
// at edge (g nil at the root): about two fills a leaf, once per table.
func (r *codeHints) fillAll(n *innerRoute, low, high uint64, g *innerRoute, gLow, gHigh uint64, edge, depth int) {
	count := racingCount(&n.gheader)
	if count == 0 || depth > maxDepth {
		return
	}
	codes := routeCodes(n)
	first := plainPointer(&n.children[0])
	if first == nil {
		return
	}
	if !first.leaf {
		for i := 0; i < count; i++ {
			child := innerChild(n, i)
			if child == nil {
				continue
			}
			childLow, childHigh := low, high
			if i > 0 {
				childLow = codes[i&(nodeSize-1)]
			}
			if i+1 < count {
				childHigh = codes[(i+1)&(nodeSize-1)]
			}
			r.fillAll(child, childLow, childHigh, n, low, high, i, depth+1)
		}
		return
	}
	// A leaf parent: each region from its low fence to its high one, or
	// on the right border to its last leaf's last key.
	last := high - 1
	if high == 0 {
		leaf := plainPointer(&n.children[(count-1)&(nodeSize-1)])
		if leaf == nil || !leaf.leaf || racingCount(leaf) == 0 {
			return
		}
		last = (*leafRoute)(unsafe.Pointer(leaf)).codes[(racingCount(leaf)-1)&(nodeSize-1)]
	}
	f := hintFrame{p: n, g: g, low: low, high: high, gLow: gLow, gHigh: gHigh, edge: edge}
	if last < low {
		return
	}
	step := uint64(1) << r.shift
	start := r.base + ((max(low, r.base)-r.base)>>r.shift)<<r.shift
	for key, regions := start, 0; key <= last && regions <= 4*nodeSize; key, regions = key+step, regions+1 {
		if c := r.cell(max(key, low)); c != nil {
			r.fill(c, max(key, low), &f, true)
		}
		if key+step < key {
			return
		}
	}
}

// descendCodeFilling is descendCode by calls to the search kernels, for an
// owner's lookup its hint didn't settle: it fills code's cell in r from the
// leaf's parent as it passes it.
func descendCodeFilling(h *gheader, code uint64, r *codeHints, place bool) (*gheader, int, bool) {
	var f hintFrame
	low, high, edge := uint64(0), uint64(0), 0
	for !h.leaf {
		n := (*innerRoute)(unsafe.Pointer(h))
		count := racingCount(h)
		codes := routeCodes(n)
		i := routeCode(codes, count, code)
		// n's parent, reached at edge, becomes its grandparent's place.
		f.g, f.gLow, f.gHigh, f.edge = f.p, f.low, f.high, edge
		f.p, f.low, f.high, edge = n, low, high, i
		if i > 0 {
			low = codes[i&(nodeSize-1)]
		}
		if i+1 < count {
			high = codes[(i+1)&(nodeSize-1)]
		}
		h = plainPointer(&n.children[i&(nodeSize-1)])
	}
	r.fillFrom(&f, code)
	i, ok := seekCode((*leafRoute)(unsafe.Pointer(h)), racingCount(h), code, place)
	return h, i, ok
}
