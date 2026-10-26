package gobbtree

import "testing"

func TestSharedSeparatorRefit(t *testing.T) {
	for _, atomicWrites := range []bool{false, true} {
		tr := wordTree{shared: true, atomicWrites: atomicWrites}
		for _, count := range []int{8, 16, 24, 32} {
			for first := 1; first+1 < count-1; first++ {
				n := &wordNode{n: uint8(count)}
				want := &wordNode{n: uint8(count)}
				for i := 0; i < count; i++ {
					n.keys[i] = uint64(i) << 48
				}
				tr.refit(n)
				sequence, prefix := n.sequence.Load(), *n.prefixWord()
				n.keys[first] += 1 << 47
				n.keys[first+1] += 1 << 47
				want.keys = n.keys
				tr.refit(want)
				tr.refitSeparators(n, first, first+1)
				if n.tags != want.tags || n.inner().inline != want.inner().inline || n.shift != want.shift {
					t.Fatalf("atomic=%v count=%d first=%d: partial summaries differ from full refit", atomicWrites, count, first)
				}
				if n.sequence.Load() != sequence || *n.prefixWord() != prefix {
					t.Fatal("interior separator update changed count, shift, epoch, or prefix")
				}
			}
		}
	}
}
