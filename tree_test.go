package gobbtree

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func checkTree[K cmp.Ordered, V any](t testing.TB, tr *Tree[K, V]) { checkCore(t, &tr.treeCore) }

func checkCore[K any, V any, O keyOrder[K]](t testing.TB, tr *treeCore[K, V, O]) {
	t.Helper()
	if tr.words != nil {
		checkWordTree(t, tr.words, tr.length)
		return
	}
	if tr.root == nil {
		if tr.length != 0 {
			t.Fatal("nil root with nonzero length")
		}
		return
	}
	var leaves []*node[K, V, O]
	leafDepth := -1
	total := 0
	var walk func(*node[K, V, O], int)
	walk = func(n *node[K, V, O], depth int) {
		if n.n < 1 || n.n > nodeSize || (depth > 0 && n.n < minNodeSize) {
			t.Fatalf("bad occupancy %d at depth %d", n.n, depth)
		}
		if n.children != nil && n == tr.root && n.n < 2 {
			t.Fatal("uncollapsed root")
		}
		for i := 0; i < n.n; i++ {
			if i > 0 && compareKeys[O](n.keys[i-1], n.keys[i]) >= 0 {
				t.Fatal("keys unordered")
			}
			if n.codes[i] != windowKey[O](n.keys[i], n.prefix) {
				t.Fatal("stale inline key")
			}
			if i > 0 && n.codes[i-1] > n.codes[i] {
				t.Fatal("codes unordered")
			}
		}
		for i := n.n; i < nodeSize; i++ {
			if n.codes[i] != math.MaxUint64 {
				t.Fatal("bad padding")
			}
		}
		if n.leaf != nil {
			if n.children != nil {
				t.Fatal("both node kinds")
			}
			if leafDepth < 0 {
				leafDepth = depth
			}
			if leafDepth != depth {
				t.Fatal("unbalanced leaf depths")
			}
			leaves = append(leaves, n)
			total += n.n
			return
		}
		for i := 0; i < n.n; i++ {
			if n.children[i] == nil || compareKeys[O](n.keys[i], n.children[i].keys[0]) != 0 {
				t.Fatal("stale child minimum")
			}
			walk(n.children[i], depth+1)
		}
		for i := n.n; i < nodeSize; i++ {
			if n.children[i] != nil {
				t.Fatal("retained child")
			}
		}
	}
	walk(tr.root, 0)
	for i, n := range leaves {
		var prev, next *node[K, V, O]
		if i > 0 {
			prev = leaves[i-1]
			if compareKeys[O](prev.keys[prev.n-1], n.keys[0]) >= 0 {
				t.Fatal("leaves overlap")
			}
		}
		if i+1 < len(leaves) {
			next = leaves[i+1]
		}
		if n.leaf.prev != prev || n.leaf.next != next {
			t.Fatal("broken leaf links")
		}
	}
	if total != tr.length {
		t.Fatalf("length %d, counted %d", tr.length, total)
	}
}

