package gobbtree

import (
	"iter"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"testing"
)

// rangeSource exposes one container's ordered iterators over uint64 keys.
type rangeSource struct {
	name                  string
	rangeOf, rangeReverse func(lo, hi uint64) iter.Seq[uint64]
	ascend, descend       func(from uint64) iter.Seq[uint64]
	forward, backward     func() iter.Seq[uint64]
}

func keysOf[V any](seq iter.Seq2[uint64, V]) iter.Seq[uint64] {
	return func(yield func(uint64) bool) {
		for k := range seq {
			if !yield(k) {
				return
			}
		}
	}
}

type orderedSeqs[V any] interface {
	Range(lo, hi uint64) iter.Seq2[uint64, V]
	RangeReverse(lo, hi uint64) iter.Seq2[uint64, V]
	Ascend(from uint64) iter.Seq2[uint64, V]
	Descend(from uint64) iter.Seq2[uint64, V]
	All() iter.Seq2[uint64, V]
	Reverse() iter.Seq2[uint64, V]
}

func sourceOf[V any](name string, c orderedSeqs[V]) rangeSource {
	return rangeSource{
		name:         name,
		rangeOf:      func(lo, hi uint64) iter.Seq[uint64] { return keysOf(c.Range(lo, hi)) },
		rangeReverse: func(lo, hi uint64) iter.Seq[uint64] { return keysOf(c.RangeReverse(lo, hi)) },
		ascend:       func(from uint64) iter.Seq[uint64] { return keysOf(c.Ascend(from)) },
		descend:      func(from uint64) iter.Seq[uint64] { return keysOf(c.Descend(from)) },
		forward:      func() iter.Seq[uint64] { return keysOf(c.All()) },
		backward:     func() iter.Seq[uint64] { return keysOf(c.Reverse()) },
	}
}

