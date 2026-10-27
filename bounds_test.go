package gobbtree

import (
	"fmt"
	"iter"
	"maps"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// boundsSource scans one container over uint64 keys, by way of whatever key
// type it stores.
type boundsSource struct {
	name              string
	scan, scanReverse func(Bounds[uint64]) iter.Seq[uint64]
}

func boundsOf[K any](b Bounds[uint64], to func(uint64) K) Bounds[K] {
	return Bounds[K]{lower: to(b.lower), upper: to(b.upper), lowerKind: b.lowerKind, upperKind: b.upperKind}
}

func scanSource[K, V any](name string, to func(uint64) K, from func(K) uint64, scan, scanReverse func(Bounds[K]) iter.Seq2[K, V]) boundsSource {
	keys := func(seq iter.Seq2[K, V]) iter.Seq[uint64] {
		return func(yield func(uint64) bool) {
			for k := range seq {
				if !yield(from(k)) {
					return
				}
			}
		}
	}
	return boundsSource{
		name:        name,
		scan:        func(b Bounds[uint64]) iter.Seq[uint64] { return keys(scan(boundsOf(b, to))) },
		scanReverse: func(b Bounds[uint64]) iter.Seq[uint64] { return keys(scanReverse(boundsOf(b, to))) },
	}
}

func hexKey(k uint64) string { return fmt.Sprintf("%016x", k) }
func unhexKey(s string) uint64 {
	var k uint64
	fmt.Sscanf(s, "%x", &k)
	return k
}

func TestScanBoundsMatchReference(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 5))
	want := []uint64{0, 1, 2, math.MaxUint64 - 1, math.MaxUint64}
	for len(want) < 3000 {
		want = append(want, r.Uint64()>>r.IntN(48))
	}
	slices.Sort(want)
	want = slices.Compact(want)
	tree, mp, set := NewTree[uint64, uint64](), New[uint64, uint64](), NewSet[uint64]()
	nodes := NewTree[nodeRoutedKey, uint64]()
	strTree, strMap := NewTree[string, uint64](), New[string, uint64]()
	for _, k := range want {
		tree.Insert(k, k)
		mp.Insert(k, k)
		set.Insert(k)
		nodes.Insert(nodeRoutedKey(k), k)
		strTree.Insert(hexKey(k), k)
		strMap.Insert(hexKey(k), k)
	}
	path := filepath.Join(t.TempDir(), "bounds.bbt")
	if err := tree.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	file, err := ReadFile[uint64, uint64](path)
	if err != nil {
		t.Fatal(err)
	}
	same := func(k uint64) uint64 { return k }
	sources := []boundsSource{
		scanSource("tree", same, same, tree.Scan, tree.ScanReverse),
		scanSource("map", same, same, mp.Scan, mp.ScanReverse),
		scanSource("file", same, same, file.Scan, file.ScanReverse),
		scanSource("node tree", func(k uint64) nodeRoutedKey { return nodeRoutedKey(k) }, func(k nodeRoutedKey) uint64 { return uint64(k) }, nodes.Scan, nodes.ScanReverse),
		scanSource("string tree", hexKey, unhexKey, strTree.Scan, strTree.ScanReverse),
		scanSource("string map", hexKey, unhexKey, strMap.Scan, strMap.ScanReverse),
		{name: "set", scan: set.Scan, scanReverse: set.ScanReverse},
	}
	points := []uint64{0, 1, 2, 3, math.MaxUint64 - 1, math.MaxUint64}
	for range 60 {
		points = append(points, want[r.IntN(len(want))]+uint64(r.IntN(3))-1)
	}
	kinds := []boundKind{unbounded, included, excluded}
	for _, src := range sources {
		for i, lo := range points {
			hi := points[(i*7+3)%len(points)]
			if lo > hi {
				lo, hi = hi, lo
			}
			for _, lk := range kinds {
				for _, uk := range kinds {
					b := Bounds[uint64]{lower: lo, upper: hi, lowerKind: lk, upperKind: uk}
					if lo == hi && lk == excluded && uk == excluded {
						continue
					}
					var in []uint64
					for _, k := range want {
						if aboveLower[uint64, natural[uint64]](&b, k) && belowUpper[uint64, natural[uint64]](&b, k) {
							in = append(in, k)
						}
					}
					if got := slices.Collect(src.scan(b)); !slices.Equal(got, in) {
						t.Fatalf("%s: Scan(%v) = %d keys, want %d", src.name, b, len(got), len(in))
					}
					slices.Reverse(in)
					if got := slices.Collect(src.scanReverse(b)); !slices.Equal(got, in) {
						t.Fatalf("%s: ScanReverse(%v) = %d keys, want %d", src.name, b, len(got), len(in))
					}
				}
			}
		}
	}
}

