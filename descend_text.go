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

// descendTextBy is descendText by calls to lowerSummary.
func descendTextBy(h *gheader, s string) (leaf, tie *gheader) {
	for !h.leaf {
		n := (*innerRoute)(unsafe.Pointer(h))
		count := owned(&n.count)
		sum := summaryOf(s, owned(&n.prefix))
		sums := (*[nodeSize]uint32)(unsafe.Pointer(&n.codes))
		lo := lowerSummary(sums, count, sum)
		if lo < count && sums[lo&(nodeSize-1)] == sum {
			return nil, h
		}
		h = plainPointer(&n.children[max(lo-1, 0)&(nodeSize-1)])
	}
	return h, nil
}
