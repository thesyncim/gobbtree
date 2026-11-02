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
// the tree grows past it, its cells starting with the old cells' leaves. It
// starts once the tree has customHintLeaves leaves: below that the descent
// is short.
//
// A table judges the one before it as it grows: where the old cells named
// fewer than a quarter of the tree's leaves then, its keys' codes spread
// too little past the shared prefix for their top bits to tell the leaves
// apart, and nearly every hint named a leaf that didn't settle its key, at
// the cost of a check before the descent. Text is so, its bytes taking few
// values: the ten digits of a number, the sixteen of hex. A table after one
// of codes' own bits whose misses' codes held few bytes, at most half of
// all, indexes its cells by the codes' bytes ranked among those, as the
// digits of a number in the base of their count, which keeps the codes'
// order and spreads them over every cell; one after a poor table is off, unread and unfilled, as is
// every table after it until the tree is cleared, as a tenant's number
// followed by a constant name is: a lookup then costs what it did without
// hints.
type customHints struct {
	// A code's cell is its top bits, shifted down by shift, masked by mask:
	// len(cells)-1.
	shift uint
	mask  uint64
	cells []atomic.Pointer[gheader]
	// rank, where set, ranks a byte among the bytes keys used, and weight
	// is a rank's weight at each of a code's bytes, 2^64 over the count of
	// those bytes to the power of its place, from 1: the ranks so weighted
	// sum to the code's place among codes of those bytes, as a 64-bit
	// fraction. nil indexes by the codes' own bits.
	rank   *[256]uint8
	weight [8]uint64
	// seen are the bytes the codes of keys that missed the table held, the
	// first four of each, for the ranks of the table after it, and misses
	// how many missed, up to customHintSamples: it judges no table on fewer,
	// as a few random keys' codes hold few bytes too.
	seen   [4]atomic.Uint64
	misses atomic.Int64
	// leaves is the tree's when the table was made.
	leaves int64
	off    bool
}

const (
	customHintLeaves  = 64
	customHintSamples = 256
)

// cell is hint code hc's cell: its top bits', or its ranked place's.
func (r *customHints) cell(hc uint64) *atomic.Pointer[gheader] {
	if r.rank != nil {
		return r.rankedCell(hc)
	}
	return &r.cells[(hc>>r.shift)&r.mask]
}

// rankedCell is cell for a table that ranks codes' bytes, out of line, so
// that cell inlines into its callers.
//
//go:noinline
func (r *customHints) rankedCell(hc uint64) *atomic.Pointer[gheader] {
	return &r.cells[(r.ranked(hc)>>r.shift)&r.mask]
}

// ranked is hc's place among codes of the ranked bytes, its bytes the
// digits of a 64-bit fraction in their base: in order as hc is. A place
// past the fraction's precision weighs 0. The places sum in pairs, then
// pairs of pairs, so that the multiplies don't wait on each other: the
// index lies on a lookup's path to its leaf.
func (r *customHints) ranked(hc uint64) uint64 {
	rank, w := r.rank, &r.weight
	a := uint64(rank[byte(hc>>56)])*w[0] + uint64(rank[byte(hc>>48)])*w[1]
	b := uint64(rank[byte(hc>>40)])*w[2] + uint64(rank[byte(hc>>32)])*w[3]
	c := uint64(rank[byte(hc>>24)])*w[4] + uint64(rank[byte(hc>>16)])*w[5]
	d := uint64(rank[byte(hc>>8)])*w[6] + uint64(rank[byte(hc)])*w[7]
	return (a + b) + (c + d)
}

// ranksOf ranks the bytes seen, each to how many seen bytes lie below it,
// one not seen to the rank of the last below it, and weighs a rank at each
// place of a code: false where the bytes are more than half of all, which
// a code's own bits spread near as well.
func ranksOf(seen *[4]atomic.Uint64) (*[256]uint8, [8]uint64, bool) {
	var rank [256]uint8
	n := -1
	for b := range 256 {
		if seen[b>>6].Load()&(1<<(b&63)) != 0 {
			n++
		}
		rank[b] = uint8(max(n, 0))
	}
	var weight [8]uint64
	base := uint64(n + 1)
	if base < 2 || base > 128 {
		return nil, weight, false
	}
	// 2^64 / base^(i+1), dividing what is left of 2^64 a place at a time.
	hi, lo := uint64(1), uint64(0)
	for i := range weight {
		q, rem := bits.Div64(hi, lo, base)
		if q == 0 {
			break
		}
		weight[i], hi, lo = q, 0, q
		_ = rem
	}
	return &rank, weight, true
}

// hintCode is custom key key's code at the shared prefix, given code, its
// window at 0: the hint table's index is its top bits.
func (t *genericTree[K, V, O]) hintCode(key K, code uint64) uint64 {
	s := t.shared.Load()
	if s == nil || len(s.text) == 0 {
		return code
	}
	return t.codeOf(key, code, len(s.text))
}

// hintedLeaf is the leaf hint table r names for key, or nil, given the
// shared prefix's length, which the caller read.
func (t *genericTree[K, V, O]) hintedLeaf(r *customHints, key K, code uint64, shared int) *gheader {
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
func (t *genericTree[K, V, O]) sharedLen(key K, w0 uint64) (int, bool) {
	s := t.shared.Load()
	if s == nil || len(s.text) == 0 {
		return 0, true
	}
	// A prefix of at most 8 bytes lies in key's first window.
	if n := len(s.text); n <= 8 {
		return n, w0&headMask(n) == s.head
	}
	return len(s.text), t.sharedOrderOf(key) == 0
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
		next.mask = uint64(len(next.cells) - 1)
		// The old cells are in key order, so a leaf they name is a run of
		// them: the runs count the leaves named.
		if r != nil {
			named := 0
			var last *gheader
			for j := range r.cells {
				if h := r.cells[j].Load(); h != nil && h != last {
					named, last = named+1, h
				}
			}
			// Codes of few bytes, as text's, rank them, as soon as a table of
			// their own bits has seen them: ranks spread them better however
			// that table did.
			rank, weight, few := ranksOf(&r.seen)
			sampled := r.misses.Load() >= customHintSamples
			switch poor := int64(4*named) < r.leaves; {
			case r.rank == nil && few && sampled:
				next.rank, next.weight = rank, weight
			case poor && sampled:
				next.off = true
			default:
				// Each new cell is a slice of an old one, and starts with its
				// leaf: a grown table hints as well as the old did.
				next.rank, next.weight = r.rank, r.weight
				for j := range next.cells {
					next.cells[j].Store(r.cells[j>>(r.shift-next.shift)].Load())
				}
			}
		}
		if !t.customHints.CompareAndSwap(r, next) {
			next = t.customHints.Load()
		}
		r = next
	}
	hc := t.hintCode(key, code)
	if c := r.cell(hc); c.Load() != h {
		c.Store(h)
	}
	// The bytes of a code that missed, for the next table's ranks, until it
	// has the sample it judges by: past it, a miss writes no line that every
	// writer's misses would.
	if r.misses.Load() >= customHintSamples {
		return
	}
	r.misses.Add(1)
	var seen [4]uint64
	for i := range 4 {
		b := byte(hc >> (56 - 8*i))
		seen[b>>6] |= 1 << (b & 63)
	}
	for i, m := range seen {
		if m != 0 && r.seen[i].Load()&m != m {
			r.seen[i].Or(m)
		}
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