func TestScanBoundsPanicsAsUpstream(t *testing.T) {
	expectPanic := func(name, want string, f func()) {
		t.Helper()
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), want) {
				t.Fatalf("%s: panic %v, want %q", name, r, want)
			}
		}()
		f()
	}
	tree, mp, set := NewTree[int, int](), New[int, int](), NewSet[int]()
	inverted, equal := AtLeast(5).Below(4), Above(5).Below(5)
	for name, f := range map[string]func(Bounds[int]){
		"tree scan":         func(b Bounds[int]) { drain(tree.Scan(b)) },
		"tree scan reverse": func(b Bounds[int]) { drain(tree.ScanReverse(b)) },
		"tree extract":      func(b Bounds[int]) { tree.ExtractIf(b, func(int, int) bool { return true }) },
		"tree update":       func(b Bounds[int]) { tree.UpdateRange(b, func(_, v int) int { return v }) },
		"map scan":          func(b Bounds[int]) { drain(mp.Scan(b)) },
		"map scan reverse":  func(b Bounds[int]) { drain(mp.ScanReverse(b)) },
		"map first":         func(b Bounds[int]) { mp.FirstIn(b) },
		"map update":        func(b Bounds[int]) { mp.UpdateRange(b, func(_, v int) int { return v }) },
		"set scan": func(b Bounds[int]) {
			for range set.Scan(b) {
			}
		},
		"set extract": func(b Bounds[int]) { set.ExtractIf(b, func(int) bool { return true }) },
	} {
		expectPanic(name, "greater than range end", func() { f(inverted) })
		expectPanic(name, "equal and excluded", func() { f(equal) })
	}
	expectPanic("range", "greater than range end", func() { drain(tree.Range(5, 4)) })
	// Equal ends with one included hold nothing or one key, without panicking.
	tree.Insert(5, 50)
	for _, b := range []Bounds[int]{Between(5, 5), Above(5).AtMost(5)} {
		if n := len(slices.Collect(keysOf(tree.Scan(b)))); n != 0 {
			t.Fatalf("%v: %d keys", b, n)
		}
	}
	if got := slices.Collect(keysOf(tree.Scan(AtLeast(5).AtMost(5)))); len(got) != 1 {
		t.Fatalf("5..=5: %v", got)
	}
}

func TestExtractIfBounds(t *testing.T) {
	tree := NewTree[int, int]()
	for i := range 100 {
		tree.Insert(i, i)
	}
	var got []int
	for k := range tree.ExtractIf(Above(10).AtMost(20), func(k, _ int) bool { return k%2 == 0 }) {
		got = append(got, k)
	}
	if !slices.Equal(got, []int{12, 14, 16, 18, 20}) || tree.Len() != 95 {
		t.Fatalf("extracted %v, %d left", got, tree.Len())
	}
	got = got[:0]
	for k := range tree.ExtractIf(Below(3), func(int, int) bool { return true }) {
		got = append(got, k)
	}
	if !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("extracted %v", got)
	}
}

func drain[K, V any](seq iter.Seq2[K, V]) {
	for range seq {
	}
}

// Keys over a two-byte alphabet with trailing NULs tie on windows often;
// comparing past the tied bytes must still order and find them exactly.
func TestTiedTextKeysCompareFromDifference(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 9))
	tree, m := NewTree[string, int](), New[string, int]()
	bt := NewTreeOf[Bytes, int]()
	want := map[string]int{}
	for i := range 4000 {
		b := make([]byte, r.IntN(40))
		for j := range b {
			if r.IntN(3) == 0 {
				b[j] = 'a'
			}
		}
		tree.Insert(string(b), i)
		m.Insert(string(b), i)
		bt.Insert(Bytes(b), i)
		want[string(b)] = i
	}
	keys := slices.Sorted(maps.Keys(want))
	if got := slices.Collect(keysOf(tree.All())); !slices.Equal(got, keys) {
		t.Fatal("tree order")
	}
	for _, k := range keys {
		v1, ok1 := tree.Get(k)
		v2, ok2 := m.Get(k)
		v3, ok3 := bt.Get(Bytes(k))
		if !ok1 || !ok2 || !ok3 || v1 != want[k] || v2 != want[k] || v3 != want[k] {
			t.Fatalf("get %q", k)
		}
		if _, ok := tree.Get(k + "\x00"); ok != (want[k+"\x00"] != 0 || slices.Contains(keys, k+"\x00")) {
			t.Fatalf("padded miss %q", k)
		}
	}
}
