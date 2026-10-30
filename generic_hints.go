package gobbtree

import (
	"math/bits"
	"slices"
	"sync/atomic"
	"unsafe"
)

// Hint tables for trees of integer keys, upstream-era Go's leaf cache's cells
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
// has doubled its children since; a tree whose root is its leaves' parent
// keeps one too, whose hints skip the root.

// leafDir is a tree's leaves by ID, ID 0 naming none. The leaves live in
// blocks of leafBlock: a leaf's address is its
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

// alloc is a slot for a new leaf, which keeps its ID: a merged-away
// leaf's, or the next, fresh, as its block made it; nil once every ID a
// cell can name is taken.
func (d *leafDir) alloc() (h *gheader, fresh bool) {
	d.mu.Lock()
	id := uint32(0)
	if n := len(d.free); n > 0 {
		id, d.free = d.free[n-1], d.free[:n-1]
	} else if d.next < hintIDMask {
		d.next++
		id, fresh = d.next, true
	}
	if id == 0 {
		d.mu.Unlock()
		return nil, false
	}
	blocks := d.blocks.Load()
	if blocks == nil || int(id/leafBlock) >= len(*blocks) {
		// A reader of the old blocks misses the leaves made since, which
		// descend. The new block goes past the old ones' length, where
		// their array has room, which no reader of them reads: the blocks
		// grow by appending, not by a copy a block.
		var grown []unsafe.Pointer
		if blocks != nil {
			grown = *blocks
		}
		// Each slot is a leaf of its ID from the block's making, before
		// any reader can reach it: a reader on a merged-away leaf reads
		// these racing as its slot is taken again, so they never change.
		block := d.newBlock()
		for j := uintptr(0); j < leafBlock; j++ {
			if slot := uint32(len(grown)*leafBlock) + uint32(j); slot != 0 {
				h := (*gheader)(unsafe.Add(block, j*d.stride))
				h.leaf, h.id = true, slot
			}
		}
		grown = append(grown, block)
		d.blocks.Store(&grown)
		blocks = &grown
	}
	h = (*gheader)(unsafe.Add((*blocks)[id/leafBlock], uintptr(id%leafBlock)*d.stride))
	d.mu.Unlock()
	return h, fresh
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
func currentHints(slot *atomic.Pointer[codeHints], making *atomic.Bool, reshaped *atomic.Uint64, root *gheader) *codeHints {
	r := plainPointer(slot)
	if root == nil || root.leaf {
		return r
	}
	if r == nil || r.top != root || 2*r.topCount <= racingCount(root) {
		if making.CompareAndSwap(false, true) {
			r = refreshCodeHints(slot, reshaped, root)
			making.Store(false)
		}
	}
	return r
}

// fillFrom fills code's cell from the leaf parent a descent recorded in f.
func (r *codeHints) fillFrom(f *hintFrame, code uint64) {
	if r == nil || f.depth == 0 {
		return
	}
	if c := r.cell(code); c != nil {
		r.fill(c, code, f, true)
	}
}

// codeHints is a tree's hint table, made for root while it had count
// children.
type codeHints struct {
	hintTable
	top      *gheader
	topCount int
	// filled is the last key the tree held as the table was filled.
	filled uint64
}

// hintFrame is where a descent met a leaf's parent: the internal nodes it
// passed, the root first, each with the edge it took, the last the leaf's
// parent p, and p's fences. A high fence of 0 is none: p is on the tree's
// right border.
type hintFrame struct {
	steps     [maxDepth]hintStep
	depth     int
	low, high uint64
}

type hintStep struct {
	n    *innerRoute
	edge int
}

// push records internal node n, of fences low and high, and the edge its
// route took.
func (f *hintFrame) push(n *innerRoute, edge int, low, high uint64) {
	if f.depth < maxDepth {
		f.steps[f.depth] = hintStep{n, edge}
		f.depth++
		f.low, f.high = low, high
	}
}

// beside is the leaf parent beside p, to its left or right: under the
// lowest node of the path whose route can turn that way, down that
// subtree's nearer border; nil on the tree's border, or where a racing
// read finds no such node.
func (f *hintFrame) beside(left bool) *innerRoute {
	for l := f.depth - 2; l >= 0; l-- {
		s := f.steps[l]
		edge := s.edge
		if left && edge == 0 || !left && edge+1 >= racingCount(&s.n.gheader) {
			continue
		}
		if left {
			edge--
		} else {
			edge++
		}
		n := innerChild(s.n, edge)
		for d := l + 1; n != nil && d < f.depth-1; d++ {
			count := racingCount(&n.gheader)
			if count == 0 {
				return nil
			}
			next := 0
			if left {
				next = count - 1
			}
			n = innerChild(n, next)
		}
		return n
	}
	return nil
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
	id := r.hintID(code)
	if id == 0 {
		return nil
	}
	return d.leaf(id)
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

// hintID is the ID of the leaf code's hint names, 0 for none: a fraction
// that equals a cut, where a region's 1024ths are coarser than keys, goes
// to the leaf starting there, as a separator is at most that leaf's first
// key, and the leaf's bounds turn away any other.
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
	if first, high := bounds(h); !owns(count, first, high, code) {
		return 0, false, false
	}
	if i < count || place {
		// Another code has its tag, or its slot is asked for.
		i = lowerCode(&n.codes, count, code)
		return i, i < count && n.codes[i&(nodeSize-1)] == code, true
	}
	return i, false, true
}

// fill names in cell c the leaves that hold key's region, up to four, and
// otherwise, where descend, marks it to descend. They are children of p,
// key's leaf's parent, the last node of f's path, and where the region
// crosses p's fences, of the leaf parent beside it. A region past the
// tree's borders is marked without reading p's leaves, which cost a miss
// each, as its every lookup refills it. Its reads race writers.
func (r *codeHints) fill(c *hintCell, key uint64, f *hintFrame, descend bool) {
	start, end := r.regionOf(key)
	var span hintSpan
	var left, right *innerRoute
	if start < f.low {
		left = f.beside(true)
		span.broken = left == nil
	}
	if f.high != 0 && end >= f.high {
		right = f.beside(false)
		span.broken = span.broken || right == nil
	}
	if span.broken {
		r.store(c, &span, start, end, descend)
		return
	}
	if left != nil {
		span.addCodes(left, 0, f.low, start, f.low-1)
	}
	// p holds the region up to its high fence; the parent beside it, the
	// rest.
	pEnd := end
	if right != nil {
		pEnd = f.high - 1
	}
	span.addCodes(f.steps[f.depth-1].n, f.low, f.high, max(start, f.low), pEnd)
	if right != nil {
		span.addCodes(right, f.high, 0, f.high, end)
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
// there is none for it or root has doubled its children since; nil where
// root is a leaf. Its reads race writers; a heuristic needs no check, and
// a hint is checked where used.
func refreshCodeHints(slot *atomic.Pointer[codeHints], reshaped *atomic.Uint64, root *gheader) *codeHints {
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
	if first := plainPointer(&n.children[0]); first != nil {
		span := sampleCodeSpan(n, count)
		if span == 0 {
			return nil
		}
		// The domain runs from the first key to the last, not between the
		// root's separators: a new root has one, and keys beyond them,
		// appends' among them, would find no hint.
		leaves, lo, hi := codeShape(n)
		codes := routeCodes(n)
		r.hintTable = *newHintTable(min(codes[1], lo), max(codes[(count-1)&(nodeSize-1)], hi), span, leaves)
		r.filled = hi
		// A table over the old one's regions, as appends grow a tree to,
		// takes its cells below the leaves moves changed since and below
		// the old one's last key, and fills the rest: an append-built
		// tree's leaves are each filled about once, not once a doubling.
		// The cells kept are as good as they were: a hint is checked
		// where used.
		from := uint64(0)
		low := reshaped.Swap(^uint64(0))
		if old := slot.Load(); old != nil && old.filled != 0 && !old.hashed && !r.hashed && old.base == r.base && old.shift == r.shift && len(old.cells) <= len(r.cells) {
			if from = min(old.filled, low); from > r.base {
				copy(r.cells, old.cells)
				from, _ = r.regionOf(from)
			} else {
				from = 0
			}
		}
		var f hintFrame
		r.fillAll(n, 0, 0, &f, from)
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
	if first := plainPointer(&root.children[0]); first != nil && first.leaf {
		// The root is the leaf parent.
		codes := routeCodes(root)
		for i := 1; i+1 < count; i++ {
			if codes[i+1] > codes[i] {
				gaps = append(gaps, codes[i+1]-codes[i])
			}
		}
	}
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

// fillAll fills every cell the leaves under internal node n hold from key
// from on, n having fences low and high and been reached by f's path:
// about two fills a leaf, once per table.
func (r *codeHints) fillAll(n *innerRoute, low, high uint64, f *hintFrame, from uint64) {
	count := racingCount(&n.gheader)
	if count == 0 || f.depth >= maxDepth-1 {
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
			if childHigh != 0 && childHigh <= from {
				continue
			}
			f.push(n, i, low, high)
			r.fillAll(child, childLow, childHigh, f, from)
			f.depth--
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
	if last < max(low, from) {
		return
	}
	f.push(n, 0, low, high)
	defer func() { f.depth-- }()
	step := uint64(1) << r.shift
	start := r.base + ((max(low, r.base, from)-r.base)>>r.shift)<<r.shift
	codes, edge := routeCodes(n), 0
	for key, regions := start, 0; key <= last && regions <= 4*nodeSize; key, regions = key+step, regions+1 {
		c := r.cell(max(key, low))
		end := key + step - 1
		switch {
		case c == nil:
		case key < low || end < key || high != 0 && end >= high:
			// A region past n's fences takes the leaves beside it too.
			r.fill(c, max(key, low), f, true)
		default:
			// A region within them, the leaves n's children are from
			// where the last region's began, as fill would gather them.
			for edge+1 < count && codes[(edge+1)&(nodeSize-1)] <= key {
				edge++
			}
			span := hintSpan{first: key, last: end}
			for k := edge; k < count && (k == edge || codes[k&(nodeSize-1)] <= end); k++ {
				child := plainPointer(&n.children[k&(nodeSize-1)])
				if child == nil || !child.leaf || child.id == 0 || span.n == len(span.ids) {
					span.broken = true
					break
				}
				span.ids[span.n], span.starts[span.n] = uint64(child.id), key
				if k > edge {
					span.starts[span.n] = codes[k&(nodeSize-1)]
				}
				span.n++
			}
			r.store(c, &span, key, end, true)
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
	low, high := uint64(0), uint64(0)
	for !h.leaf {
		n := (*innerRoute)(unsafe.Pointer(h))
		count := racingCount(h)
		codes := routeCodes(n)
		i := routeCode(codes, count, code)
		f.push(n, i, low, high)
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

// hintTable is a hint table's cells and their domain: regions of 1<<shift
// keys from base, indexed directly, or where hashed, by bits of a hash.
type hintTable struct {
	base   uint64
	shift  uint8
	hashed bool
	bits   uint8
	cells  []hintCell
}

// A cell: four 24-bit leaf IDs and three 10-bit fractions of the region
// where the second, third and fourth start, as upstream-era Go's leaf cache
// packed them. A head of 0 is empty; 2 marks a region whose leaves no cell
// holds, which descends.
type hintCell struct{ head, tail uint64 }

const (
	hintIDMask  = 1<<24 - 1
	hintDescend = 2
)

// holds reports that fences low and high hold key, high 0 being none.
func holds(low, high, key uint64) bool { return low <= key && (high == 0 || key < high) }

// cell is key's region's cell, or nil outside the table's domain. A table
// over a domain no wider than itself indexes regions directly; a sparser
// one hashes them.
func (r *hintTable) cell(key uint64) *hintCell {
	region := (key - r.base) >> r.shift
	if r.hashed {
		return &r.cells[region*0x9e3779b97f4a7c15>>(64-r.bits)]
	}
	if region < uint64(len(r.cells)) {
		return &r.cells[region]
	}
	return nil
}

// fraction is key's position in its region, in 1024ths.
func (r *hintTable) fraction(key uint64) uint64 {
	if r.shift >= 10 {
		return key >> (r.shift - 10) & 1023
	}
	return (key & (uint64(1)<<r.shift - 1)) << (10 - r.shift)
}

// regionOf is key's region, from its start to its end.
func (r *hintTable) regionOf(key uint64) (start, end uint64) {
	start = r.base + ((key-r.base)>>r.shift)<<r.shift
	end = start + (uint64(1)<<r.shift - 1)
	if end < start {
		end = ^uint64(0)
	}
	return start, end
}

// store names in cell c the leaves span gathered for the region from start
// to end where they hold it whole, up to four; otherwise, where descend,
// it marks the cell to descend, and leaves it as it was where not.
func (r *hintTable) store(c *hintCell, span *hintSpan, start, end uint64, descend bool) {
	head, tail := uint64(hintDescend), uint64(0)
	covered := span.n > 0 && span.n <= 4 && span.covers(start, end)
	if !covered && !descend {
		return
	}
	if covered {
		ids, cuts := span.ids, [3]uint64{1023, 1023, 1023}
		for i := span.n; i < 4; i++ {
			ids[i] = ids[span.n-1]
		}
		for i := 1; i < span.n; i++ {
			cuts[i-1] = r.fraction(span.starts[i])
		}
		head = ids[0] | ids[1]<<24 | cuts[0]<<48 | (cuts[1]&63)<<58
		tail = ids[2] | ids[3]<<24 | (cuts[1]>>6)<<48 | cuts[2]<<52
	}
	// Only a change is written: every core reads the table, so a store of
	// what is there already costs each of them its line.
	if c.head != head || c.tail != tail {
		c.tail, c.head = tail, head
	}
}

// hintSpan gathers, in key order, the leaves that hold a region and where
// each starts, up to five: one more than a cell holds, to tell it overflowed.
type hintSpan struct {
	ids    [5]uint64
	starts [5]uint64
	n      int
	first  uint64
	last   uint64
	broken bool
}

// covers reports that the gathered leaves hold [start, end] whole.
func (s *hintSpan) covers(start, end uint64) bool {
	return !s.broken && s.first == start && s.last == end
}

// newHintTable makes a hint table for about leaves leaves whose span is
// about span, over keys from lo to hi. A domain with no more regions of
// half a leaf's span to a whole one than about two for each leaf is indexed
// directly. A sparser one, such as clustered keys', is hashed: its regions
// span a leaf's span up to twice it, within a cell's four leaves, into
// about four cells for each leaf, as hashed regions collide.
func newHintTable(lo, hi, span uint64, leaves int) *hintTable {
	tableBits := min(max(bits.Len(uint(leaves)), 8), 22)
	shift := max(bits.Len64(span)-1, 0)
	width := bits.Len64(lo ^ hi)
	if width < 64 && width+1-shift <= tableBits+1 {
		// Room for keys beyond the separators, as ascending keys add them.
		width++
	}
	r := &hintTable{shift: uint8(min(shift, 63))}
	if width-shift <= tableBits+1 {
		r.base = lo &^ (uint64(1)<<width - 1)
		r.cells = make([]hintCell, 1<<max(width-shift, 0))
	} else {
		r.shift = uint8(min(shift+1, 63))
		r.hashed, r.bits = true, uint8(min(tableBits+2, 22))
		r.cells = make([]hintCell, 1<<r.bits)
	}
	return r
}
