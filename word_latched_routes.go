package gobbtree

import (
	"math/bits"
	"slices"
)

// Route hints let a descent skip the internal levels: a table of cells, one
// per region of the key space, each naming the leaves that hold its region,
// up to four, with the region's fractions where each next leaf starts. A
// hint is never trusted. The descent reads the named leaf's version, checks
// that it holds entries and that its fences hold the key, and only then
// uses it; otherwise it descends from the root. So a stale or torn cell
// costs only a descent.
//
// A node's fences are the lower bound of every key under it (an internal
// node's first separator) and a high fence, the lower bound of the next
// node's keys, 0 on the tree's right border. Separators move whole between
// nodes but never change value (upstream's "fence values move but never
// change"), so a node's fences change only when it splits, merges or trades
// an entry with a sibling, while it is held. Its version vouches for them as
// for everything else in it; a freed node holds nothing.
//
// Regions span about a leaf or two, so cells stay few, about one for every
// leaf and a half, and the table in cache. Lookups fill a cell as they pass
// the leaves' parent; only plain-store builds keep hints, as only their
// readers route by windows.
type latchedRoutes struct {
	root      uint32
	rootCount int
	base      uint64
	shift     uint8
	hashed    bool
	bits      uint8
	cells     []hintCell
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

// A node's fences: low, the lower bound of its keys, in the prefix word
// (an internal node's first separator, which its windows share), and high.
func lowFence(n *wordNode) uint64  { return *n.prefixWord() }
func highFence(n *wordNode) uint64 { return n.high }

func setHighFence(n *wordNode, high uint64) {
	if seqlockPlainStores {
		n.high = high
	}
}

// setLeafFences sets a leaf's fences; an internal node's low fence is its
// first separator, which refitWindows keeps.
func setLeafFences(n *wordNode, low, high uint64) {
	if seqlockPlainStores {
		*n.prefixWord() = low
		n.high = high
	}
}

// holds reports that fences low and high hold key.
func holds(low, high, key uint64) bool { return low <= key && (high == 0 || key < high) }

// cell is key's region's cell, or nil outside the table's domain. A table
// over a domain no wider than itself indexes regions directly; a sparser
// one hashes them.
func (r *latchedRoutes) cell(key uint64) *hintCell {
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
func (r *latchedRoutes) fraction(key uint64) uint64 {
	if r.shift >= 10 {
		return key >> (r.shift - 10) & 1023
	}
	return (key & (uint64(1)<<r.shift - 1)) << (10 - r.shift)
}

// leafFor is the leaf cell c names for key, or 0. A fraction that equals a
// cut, where a region's 1024ths are coarser than keys, goes to the leaf
// starting there: a separator is that leaf's first key, which lookups ask
// for, and the fences turn away any other key.
func (r *latchedRoutes) leafFor(c *hintCell, key uint64) uint32 {
	head := c.head
	if head <= hintDescend {
		return 0
	}
	// A cell of one leaf names it four times; selects, not branches,
	// pick among them.
	first, second := head&hintIDMask, head>>24&hintIDMask
	tail := c.tail
	cut0 := head >> 48 & 1023
	cut1 := head>>58 | (tail>>48&15)<<6
	cut2 := tail >> 52 & 1023
	q := r.fraction(key)
	id := first
	if q >= cut0 {
		id = second
	}
	if q >= cut1 {
		id = tail & hintIDMask
	}
	if q >= cut2 {
		id = tail >> 24 & hintIDMask
	}
	return uint32(id) | wordLeafID
}

// fill names in cell c the leaves that hold key's region, up to four, and
// otherwise marks it to descend. They are children of p, key's leaf's
// parent, and where the region crosses p's fences, of the parent beside it
// under g, p's parent, which reached p at edge; g is nil at the root. Its
// reads race writers; a hint is checked where used.
func (r *latchedRoutes) fill(dir []*wordNode, c *hintCell, key uint64, p, g *wordNode, edge int) {
	start := r.base + ((key-r.base)>>r.shift)<<r.shift
	end := start + (uint64(1)<<r.shift - 1)
	if end < start {
		end = ^uint64(0)
	}
	var span hintSpan
	low, high := lowFence(p), highFence(p)
	if start < low && g != nil && edge > 0 {
		span.add(r, nodeOf(dir, g, edge-1), start, low-1)
	}
	span.add(r, p, max(start, low), end)
	if high != 0 && end >= high && g != nil && edge+1 < min(int(g.n), nodeSize) {
		span.add(r, nodeOf(dir, g, edge+1), high, end)
	}
	head, tail := uint64(hintDescend), uint64(0)
	if span.n > 0 && span.n <= 4 && span.covers(start, end) {
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

// nodeOf is internal node g's internal child at edge, read racing, or nil.
func nodeOf(dir []*wordNode, g *wordNode, edge int) *wordNode {
	id := g.inner().children[edge&(nodeSize-1)]
	if id&wordLeafID != 0 {
		return nil
	}
	return nodeIn(dir, id)
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

// add appends parent p's leaves that hold [from, to], which p's fences hold.
func (s *hintSpan) add(r *latchedRoutes, p *wordNode, from, to uint64) {
	if p == nil || p.leaf || from > to {
		s.broken = true
		return
	}
	count := min(int(p.n), nodeSize)
	low, high := lowFence(p), highFence(p)
	if count == 0 || !holds(low, high, from) || !holds(low, high, to) {
		s.broken = true
		return
	}
	children := &p.inner().children
	first, last := routeCode(&p.keys, count, from), routeCode(&p.keys, count, to)
	if s.n == 0 {
		s.first = from
	}
	s.last = to
	for i := first; i <= last; i++ {
		child := children[i&(nodeSize-1)]
		id := uint64(child &^ wordLeafID)
		if child&wordLeafID == 0 || id == 0 || id > hintIDMask || s.n == len(s.ids) {
			s.broken = true
			return
		}
		start := from
		if i > first {
			start = p.keys[i&(nodeSize-1)]
		}
		s.ids[s.n], s.starts[s.n] = id, start
		s.n++
	}
}

// covers reports that the gathered leaves hold [start, end] whole.
func (s *hintSpan) covers(start, end uint64) bool {
	return !s.broken && s.first == start && s.last == end
}

// refreshRoutes makes a hint table for the current root when there is none,
// or the root changed or doubled its children since. A region spans half a
// leaf's span up to a whole one, the median gap between separators of a few
// leaf parents, whatever the keys' distribution. A domain, the root's
// separators' span, with no more regions than cells for a leaf and a half
// is indexed directly; a sparse one, such as clustered keys', is hashed. The
// tree needs internal nodes above its leaf parents for hints to skip any.
func (t *latchedTree) refreshRoutes(dir []*wordNode) *latchedRoutes {
	rootID := t.root.Load()
	root := nodeIn(dir, rootID)
	if root == nil || rootID&wordLeafID != 0 {
		return nil
	}
	version, ok := root.tryReadVersion()
	if !ok {
		return nil
	}
	count, level := int(root.n), int(root.level)
	if count < 2 || count > nodeSize {
		return nil
	}
	lo, hi := root.keys[1], root.keys[count-1]
	loadFence()
	if !root.isUnchanged(version) || t.root.Load() != rootID {
		return nil
	}
	if current := t.routes.Load(); current != nil && current.root == rootID && current.rootCount*2 > count {
		return current
	}
	r := &latchedRoutes{root: rootID, rootCount: count}
	if level >= 2 {
		span := sampleLeafSpan(dir, root, count)
		if span == 0 {
			return nil
		}
		// Nodes hold about twenty children or entries.
		leaves := count
		for range level - 1 {
			leaves *= 20
		}
		tableBits := min(max(bits.Len(uint(leaves)), 8), 22)
		shift := max(bits.Len64(span)-1, 0)
		width := bits.Len64(lo ^ hi)
		r.shift = uint8(min(shift, 63))
		if width-shift <= tableBits+1 {
			// Few enough regions to index directly.
			r.base = lo &^ (uint64(1)<<width - 1)
			r.cells = make([]hintCell, 1<<max(width-shift, 0))
		} else {
			// Hashed regions collide: wider ones, a leaf's span up to
			// twice it, still within a cell's four leaves, and about four
			// cells for every leaf.
			r.shift = uint8(min(shift+1, 63))
			r.hashed, r.bits = true, uint8(min(tableBits+2, 22))
			r.cells = make([]hintCell, 1<<r.bits)
		}
	}
	// Below that, no level to skip: an empty table, kept until the root
	// changes or doubles.
	t.routes.Store(r)
	return r
}

// sampleLeafSpan estimates a leaf's span: the median gap between
// neighboring separators of leaf parents under a few of the root's
// children. Its reads race writers; a heuristic needs no check.
func sampleLeafSpan(dir []*wordNode, root *wordNode, count int) uint64 {
	var gaps []uint64
	for j := 0; j < count && len(gaps) < 256; j += max(count/8, 1) {
		n := nodeIn(dir, root.inner().children[j&(nodeSize-1)])
		for depth := 0; n != nil && !n.leaf && depth < maxDepth; depth++ {
			child := n.inner().children[0]
			if child&wordLeafID != 0 {
				for i := 1; i+1 < min(int(n.n), nodeSize); i++ {
					if gap := n.keys[i+1] - n.keys[i]; n.keys[i+1] > n.keys[i] {
						gaps = append(gaps, gap)
					}
				}
				break
			}
			n = nodeIn(dir, child)
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	slices.Sort(gaps)
	return gaps[len(gaps)/2]
}