func TestTreeGrowthAndCollapse(t *testing.T) {
	for _, order := range []string{"ascending", "descending", "random"} {
		t.Run(order, func(t *testing.T) {
			var tr Tree[uint64, uint64]
			const size = 20000
			keys := make([]int, size)
			for i := range keys {
				keys[i] = i
			}
			rng := rand.New(rand.NewPCG(12, 34))
			if order == "descending" {
				slices.Reverse(keys)
			}
			if order == "random" {
				rng.Shuffle(size, func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
			}
			for _, k := range keys {
				if _, ok := tr.Insert(uint64(k), uint64(k)*3); ok {
					t.Fatal("unexpected replacement")
				}
			}
			checkTree(t, &tr)
			for k := 0; k < size; k++ {
				v, ok := tr.Get(uint64(k))
				if !ok || v != uint64(k)*3 {
					t.Fatalf("get %d", k)
				}
			}
			for _, k := range keys {
				old, ok := tr.Insert(uint64(k), uint64(k)*5)
				if !ok || old != uint64(k)*3 {
					t.Fatal("replacement")
				}
			}
			for i, k := range keys {
				old, ok := tr.Delete(uint64(k))
				if !ok || old != uint64(k)*5 {
					t.Fatalf("delete %d", k)
				}
				if i%193 == 0 {
					checkTree(t, &tr)
				}
			}
			checkTree(t, &tr)
			if !tr.IsEmpty() {
				t.Fatal("not empty")
			}
			tr.Insert(math.MaxUint64, 7)
			tr.Insert(0, 8)
			checkTree(t, &tr)
		})
	}
}

func runTrace[K cmp.Ordered](t testing.TB, keys []K, operations []byte) {
	t.Helper()
	var tr Tree[K, uint64]
	ref := make(map[K]uint64)
	for i, op := range operations {
		key := keys[i%len(keys)]
		value := uint64(i) * 7919
		switch op % 4 {
		case 0, 1:
			want, had := ref[key]
			old, ok := tr.Insert(key, value)
			if old != want || ok != had {
				t.Fatalf("insert %v: (%v,%v), want (%v,%v)", key, old, ok, want, had)
			}
			ref[key] = value
		case 2:
			want, had := ref[key]
			old, ok := tr.Delete(key)
			if old != want || ok != had {
				t.Fatalf("delete %v", key)
			}
			delete(ref, key)
		case 3:
			want, had := ref[key]
			got, ok := tr.Get(key)
			if got != want || ok != had {
				t.Fatalf("get %v", key)
			}
		}
		if i%137 == 0 {
			checkTree(t, &tr)
		}
	}
	checkTree(t, &tr)
	ordered := make([]K, 0, len(ref))
	for k := range ref {
		ordered = append(ordered, k)
	}
	slices.Sort(ordered)
	pos := 0
	for k, v := range tr.All() {
		if k != ordered[pos] || v != ref[k] {
			t.Fatal("iteration")
		}
		pos++
	}
	if pos != len(ref) {
		t.Fatal("iteration length")
	}
	pos = len(ordered) - 1
	for k, v := range tr.Reverse() {
		if k != ordered[pos] || v != ref[k] {
			t.Fatal("reverse iteration")
		}
		pos--
	}
	if pos != -1 {
		t.Fatal("reverse length")
	}
	for _, k := range ordered {
		if v, ok := tr.Get(k); !ok || v != ref[k] {
			t.Fatal("final lookup")
		}
	}
}
func TestRandomDifferential(t *testing.T) {
	rng := rand.New(rand.NewPCG(55, 66))
	const n = 60000
	keys := make([]int64, n)
	ops := make([]byte, n)
	for i := range keys {
		keys[i] = rng.Int64N(4096) - 2048
		ops[i] = byte(rng.Uint32())
	}
	runTrace(t, keys, ops)
}
func TestStringsAndPrefixes(t *testing.T) {
	rng := rand.New(rand.NewPCG(8, 9))
	const n = 18000
	pool := []string{"", "\x00", "\x00\x00", "a", "a\x00", "a\x00\x00", "abcdefgh", "abcdefgh\x00", "abcdefgh1", "abcdefghi", "\xff"}
	for i := 0; i < 1000; i++ {
		pool = append(pool, fmt.Sprintf("/tenant/long/shared/prefix/records/%08d/entry", i))
	}
	keys := make([]string, n)
	ops := make([]byte, n)
	for i := range keys {
		keys[i] = pool[rng.IntN(len(pool))]
		ops[i] = byte(rng.Uint32())
	}
	runTrace(t, keys, ops)
	var tr Tree[string, int]
	for _, s := range pool {
		tr.Insert(s, 1)
	}
	for _, s := range []string{"/tenant/long", "/tenant/long/shared/prefix/records/", "/tenans", "/tenanu", "abcdefgh\x00z"} {
		if _, ok := tr.Get(s); ok {
			t.Fatalf("false hit %q", s)
		}
	}
	checkTree(t, &tr)
}
func TestFloatsAndDefinedTypes(t *testing.T) {
	var tr Tree[float64, int]
	keys := []float64{math.NaN(), math.Inf(-1), -9, -math.SmallestNonzeroFloat64, math.Copysign(0, -1), 0, math.SmallestNonzeroFloat64, 9, math.Inf(1), math.Float64frombits(0x7ff8000000000022)}
	for i, k := range keys {
		tr.Insert(k, i)
	}
	if tr.Len() != 8 {
		t.Fatal(tr.Len())
	}
	checkTree(t, &tr)
	for _, k := range keys {
		if _, ok := tr.Get(k); !ok {
			t.Fatal(k)
		}
	}
	if v, ok := tr.Get(math.NaN()); !ok || v != 9 {
		t.Fatal("NaN replacement")
	}
	if v, ok := tr.Get(0); !ok || v != 5 {
		t.Fatal("zero replacement")
	}
	type ID uint64
	type Name string
	type Number int16
	var ids Tree[ID, int]
	ids.Insert(ID(math.MaxUint64), 1)
	ids.Insert(0, 2)
	checkTree(t, &ids)
	var names Tree[Name, int]
	names.Insert("abc/a", 1)
	names.Insert("abc/b", 2)
	checkTree(t, &names)
	if v, ok := names.Get("abc/b"); !ok || v != 2 {
		t.Fatal("defined string")
	}
	var ints Tree[Number, int]
	ints.Insert(-32768, 1)
	ints.Insert(32767, 2)
	checkTree(t, &ints)
}
func TestRangesBatchesAndOwnership(t *testing.T) {
	var tr Tree[int, int]
	for i := 0; i < 5000; i++ {
		tr.Insert(i, i*2)
	}
	for _, bounds := range [][2]int{{-5, 0}, {-5, 90}, {15, 33}, {16, 32}, {319, 811}, {4990, 6000}, {9, 9}, {8, 4}} {
		var got, rev []int
		tr.Scan(bounds[0], bounds[1], func(k, v int) bool {
			if v != k*2 {
				t.Fatal("value")
			}
			got = append(got, k)
			return true
		})
		tr.ScanReverse(bounds[0], bounds[1], func(k, v int) bool { rev = append(rev, k); return true })
		var want []int
		for i := max(0, bounds[0]); i < min(5000, bounds[1]); i++ {
			want = append(want, i)
		}
		if !slices.Equal(got, want) {
			t.Fatal("scan", bounds, got, want)
		}
		slices.Reverse(rev)
		if !slices.Equal(rev, want) {
			t.Fatal("reverse scan", bounds)
		}
	}
	calls := 0
	tr.Scan(0, 5000, func(k, v int) bool { calls++; return false })
	if calls != 1 {
		t.Fatal(calls)
	}
	queries := []int{0, 4999, 5000, 100, 100, -1, 31, 32, 33, 3000, 200, 4000, 8000, 25, 1, 0, 2}
	dst := make([]Result[int], len(queries))
	tr.GetMany(queries, dst)
	for i, k := range queries {
		v, ok := tr.Get(k)
		if dst[i] != (Result[int]{v, ok}) {
			t.Fatal("batch", i)
		}
	}
	previous := make([]Result[int], 3)
	tr.InsertMany([]Entry[int, int]{{5001, 1}, {5001, 2}, {5001, 3}}, previous)
	if previous[0].OK || previous[1] != (Result[int]{1, true}) || previous[2] != (Result[int]{2, true}) {
		t.Fatal(previous)
	}
	clone := tr.Clone()
	clone.Delete(0)
	if !tr.Contains(0) {
		t.Fatal("clone aliases nodes")
	}
	k, _, ok := tr.PopFirst()
	if !ok || k != 0 {
		t.Fatal("pop first")
	}
	k, _, ok = tr.PopLast()
	if !ok || k != 5001 {
		t.Fatal("pop last")
	}
	tr.Clear()
	if _, _, ok = tr.First(); ok {
		t.Fatal("empty first")
	}
	checkTree(t, &tr)
}
func TestShortDestinations(t *testing.T) {
	var tr Tree[int, int]
	tr.Insert(1, 2)
	for _, call := range []func(){func() { tr.GetMany([]int{1}, nil) }, func() { tr.InsertMany([]Entry[int, int]{{2, 3}}, nil) }} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("missing panic")
				}
			}()
			call()
		}()
	}
	if tr.Len() != 1 {
		t.Fatal("mutated before validation")
	}
}
func TestSearchAllLengths(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for n := 0; n <= nodeSize; n++ {
		for trial := 0; trial < 200; trial++ {
			var keys [nodeSize]uint64
			for i := 0; i < n; i++ {
				keys[i] = rng.Uint64N(20)
			}
			slices.Sort(keys[:n])
			for i := n; i < nodeSize; i++ {
				keys[i] = math.MaxUint64
			}
			for _, q := range []uint64{0, 1, 5, 15, 19, 20, math.MaxUint64 - 1, math.MaxUint64} {
				want, _ := slices.BinarySearch(keys[:n], q)
				if got := lowerCode(&keys, n, q); got != want {
					t.Fatalf("n=%d q=%d got=%d want=%d", n, q, got, want)
				}
			}
		}
	}
	hardware, width := SIMD()
	t.Logf("hardware SIMD=%v, vector bits=%d", hardware, width)
}
func TestSteadyStateAllocations(t *testing.T) {
	tr := NewTree[uint64, uint64]()
	m := New[uint64, uint64]()
	for i := uint64(0); i < 1000; i++ {
		tr.Insert(i, i)
		m.Insert(i, i)
	}
	queries := []uint64{1, 9, 500, 999, 9999}
	dst := make([]Result[uint64], len(queries))
	for name, call := range map[string]func(){
		"tree get": func() { tr.Get(500) }, "map get": func() { m.Get(500) },
		"tree replace": func() { tr.Insert(500, 2) }, "map replace": func() { m.Insert(500, 2) },
		"tree churn": func() { tr.Delete(500); tr.Insert(500, 2) },
		"map churn":  func() { m.Delete(500); m.Insert(500, 2) },
		"tree batch": func() { tr.GetMany(queries, dst) }, "map batch": func() { m.GetMany(queries, dst) },
		"tree scan": func() { tr.Scan(300, 340, func(k, v uint64) bool { return true }) },
		"map scan":  func() { m.Scan(300, 340, func(k, v uint64) bool { return true }) },
	} {
		if n := testing.AllocsPerRun(100, call); n != 0 {
			t.Errorf("%s: %v allocations", name, n)
		}
	}
}