func TestRangeIteratorsMatchReference(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 3))
	want := []uint64{0, 1, 2, math.MaxUint64 - 1, math.MaxUint64}
	for len(want) < 5000 {
		want = append(want, r.Uint64()>>r.IntN(48))
	}
	slices.Sort(want)
	want = slices.Compact(want)
	tree, mp, set := NewTree[uint64, uint64](), New[uint64, uint64](), NewSet[uint64]()
	generic := NewTree[nodeRoutedKey, uint64]()
	for _, k := range want {
		tree.Insert(k, k)
		mp.Insert(k, k)
		set.Insert(k)
		generic.Insert(nodeRoutedKey(k), k)
	}
	path := filepath.Join(t.TempDir(), "range.bbt")
	if err := tree.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	file, err := ReadFile[uint64, uint64](path)
	if err != nil {
		t.Fatal(err)
	}
	sources := []rangeSource{sourceOf[uint64]("tree", tree), sourceOf[uint64]("map", mp), sourceOf[uint64]("file", file)}
	sources = append(sources, rangeSource{
		name:         "set",
		rangeOf:      set.Range,
		rangeReverse: set.RangeReverse,
		ascend:       set.Ascend,
		descend:      set.Descend,
		forward:      set.All,
		backward:     set.Reverse,
	})
	sources = append(sources, rangeSource{
		name: "node tree",
		rangeOf: func(lo, hi uint64) iter.Seq[uint64] {
			return generic64(generic.Range(nodeRoutedKey(lo), nodeRoutedKey(hi)))
		},
		rangeReverse: func(lo, hi uint64) iter.Seq[uint64] {
			return generic64(generic.RangeReverse(nodeRoutedKey(lo), nodeRoutedKey(hi)))
		},
		ascend:   func(from uint64) iter.Seq[uint64] { return generic64(generic.Ascend(nodeRoutedKey(from))) },
		descend:  func(from uint64) iter.Seq[uint64] { return generic64(generic.Descend(nodeRoutedKey(from))) },
		forward:  func() iter.Seq[uint64] { return generic64(generic.All()) },
		backward: func() iter.Seq[uint64] { return generic64(generic.Reverse()) },
	})
	reversed := slices.Clone(want)
	slices.Reverse(reversed)
	bounds := []uint64{0, 1, 2, 3, math.MaxUint64 - 1, math.MaxUint64}
	for range 200 {
		bounds = append(bounds, want[r.IntN(len(want))]+uint64(r.IntN(3))-1)
	}
	for _, src := range sources {
		if got := slices.Collect(src.forward()); !slices.Equal(got, want) {
			t.Fatalf("%s: All", src.name)
		}
		if got := slices.Collect(src.backward()); !slices.Equal(got, reversed) {
			t.Fatalf("%s: Reverse", src.name)
		}
		for i, lo := range bounds {
			hi := bounds[(i*7+3)%len(bounds)]
			var inRange, atOrAbove, atOrBelow []uint64
			for _, k := range want {
				if k >= lo && k < hi {
					inRange = append(inRange, k)
				}
				if k >= lo {
					atOrAbove = append(atOrAbove, k)
				}
				if k <= lo {
					atOrBelow = append(atOrBelow, k)
				}
			}
			slices.Reverse(atOrBelow)
			if got := slices.Collect(src.rangeOf(lo, hi)); !slices.Equal(got, inRange) {
				t.Fatalf("%s: Range(%d, %d) = %d keys, want %d", src.name, lo, hi, len(got), len(inRange))
			}
			slices.Reverse(inRange)
			if got := slices.Collect(src.rangeReverse(lo, hi)); !slices.Equal(got, inRange) {
				t.Fatalf("%s: RangeReverse(%d, %d)", src.name, lo, hi)
			}
			if got := slices.Collect(src.ascend(lo)); !slices.Equal(got, atOrAbove) {
				t.Fatalf("%s: Ascend(%d) = %d keys, want %d", src.name, lo, len(got), len(atOrAbove))
			}
			if got := slices.Collect(src.descend(lo)); !slices.Equal(got, atOrBelow) {
				t.Fatalf("%s: Descend(%d) = %d keys, want %d", src.name, lo, len(got), len(atOrBelow))
			}
			// Stopping early yields a prefix.
			for k := range src.ascend(lo) {
				if k != atOrAbove[0] {
					t.Fatalf("%s: early stop", src.name)
				}
				break
			}
		}
	}
}

func generic64(seq iter.Seq2[nodeRoutedKey, uint64]) iter.Seq[uint64] {
	return func(yield func(uint64) bool) {
		for k := range seq {
			if !yield(uint64(k)) {
				return
			}
		}
	}
}

func TestTakeReturnsStoredKey(t *testing.T) {
	tr := NewTreeOf[foldKey, int]()
	m := NewMapOf[foldKey, int]()
	tr.Insert("Bob", 1)
	m.Insert("Bob", 1)
	if k, v, ok := tr.Take("BOB"); !ok || k != "Bob" || v != 1 || tr.Len() != 0 {
		t.Fatalf("tree take %q %d %v", k, v, ok)
	}
	if k, v, ok := m.Take("bob"); !ok || k != "Bob" || v != 1 || m.Len() != 0 {
		t.Fatalf("map take %q %d %v", k, v, ok)
	}
	if _, _, ok := m.Take("bob"); ok {
		t.Fatal("take of absent key")
	}
	n := New[uint64, uint64]()
	n.Insert(7, 70)
	if k, v, ok := n.Take(7); !ok || k != 7 || v != 70 || n.Len() != 0 {
		t.Fatalf("numeric take %d %d %v", k, v, ok)
	}
}

func TestTreeUpdateOrInsert(t *testing.T) {
	tr := NewTree[string, int]()
	add := func(old, value int) int { return old + value }
	if _, found := tr.UpdateOrInsert("a", 1, add); found {
		t.Fatal("found absent key")
	}
	if old, found := tr.UpdateOrInsert("a", 2, add); !found || old != 1 {
		t.Fatalf("old %d %v", old, found)
	}
	if v, _ := tr.Get("a"); v != 3 {
		t.Fatalf("merged %d", v)
	}
}
