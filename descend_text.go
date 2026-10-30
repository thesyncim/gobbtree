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
	tags  [nodeSize]uint8
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
	return p <= 16 && len(s) <= p+16 && len(s) >= p && headDiff(stringCode(s, 0), headWindow(s), p, h0, h1) == 0
}

// seekCode finds code in leaf n, of count entries: its slot, by its tag
// where it has one, and whether it is there. Where it isn't and place is
// true, the slot it would take; where no tag is its and place is false,
// count, without reading the codes.
func seekCode(n *leafRoute, count int, code uint64, place bool) (int, bool) {
	i := firstTag(&n.tags, count, code)
	if i < count && n.codes[i&(nodeSize-1)] == code {
		return i, true
	}
	if i == count && !place {
		return count, false
	}
	lo := lowerCode(&n.codes, count, code)
	return lo, lo < count && n.codes[lo&(nodeSize-1)] == code
}

// descendCodeBy is descendCode by calls to the search kernels. An internal
// node's codes are its separators', the first 0: the route is the last
// child whose separator is at most code.
func descendCodeBy(h *gheader, code uint64, place bool) (*gheader, int, bool) {
	for !h.leaf {
		n := (*innerRoute)(unsafe.Pointer(h))
		count := owned(&n.count)
		codes := (*[nodeSize]uint64)(unsafe.Pointer(&n.codes))
		i := lowerCode(codes, count, code)
		if i < count && codes[i&(nodeSize-1)] == code {
			i++
		}
		h = plainPointer(&n.children[(i-1)&(nodeSize-1)])
	}
	i, ok := seekCode((*leafRoute)(unsafe.Pointer(h)), owned(&h.count), code, place)
	return h, i, ok
}

// What descendTextOrdered finds in its leaf: key's code at the slot, or no
// code of key's there or key lacking the leaf's prefix, so key absent.
const (
	orderedCode = iota
	orderedAbsent
)

// descendTextOrderedBy is descendTextOrdered by calls to the search
// kernels.
func descendTextOrderedBy(root **gheader, s string) (h *gheader, word uint64, lo, state int, acc uint64, ok bool) {
	h = *root
	if h == nil {
		return nil, 0, 0, 0, 0, false
	}
	word = h.latch.Load()
	if *after(root, word) != h {
		return nil, 0, 0, 0, 0, false
	}
	for depth := 0; ; depth++ {
		if word&latchExclusive != 0 || depth == maxDepth {
			return nil, 0, 0, 0, 0, false
		}
		if h.leaf {
			break
		}
		n := (*innerRoute)(unsafe.Pointer(h))
		count := int(*(*int32)(unsafe.Pointer(&n.count)))
		if count < 1 || count > nodeSize {
			return nil, 0, 0, 0, 0, false
		}
		p := int(*(*int32)(unsafe.Pointer(&n.prefix)))
		sum := summaryOf(s, p)
		sums := (*[nodeSize]uint32)(unsafe.Pointer(&n.codes))
		i := max(lowerSummary(sums, count, sum)-1, 0)
		below, next := sums[i&(nodeSize-1)], sums[(i+1)&(nodeSize-1)]
		if i > 0 && below >= sum || i+1 < count && next <= sum {
			return nil, 0, 0, 0, 0, false
		}
		a := uint64(i) ^ uint64(count) ^ uint64(p)<<8 ^ uint64(below)<<16 ^ uint64(next)<<32
		child := *after((**gheader)(unsafe.Pointer(&n.children[i&(nodeSize-1)])), a)
		if child == nil {
			return nil, 0, 0, 0, 0, false
		}
		childWord := child.latch.Load()
		if *after((*uint64)(unsafe.Pointer(&h.latch)), a^childWord^uint64(uintptr(unsafe.Pointer(child))))&^latchSharedMask != word&^latchSharedMask {
			return nil, 0, 0, 0, 0, false
		}
		h, word = child, childWord
	}
	n := (*leafRoute)(unsafe.Pointer(h))
	count := int(*(*int32)(unsafe.Pointer(&n.count)))
	p := int(*(*int32)(unsafe.Pointer(&n.prefix)))
	if count < 0 || count > nodeSize || p > 16 {
		return nil, 0, 0, 0, 0, false
	}
	h0, h1 := *(*uint64)(unsafe.Pointer(&n.head[0])), *(*uint64)(unsafe.Pointer(&n.head[1]))
	acc = uint64(count) ^ uint64(p)<<8 ^ h0 ^ h1<<1
	if len(s) < p || headDiff(stringCode(s, 0), headWindow(s), p, h0, h1) != 0 {
		return h, word, 0, orderedAbsent, acc, true
	}
	if len(s) > p+16 {
		return nil, 0, 0, 0, 0, false
	}
	code := stringCode(s, p)
	lo = lowerCode(&n.codes, count, code)
	c := n.codes[lo&(nodeSize-1)]
	acc ^= uint64(lo)<<40 ^ c
	if lo < count && c == code {
		return h, word, lo, orderedCode, acc, true
	}
	return h, word, lo, orderedAbsent, acc, true
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
