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
// A table holds a cell for every half a leaf the tree has made, rounded up
// to a power of two, and doubles as the tree grows past it. It starts once
// the tree has customHintLeaves leaves: below that the descent is short.
type customHints struct {
	shift uint
	cells []atomic.Pointer[gheader]
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

// hintedLeaf is the leaf the hint table names for key, or nil.
func (t *genericTree[K, V, O]) hintedLeaf(key K, code uint64) *gheader {
	r := t.customHints.Load()
	if r == nil {
		return nil
	}
	return r.cell(t.hintCode(key, code)).Load()
}

// noteLeaf names leaf h, which a descent found for key, in key's cell,
// making the table first, or growing it, where the tree outgrew it.
func (t *genericTree[K, V, O]) noteLeaf(key K, code uint64, h *gheader) {
	r := t.customHints.Load()
	if leaves := t.customLeaves.Load(); r == nil || int64(len(r.cells)) < 4*leaves {
		if leaves < customHintLeaves {
			return
		}
		next := &customHints{cells: make([]atomic.Pointer[gheader], 1<<bits.Len64(uint64(4*leaves-1)))}
		next.shift = uint(64 - bits.Len64(uint64(len(next.cells)-1)))
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
