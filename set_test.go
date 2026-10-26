package gobbtree

import (
	"bytes"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"testing"
)

func TestUpstreamSetOracle(t *testing.T) {
	binary := os.Getenv("GOBBTREE_ORACLE")
	if binary == "" {
		t.Skip("set GOBBTREE_ORACLE to the pinned Rust oracle binary")
	}
	sets := [2]*Set[uint64]{NewSet[uint64](), NewSet[uint64]()}
	var input, want bytes.Buffer
	rng := rand.New(rand.NewPCG(97, 51))
	for i := 0; i < 6000; i++ {
		slot := rng.IntN(2)
		s := sets[slot]
		key := rng.Uint64N(5000)
		op := rng.IntN(14)
		if i < 1800 {
			op = 0
		}
		switch op {
		case 0, 1:
			fmt.Fprintf(&input, "I %d %d\n", slot, key)
			fmt.Fprintln(&want, s.Insert(key))
		case 2:
			fmt.Fprintf(&input, "D %d %d\n", slot, key)
			fmt.Fprintln(&want, s.Delete(key))
		case 3:
			fmt.Fprintf(&input, "G %d %d\n", slot, key)
			fmt.Fprintln(&want, s.Contains(key))
		case 4:
			fmt.Fprintf(&input, "P %d %d\n", slot, key%2)
			var k uint64
			var ok bool
			if key%2 == 0 {
				k, ok = s.PopFirst()
			} else {
				k, ok = s.PopLast()
			}
			if ok {
				fmt.Fprintln(&want, k)
			} else {
				fmt.Fprintln(&want, "-")
			}
		case 5:
			fmt.Fprintf(&input, "T %d\n", slot)
			s.Retain(func(k uint64) bool { return k%3 != 0 })
			fmt.Fprintln(&want, s.Len())
		case 6:
			limit := rng.IntN(5)
			fmt.Fprintf(&input, "E %d %d %d %d\n", slot, key, key+300, limit)
			if limit > 0 {
				count := 0
				for k := range s.ExtractIf(key, key+300, func(k uint64) bool { return k%2 == 0 }) {
					fmt.Fprintf(&want, "%d,", k)
					count++
					if count == limit {
						break
					}
				}
			}
			fmt.Fprintln(&want)
		case 7:
			fmt.Fprintf(&input, "S %d\n", key)
			sets[1] = sets[0].SplitOff(key)
			fmt.Fprintf(&want, "%d,%d\n", sets[0].Len(), sets[1].Len())
		case 8:
			fmt.Fprintln(&input, "A")
			sets[0].Append(sets[1])
			fmt.Fprintln(&want, sets[0].Len())
		case 9:
			fmt.Fprintln(&input, "L")
			fmt.Fprintf(&want, "%d,%d\n", sets[0].Len(), sets[1].Len())
		default:
			var values iter.Seq[uint64]
			code := ""
			switch op {
			case 10:
				code = "U"
				values = sets[0].Union(sets[1])
			case 11:
				code = "N"
				values = sets[0].Intersection(sets[1])
			case 12:
				code = "F"
				values = sets[0].Difference(sets[1])
			case 13:
				code = "Y"
				values = sets[0].SymmetricDifference(sets[1])
			}
			fmt.Fprintln(&input, code)
			for k := range values {
				fmt.Fprintf(&want, "%d,", k)
			}
			fmt.Fprintln(&want)
		}
	}
	cmd := exec.Command(binary, "sets")
	cmd.Stdin = &input
	actual, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rust set oracle: %v\n%s", err, actual)
	}
	if !bytes.Equal(actual, want.Bytes()) {
		t.Fatal("Rust set oracle differs")
	}
}

func TestSetOperations(t *testing.T) {
	rng := rand.New(rand.NewPCG(71, 23))
	s := NewSet[int]()
	reference := map[int]bool{}
	for i := 0; i < 20000; i++ {
		k := rng.IntN(2000)
		was := reference[k]
		if rng.IntN(3) != 0 {
			if s.Insert(k) == was {
				t.Fatal("insert", k)
			}
			reference[k] = true
		} else {
			if s.Delete(k) != was {
				t.Fatal("delete", k)
			}
			delete(reference, k)
		}
		if i%1000 == 0 {
			checkTree(t, &s.tree)
		}
	}
	var want []int
	for k := range reference {
		want = append(want, k)
	}
	slices.Sort(want)
	if !slices.Equal(slices.Collect(s.All()), want) {
		t.Fatal("iteration")
	}
	reverse := slices.Collect(s.Reverse())
	slices.Reverse(reverse)
	if !slices.Equal(reverse, want) {
		t.Fatal("reverse")
	}
	upper := s.SplitOff(1000)
	for k := range s.All() {
		if k >= 1000 {
			t.Fatal("split lower")
		}
	}
	for k := range upper.All() {
		if k < 1000 {
			t.Fatal("split upper")
		}
	}
	checkTree(t, &s.tree)
	checkTree(t, &upper.tree)
	s.Append(upper)
	if !upper.IsEmpty() || !slices.Equal(slices.Collect(s.All()), want) {
		t.Fatal("append")
	}
	clone := s.Clone()
	s.Retain(func(k int) bool { return k%2 == 0 })
	for k := range s.All() {
		if k%2 != 0 {
			t.Fatal("retain")
		}
	}
	if clone.Len() != len(want) {
		t.Fatal("clone changed")
	}
	before := clone.Len()
	count := 0
	for k := range clone.ExtractIf(0, 2000, func(k int) bool { return true }) {
		count++
		if clone.Contains(k) {
			t.Fatal("not extracted")
		}
		if count == 7 {
			break
		}
	}
	if clone.Len() != before-7 {
		t.Fatal("early stop")
	}
	checkTree(t, &clone.tree)
}

