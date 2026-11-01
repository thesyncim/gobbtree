package gobbtree

import (
	"math/rand/v2"
	"testing"
	"unsafe"
)

// TestDescendCodesRoutesAsBrackets checks descendCodes's one-pass route
// against the rule it replaced: the last child whose separator is below the
// key's code, read from the separators that bracket it, a tie where the one
// after the route equals the code. Nodes hold sorted separators, ties and
// all, codePad past their count, in both layouts, 4-byte summaries and
// 8-byte codes, and the codes probed are each separator's, its neighbors,
// and the extremes.
func TestDescendCodesRoutesAsBrackets(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	children := make([]gheader, nodeSize)
	for i := range children {
		children[i].leaf = true
	}
	for trial := range 4000 {
		narrow := trial%2 == 0
		count := 1 + r.IntN(nodeSize)
		n := &innerRoute{}
		n.count.Store(int32(count))
		for i := range nodeSize {
			n.codes[i].Store(codePad)
			n.children[i].Store(&children[i])
		}
		// Few distinct values, so that separators tie.
		span := uint64(1 + r.IntN(3*count))
		var seps []uint64
		for range count {
			seps = append(seps, r.Uint64N(span))
		}
		for i := 1; i < count; i++ {
			for j := i; j > 0 && seps[j] < seps[j-1]; j-- {
				seps[j], seps[j-1] = seps[j-1], seps[j]
			}
		}
		sums := (*[nodeSize]uint32)(unsafe.Pointer(&n.codes))
		codes := (*[nodeSize]uint64)(unsafe.Pointer(&n.codes))
		for i, v := range seps {
			if narrow {
				sums[i] = uint32(v)
			} else {
				codes[i] = v
			}
		}
		probes := []uint64{0, ^uint64(0) >> 32, ^uint64(0)}
		for _, v := range seps {
			probes = append(probes, v, v+1)
			if v > 0 {
				probes = append(probes, v-1)
			}
		}
		for _, probe := range probes {
			// The rule before: brackets read after the count.
			var i int
			var tie bool
			if narrow {
				s := uint32(probe)
				if probe > uint64(^uint32(0)) {
					continue
				}
				i = max(lowerSummaryScalar(sums, count, s)-1, 0)
				tie = i > 0 && sums[i] >= s || i+1 < count && sums[i+1] <= s
			} else {
				i = max(lowerCodeScalar(codes, count, probe)-1, 0)
				tie = i > 0 && codes[i] >= probe || i+1 < count && codes[i+1] <= probe
			}
			// A 4-byte summary is the top of a code 4 bytes wide.
			c := probe
			if narrow {
				c = probe << 32
			}
			got, _, _, reached := descendCodes(&n.gheader, 0, 0, c, 0, narrow, 4)
			if tie {
				if reached != codesRaced {
					t.Fatalf("narrow %v, separators %v, code %d: routed to %v, want a tie", narrow, seps, probe, got)
				}
				continue
			}
			if reached != codesAtLeaf || got != &children[i] {
				t.Fatalf("narrow %v, separators %v, code %d: reached %d at %p, want child %d at %p", narrow, seps, probe, reached, got, i, &children[i])
			}
		}
	}
}

// TestLeafCodeRangeIsTieRange checks leafCodeRange's one pass against
// tieRange, over sorted codes with runs of ties and codePad past the count.
func TestLeafCodeRangeIsTieRange(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	for range 4000 {
		count := r.IntN(nodeSize + 1)
		n := &leafRoute{}
		for i := range n.codes {
			n.codes[i] = codePad
		}
		span := uint64(1 + r.IntN(2*count+1))
		for i := range count {
			n.codes[i] = r.Uint64N(span)
		}
		for i := 1; i < count; i++ {
			for j := i; j > 0 && n.codes[j] < n.codes[j-1]; j-- {
				n.codes[j], n.codes[j-1] = n.codes[j-1], n.codes[j]
			}
		}
		for _, c := range []uint64{0, span, codePad, r.Uint64N(span)} {
			lo, hi := leafCodeRange(&n.gheader, count, c)
			wantLo, wantHi := tieRange(&n.codes, count, c)
			if lo != wantLo || hi != wantHi {
				t.Fatalf("codes %v, code %d: [%d, %d), want [%d, %d)", n.codes[:count], c, lo, hi, wantLo, wantHi)
			}
		}
	}
}
