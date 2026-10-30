package gobbtree

import (
	"testing"
	"unsafe"
)

// TestHintFillWalksRegions fills hint tables for trees of each key pattern,
// indexed and hashed, and checks them against tables filled region by
// region the general way: fillAll walks a leaf parent's regions and
// children together, and leaves only regions past its fences to fill.
func TestHintFillWalksRegions(t *testing.T) {
	for name, key := range map[string]func(i int) uint64{
		"mixed":      func(i int) uint64 { return mixedKey(uint64(i)) },
		"sequential": func(i int) uint64 { return uint64(i) << 1 },
		"clustered":  func(i int) uint64 { return mixedKey(uint64(i>>10))&^(1<<20-1) | uint64(i&1023)<<1 },
	} {
		tr := NewTree[uint64, uint64]()
		for i := 0; i < 100000; i++ {
			tr.Insert(key(i), 0)
		}
		g := tr.generic
		g.hints.Store(nil)
		root := (*innerRoute)(unsafe.Pointer(plainPointer(&g.root)))
		r := refreshCodeHints(&g.hints, &g.reshaped, &root.gheader)
		if r == nil {
			t.Fatalf("%s: no table", name)
		}
		want := &codeHints{hintTable: r.hintTable}
		want.cells = make([]hintCell, len(r.cells))
		var f hintFrame
		fillRegions(want, root, 0, 0, &f)
		filled := 0
		for i := range want.cells {
			if want.cells[i] != r.cells[i] {
				t.Fatalf("%s: cell %d is %x %x, want %x %x", name, i, r.cells[i].head, r.cells[i].tail, want.cells[i].head, want.cells[i].tail)
			}
			if want.cells[i].head > hintDescend {
				filled++
			}
		}
		if filled == 0 {
			t.Fatalf("%s: no cell names a leaf", name)
		}
	}
}

// fillRegions is fillAll, from the first key, as each region's fill alone
// fills it.
func fillRegions(r *codeHints, n *innerRoute, low, high uint64, f *hintFrame) {
	count := racingCount(&n.gheader)
	codes := routeCodes(n)
	if first := plainPointer(&n.children[0]); !first.leaf {
		for i := 0; i < count; i++ {
			childLow, childHigh := low, high
			if i > 0 {
				childLow = codes[i&(nodeSize-1)]
			}
			if i+1 < count {
				childHigh = codes[(i+1)&(nodeSize-1)]
			}
			f.push(n, i, low, high)
			fillRegions(r, innerChild(n, i), childLow, childHigh, f)
			f.depth--
		}
		return
	}
	last := high - 1
	if high == 0 {
		leaf := plainPointer(&n.children[(count-1)&(nodeSize-1)])
		last = (*leafRoute)(unsafe.Pointer(leaf)).codes[(racingCount(leaf)-1)&(nodeSize-1)]
	}
	if last < low {
		return
	}
	f.push(n, 0, low, high)
	defer func() { f.depth-- }()
	step := uint64(1) << r.shift
	start := r.base + ((max(low, r.base)-r.base)>>r.shift)<<r.shift
	for key, regions := start, 0; key <= last && regions <= 4*nodeSize; key, regions = key+step, regions+1 {
		if c := r.cell(max(key, low)); c != nil {
			r.fill(c, max(key, low), f, true)
		}
		if key+step < key {
			return
		}
	}
}
