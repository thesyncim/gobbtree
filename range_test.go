package gobbtree

import (
	"fmt"
	"iter"
	"maps"
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

func keysOf[K, V any](seq iter.Seq2[K, V]) iter.Seq[K] {
	return func(yield func(K) bool) {
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
			if lo > hi {
				lo, hi = hi, lo
			}
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

// parityOps is the common surface of Tree and Map for the parity helpers.
type parityOps[K comparable] interface {
	Get(K) (int, bool)
	Len() int
	InsertAll(iter.Seq2[K, int])
	UpdateRange(b Bounds[K], change func(K, int) int)
	UpdateAll(change func(K, int) int)
	LoadOrStoreFunc(K, func() int) (int, bool)
}

func checkParityOps[K comparable](t *testing.T, c parityOps[K], key func(int) K) {
	t.Helper()
	c.InsertAll(func(yield func(K, int) bool) {
		for i := range 1000 {
			if !yield(key(i), i) {
				return
			}
		}
	})
	if c.Len() != 1000 {
		t.Fatalf("InsertAll len %d", c.Len())
	}
	c.UpdateRange(Between(key(100), key(200)), func(_ K, v int) int { return v + 1000 })
	c.UpdateAll(func(_ K, v int) int { return v * 2 })
	for i := range 1000 {
		want := 2 * i
		if i >= 100 && i < 200 {
			want = 2 * (i + 1000)
		}
		if v, _ := c.Get(key(i)); v != want {
			t.Fatalf("key %d = %d, want %d", i, v, want)
		}
	}
	calls := 0
	make := func() int { calls++; return -1 }
	if v, loaded := c.LoadOrStoreFunc(key(5), make); !loaded || v != 10 || calls != 0 {
		t.Fatalf("present LoadOrStoreFunc %d %v calls %d", v, loaded, calls)
	}
	if v, loaded := c.LoadOrStoreFunc(key(5000), make); loaded || v != -1 || calls != 1 {
		t.Fatalf("absent LoadOrStoreFunc %d %v", v, loaded)
	}
}

func TestParityOperations(t *testing.T) {
	str := func(i int) string { return fmt.Sprintf("k%06d", i) }
	checkParityOps[uint64](t, NewTree[uint64, int](), func(i int) uint64 { return uint64(i) })
	checkParityOps[uint64](t, New[uint64, int](), func(i int) uint64 { return uint64(i) })
	checkParityOps[string](t, NewTree[string, int](), str)
	checkParityOps[string](t, New[string, int](), str)

	m := New[string, int]()
	m.InsertAll(maps.All(map[string]int{"a": 1, "b": 2}))
	found := map[int]int{}
	m.ReadMany([]string{"b", "z", "a"}, func(i int, _ string, v int) { found[i] = v })
	if len(found) != 2 || found[0] != 2 || found[2] != 1 {
		t.Fatalf("ReadMany %v", found)
	}
	dst := make([]Result[int], 3)
	m.UpdateOrInsertMany([]Entry[string, int]{{"a", 1}, {"c", 1}, {"a", 1}}, func(old, v int) int { return old + v }, dst)
	if dst[0] != (Result[int]{1, true}) || dst[1].OK || dst[2] != (Result[int]{2, true}) {
		t.Fatalf("UpdateOrInsertMany %v", dst)
	}
	s := NewSet[int]()
	s.InsertAll(slices.Values([]int{3, 1, 2, 1}))
	if got := slices.Collect(s.All()); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("set InsertAll %v", got)
	}
}

func TestMapFirstIn(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 5))
	var m Map[uint64, uint64]
	var keys []uint64
	for range 3000 {
		k := r.Uint64() >> r.IntN(40)
		m.Insert(k, ^k)
		keys = append(keys, k)
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	m.Get(keys[0]) // activate optimistic reads
	for range 2000 {
		lo, hi := r.Uint64()>>r.IntN(40), r.Uint64()>>r.IntN(40)
		if r.IntN(4) == 0 {
			lo = keys[r.IntN(len(keys))]
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		k, v, ok := m.FirstIn(Between(lo, hi))
		i, _ := slices.BinarySearch(keys, lo)
		if want := i < len(keys) && keys[i] < hi; ok != want || ok && (k != keys[i] || v != ^k) {
			t.Fatalf("FirstIn(%d, %d) = %d, %d, %v", lo, hi, k, v, ok)
		}
	}
	if _, _, ok := (&Map[uint64, uint64]{}).FirstIn(Between[uint64](0, math.MaxUint64)); ok {
		t.Fatal("empty map has a first entry")
	}
}