func TestSetAlgebra(t *testing.T) {
	for maskA := 0; maskA < 256; maskA++ {
		maskB := (maskA*137 + 53) & 255
		a, b := NewSet[int](), NewSet[int]()
		for k := 0; k < 8; k++ {
			if maskA&(1<<k) != 0 {
				a.Insert(k)
			}
			if maskB&(1<<k) != 0 {
				b.Insert(k)
			}
		}
		check := func(values []int, mask int) {
			t.Helper()
			var want []int
			for k := 0; k < 8; k++ {
				if mask&(1<<k) != 0 {
					want = append(want, k)
				}
			}
			if !slices.Equal(values, want) {
				t.Fatalf("%d %d got %v want %v", maskA, maskB, values, want)
			}
		}
		check(slices.Collect(a.Union(b)), maskA|maskB)
		check(slices.Collect(a.Intersection(b)), maskA&maskB)
		check(slices.Collect(a.Difference(b)), maskA&^maskB)
		check(slices.Collect(a.SymmetricDifference(b)), maskA^maskB)
		if a.IsSubset(b) != (maskA&^maskB == 0) || a.IsDisjoint(b) != (maskA&maskB == 0) || a.IsSuperset(b) != (maskB&^maskA == 0) {
			t.Fatal("relations")
		}
		for range a.Union(b) {
			break
		}
		if a.Len() != len(slices.Collect(a.All())) {
			t.Fatal("early stop changed set")
		}
	}
}

func TestSetStoredKeyAndBatch(t *testing.T) {
	s := NewSet[float64]()
	negative := math.Copysign(0, -1)
	s.Insert(negative)
	s.Insert(0)
	if key, ok := s.Get(0); !ok || !math.Signbit(key) {
		t.Fatal("insert replaced stored key")
	}
	if old, ok := s.Replace(0); !ok || !math.Signbit(old) {
		t.Fatal("replace old key")
	}
	if key, ok := s.Get(negative); !ok || math.Signbit(key) {
		t.Fatal("replace did not store new key")
	}
	x := NewSet[int]()
	keys := []int{4, 2, 4, 7}
	result := make([]bool, 4)
	x.InsertMany(keys, result)
	if !slices.Equal(result, []bool{true, true, false, true}) {
		t.Fatal(result)
	}
	x.ContainsMany([]int{2, 3, 7, 4}, result)
	if !slices.Equal(result, []bool{true, false, true, true}) {
		t.Fatal(result)
	}
}

func TestTreeBoundsAndMoves(t *testing.T) {
	tr := NewTree[uint64, uint64]()
	for i := uint64(0); i < 5000; i++ {
		tr.Insert(i*2, i)
	}
	for key := uint64(0); key < 10002; key++ {
		k, v, ok := tr.LowerBound(key)
		want := (key + 1) / 2
		if ok != (want < 5000) || (ok && (k != want*2 || v != want)) {
			t.Fatal("lower", key, k, v, ok)
		}
		k, v, ok = tr.UpperBound(key)
		want = key/2 + 1
		if ok != (want < 5000) || (ok && (k != want*2 || v != want)) {
			t.Fatal("upper", key, k, v, ok)
		}
	}
	for _, cut := range []uint64{5001, 0, math.MaxUint64, 9998, 1500} {
		copy := tr.Clone()
		upper := copy.SplitOff(cut)
		checkTree(t, copy)
		checkTree(t, upper)
		if copy.Len()+upper.Len() != tr.Len() {
			t.Fatal("split length")
		}
		copy.Append(upper)
		checkTree(t, copy)
		if !upper.IsEmpty() || copy.Len() != tr.Len() {
			t.Fatal("append length")
		}
		for k, v := range tr.All() {
			if actual, ok := copy.Get(k); !ok || actual != v {
				t.Fatal("move lost entry")
			}
		}
	}
	other := NewTree[uint64, uint64]()
	other.Insert(4, 99)
	tr.Append(other)
	if v, _ := tr.Get(4); v != 99 {
		t.Fatal("append precedence")
	}
}
