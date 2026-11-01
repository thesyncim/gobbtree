package gobbtree

import (
	"math/bits"
	"sync/atomic"
	"unsafe"
)

// Hints for custom keys, as the integer trees keep theirs (generic_hints.go),
// beyond upstream: a table of leaves by the top bits of their keys' codes at
// the shared prefix, which order the tree's keys by their first bytes past
// what all of them share, so that a lookup or a write goes from its key
// straight to its leaf, without the internal nodes. A hint is never trusted.
// The leaf it names settles a key only where, read at a version that holds,
// the key has the leaf's prefix and its code lies strictly between the
// codes of the leaf's first key and its last: every key between two of a
// leaf's keys is the leaf's. Anything else descends, and the descent names
// the leaf it found in the key's cell. A leaf merged away holds nothing
// (freed), so a stale cell costs only a descent.
//
// A table holds four to eight cells a leaf, a power of two, and doubles as
// the tree grows past it, its cells starting with the old cells' leaves. It starts once
// the tree has customHintLeaves leaves: below that the descent is short.
type customHints struct {
	shift uint
	cells []atomic.Pointer[gheader]
	// leaves is the tree's when the table was made, and off whether its
	// hints go unread: where the old table's cells named fewer than a
	// quarter of the tree's leaves then, its keys' codes spread too little
	// past the shared prefix for the top bits to tell their leaves apart,
	// as a tenant's number followed by a constant name, or text, whose bytes
	// spread over few values: nearly every hint would name a leaf that
	// doesn't settle its key, and cost a check before the descent. A table
	// off stays so, unread and unfilled, as does every table after it, until
	// the tree is cleared: a lookup then costs what it did without hints.
	leaves int64
	off    bool
}

const customHintLeaves = 64

// cell is hint code hc's cell.
func (r *customHints) cell(hc uint64) *atomic.Pointer[gheader] {
	return &r.cells[(hc>>r.shift)&uint64(len(r.cells)-1)]
}

// hintCode is custom key key's code at the shared prefix, given code, its
// window at 0: the hint table's index is its top bits.
func (t *genericTree[K, V, O]) hintCode(key K, code uint64) uint64 {
	s := t.shared.Load()
	if s == nil || len(*s) == 0 {
		return code
	}
	return t.codeOf(key, code, len(*s))
}

// hintedLeaf is the leaf the hint table names for key, or nil, given the
// shared prefix's length, which the caller read.
func (t *genericTree[K, V, O]) hintedLeaf(key K, code uint64, shared int) *gheader {
	r := t.customHints.Load()
	if r == nil || r.off {
		return nil
	}
	if shared > 0 {
		code = t.codeOf(key, code, shared)
	}
	return r.cell(code).Load()
}

// sharedLen is the length of what every key of the tree shares, and false
// where key doesn't share it, and so is in no leaf of a lookup's: a custom
// key's check of it, once, for its descent and its hint.
func (t *genericTree[K, V, O]) sharedLen(key K) (int, bool) {
	s := t.shared.Load()
	if s != nil && len(*s) == 0 {
		return 0, true
	}
	if t.sharedOrderOf(key) != 0 {
		return 0, false
	}
	if s == nil {
		return 0, true
	}
	return len(*s), true
}

// noteLeaf names leaf h, which a descent found for key, in key's cell,
// making the table first, or growing it, where the tree outgrew it.
func (t *genericTree[K, V, O]) noteLeaf(key K, code uint64, h *gheader) {
	r := t.customHints.Load()
	if r != nil && r.off {
		return
	}
	if leaves := t.customLeaves.Load(); r == nil || int64(len(r.cells)) < 4*leaves {
		if leaves < customHintLeaves {
			return
		}
		next := &customHints{cells: make([]atomic.Pointer[gheader], 1<<bits.Len64(uint64(4*leaves-1))), leaves: leaves}
		next.shift = uint(64 - bits.Len64(uint64(len(next.cells)-1)))
		// Each new cell is a slice of an old one, and starts with its
		// leaf: a grown table hints as well as the old did. The old cells
		// are in key order, so a leaf they name is a run of them: the runs
		// count the leaves named.
		if r != nil {
			named := 0
			var last *gheader
			for j := range r.cells {
				if h := r.cells[j].Load(); h != nil && h != last {
					named, last = named+1, h
				}
			}
			next.off = int64(4*named) < r.leaves
			for j := range next.cells {
				next.cells[j].Store(r.cells[j>>(r.shift-next.shift)].Load())
			}
		}
		if !t.customHints.CompareAndSwap(r, next) {
			next = t.customHints.Load()
		}
		r = next
	}
	if c := r.cell(t.hintCode(key, code)); c.Load() != h {
		c.Store(h)
	}
}

// hasLeafPrefix reports whether custom key key, w0 its window at 0, begins
// with leaf n's prefix of p bytes, read racing, for a leaf a hint named:
// its first 16 bytes from the leaf's header, the rest from its head copy's
// windows, which no writer changes. A leaf without a head copy, the
// leftmost, is no hinted leaf past 16 bytes.
func (t *genericTree[K, V, O]) hasLeafPrefix(n *gleaf[K, V], key K, w0 uint64, p int) bool {
	if p <= 8 {
		return hasShortPrefix(&n.gheader, w0, p)
	}
	for off := 0; off < p; off += 8 {
		m := headMask(p - off)
		var h uint64
		if off < 16 {
			h = *(*uint64)(unsafe.Pointer(&n.head[(off>>3)&1]))
		} else if c := headOf(n); c != nil {
			h = c.win[(off>>3)&(headWindows-1)] & m
		} else {
			return false
		}
		w := w0
		if off > 0 {
			w = t.window(key, off)
		}
		if w&m != h {
			return false
		}
	}
	return true
}

// hasShortPrefix is hasLeafPrefix for a prefix of at most 8 bytes, which
// the header's first word holds: the callers' own check, inlined.
func hasShortPrefix(h *gheader, w0 uint64, p int) bool {
	return p == 0 || w0&headMask(p) == *(*uint64)(unsafe.Pointer(&h.head[0]))
}
