package gobbtree

import "math/bits"

// A Tree's leaf hints use the hint cells the Map's latched tree uses
// (word_latched_routes.go): per region of the key space, the leaves that
// hold it, up to four, and where each starts. A Tree has one owner, so a
// hint is checked by its leaf alone: a key the leaf holds is there, and a
// key between its first and last keys is its own, as separators bound
// leaves' keys from below. A freed leaf holds nothing. Any other key
// descends, and the descent fills the key's cell from the leaf's parent.
type wordHints struct {
	table *latchedRoutes
	// built is the entry count the table was made for; it is made again,
	// finer, when the tree doubles.
	built int
}

// hintedLeaf is the leaf key's hint names, or nil.
func (t *wordTree) hintedLeaf(key uint64) *wordNode {
	r := t.hints.table
	if r == nil {
		return nil
	}
	c := r.cell(key)
	if c == nil {
		return nil
	}
	id := r.leafFor(c, key)
	if id == 0 {
		return nil
	}
	return t.node(id)
}

// hintParent fills key's cell from p, the parent of key's leaf, which the
// descent reached from g at edge, or from the root (g nil).
func (t *wordTree) hintParent(key uint64, p, g *wordNode, edge int) {
	if r := t.hints.table; r != nil {
		if c := r.cell(key); c != nil {
			r.fill(t.slabs, c, key, p, g, edge)
		}
	}
}

// refreshHints makes the hint table for a tree of length entries when it
// has none or has doubled since, as the latched tree's refreshRoutes sizes
// it. A tree without levels above its leaf parents keeps none.
func (t *wordTree) refreshHints(length int) {
	h := &t.hints
	if length < 2*h.built && h.table != nil {
		return
	}
	h.built = length
	root := t.rootNode
	if root == nil || root.leaf || root.level < 2 || root.n < 2 {
		h.table = nil
		return
	}
	span := sampleLeafSpan(t.slabs, root, int(root.n))
	if span == 0 {
		h.table = nil
		return
	}
	h.table = newHintTable(root.keys[1], root.keys[root.n-1], span, length/(nodeSize*3/4))
}

// dropHints forgets every hint, for a tree that let go of its arena.
func (t *wordTree) dropHints() { t.hints = wordHints{} }

// newHintTable sizes a hint table for about leaves leaves of span span, over
// a domain from lo to hi, as refreshRoutes does.
func newHintTable(lo, hi, span uint64, leaves int) *latchedRoutes {
	tableBits := min(max(bits.Len(uint(leaves)), 8), 22)
	shift := max(bits.Len64(span)-1, 0)
	width := bits.Len64(lo ^ hi)
	r := &latchedRoutes{shift: uint8(min(shift, 63))}
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