// Textual maps with non-word values read leaves under their latch, so leaf
// writes publish no search image.
func TestTextualMapWritesDoNotAllocate(t *testing.T) {
	m := New[string, [2]int]()
	c := NewMapOf[textKey, [2]int]()
	for i := range 1000 {
		key := fmt.Sprintf("key-%04d", i)
		m.Insert(key, [2]int{i})
		c.Insert(textKey{key}, [2]int{i})
	}
	key, custom := "key-0500", textKey{"key-0500"}
	for name, call := range map[string]func(){
		"map replace":  func() { m.Insert(key, [2]int{1}) },
		"map churn":    func() { m.Delete(key); m.Insert(key, [2]int{2}) },
		"map get":      func() { m.Get(key) },
		"custom churn": func() { c.Delete(custom); c.Insert(custom, [2]int{2}) },
		"custom get":   func() { c.Get(custom) },
	} {
		if n := testing.AllocsPerRun(100, call); n != 0 {
			t.Errorf("%s: %v allocations", name, n)
		}
	}
}
// Word-valued textual maps are read from the live node, so a churn costs
// only the new key's interned copy.
func TestWordTextMapWritesAllocateOnlyKeys(t *testing.T) {
	if !seqlockPlainStores {
		t.Skip("textual leaves publish images on this target")
	}
	m := New[string, int]()
	for i := range 1000 {
		m.Insert(fmt.Sprintf("key-%04d", i), i)
	}
	key := "key-0500"
	if n := testing.AllocsPerRun(100, func() { m.Insert(key, 1) }); n != 0 {
		t.Errorf("replace: %v allocations", n)
	}
	if n := testing.AllocsPerRun(100, func() { m.Delete(key); m.Insert(key, 2) }); n != 1 {
		t.Errorf("churn: %v allocations, want the interned key", n)
	}
}
func FuzzTree(f *testing.F) {
	f.Add([]byte{0, 0, 1, 1, 2, 0, 3, 1})
	f.Add([]byte("a long shared prefix with embedded\x00bytes"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		if len(data) > 4096 {
			data = data[:4096]
		}
		// Keep operation bytes independent of key bytes so a single key can
		// be inserted, replaced, deleted and read in one fuzz input.
		if len(data) < 2 {
			return
		}
		count := len(data) / 2
		keys := make([]int, count)
		operations := make([]byte, count)
		for i := 0; i < count; i++ {
			keys[i] = int(data[2*i]) - 128
			operations[i] = data[2*i+1]
		}
		runTrace(t, keys, operations)
	})
}
