package gobbtree

import (
	"sync/atomic"
	"unsafe"
)

// innerRoute is what a text descent reads of an internal node, laid out as
// every ginner begins whatever its key type: a descent that reads only it
// needs no type parameters, and so can live beside the search kernels.
type innerRoute struct {
	gheader
	codes    [nodeSize]atomic.Uint64
	children [nodeSize]atomic.Pointer[gheader]
}

// leafRoute is what a text lookup reads of a leaf before its key's slot,
// laid out as every gleaf begins.
type leafRoute struct {
	gheader
	codes [nodeSize]uint64
}

// What descendText finds: where text key s's summary ties a separator's,
// the internal node, for the separators to settle; otherwise s's leaf,
// where s doesn't fit the leaf's windows or lacks its head, to search; or
// where s fits, the slot of the first code at least s's, holding s's code
// or not, with s absent.
const (
	textTie = iota
	textSearch
	textCode
	textAbsent
)

// textFits reports whether text s, in leaf n of count keys at prefix p, is
// settled by its windows: it begins with the leaf's head and ends within
// them. Its loads are plain.
func textFits(n *leafRoute, s string, p int) bool {
	h0, h1 := *(*uint64)(unsafe.Pointer(&n.head[0])), *(*uint64)(unsafe.Pointer(&n.head[1]))
	return p <= 16 && len(s) <= p+16 && len(s) < 255 && len(s) >= p && headDiff(stringCode(s, 0), headWindow(s), p, h0, h1) == 0
}

// descendTextBy is descendText by calls to the search kernels.
func descendTextBy(h *gheader, s string) (*gheader, int, int) {
	for !h.leaf {
		n := (*innerRoute)(unsafe.Pointer(h))
		count := owned(&n.count)
		sum := summaryOf(s, owned(&n.prefix))
		sums := (*[nodeSize]uint32)(unsafe.Pointer(&n.codes))
		lo := lowerSummary(sums, count, sum)
		if lo < count && sums[lo&(nodeSize-1)] == sum {
			return h, 0, textTie
		}
		h = plainPointer(&n.children[max(lo-1, 0)&(nodeSize-1)])
	}
	n := (*leafRoute)(unsafe.Pointer(h))
	count, p := owned(&n.count), owned(&n.prefix)
	if !textFits(n, s, p) {
		return h, 0, textSearch
	}
	code := stringCode(s, p)
	lo := lowerCode(&n.codes, count, code)
	if lo < count && n.codes[lo&(nodeSize-1)] == code {
		return h, lo, textCode
	}
	return h, lo, textAbsent
}
