package gobbtree

import (
	"math"
	"reflect"
	"testing"
	"unsafe"
)

func TestWordArenaIDsAcrossSlabGrowth(t *testing.T) {
	for _, shared := range []bool{false, true} {
		tr := wordTree{shared: shared, atomicWrites: shared}
		live := make(map[uint32]uint64)
		for i := 0; i < 16000; i++ {
			leaf := i%23 != 0
			id := tr.alloc(leaf)
			if _, exists := live[id]; exists {
				t.Fatalf("shared=%v: allocated live ID %d twice", shared, id)
			}
			n := tr.node(id)
			if n.leaf != leaf || (id&wordLeafID != 0) != leaf {
				t.Fatal("allocated node has the wrong kind")
			}
			value := uint64(i + 1)
			if leaf {
				n.entries()[0].key = value
			} else {
				n.keys[0] = value
			}
			live[id] = value
			if i%7 == 0 {
				tr.release(id)
				delete(live, id)
			}
		}
		nodes := 0
		for _, slab := range tr.slabs {
			nodes += len(slab)
		}
		cache := wordReadCache{nodes: make([]wordNode, nodes)}
		positions := make(map[*wordNode]uint32)
		for id, value := range live {
			if got := tr.node(id).keyAt(0); got != value {
				t.Fatalf("ID %d changed during growth: got %d, want %d", id, got, value)
			}
			n := cache.node(id)
			if n == nil {
				t.Fatalf("ID %d cannot address the compact read cache", id)
			}
			if other, exists := positions[n]; exists {
				t.Fatalf("IDs %d and %d alias in the read cache", id, other)
			}
			positions[n] = id
		}
	}
}

func TestOwnerDeleteWithCollidingTags(t *testing.T) {
	const size = 1 << 16
	keyAt := func(i int) uint64 { return uint64(i)<<48 | uint64(i)<<8 }
	var tr Tree[uint64, uint64]
	for i := 0; i < size; i++ {
		key := keyAt(i * 499 & (size - 1))
		tr.Insert(key, ^key)
	}
	if len(tr.words.routes.ids) == 0 {
		t.Fatal("test did not populate cached routes")
	}
	// Every key has the same byte tag. Remove in a different permutation,
	// exercising collisions, changing minima, merges, and eventual collapse.
	for i := 0; i < size; i++ {
		key := keyAt(i * 31337 & (size - 1))
		if value, ok := tr.Delete(key); !ok || value != ^key {
			t.Fatalf("delete %x: got %x, %v", key, value, ok)
		}
		if value, ok := tr.Delete(key); ok || value != 0 {
			t.Fatalf("deleted key %x still present", key)
		}
		if tr.Len() != size-i-1 {
			t.Fatal("incorrect length after deletion")
		}
		if i%1024 == 0 {
			checkTree(t, &tr)
		}
	}
	checkTree(t, &tr)
	tr.Insert(math.MaxUint64, 7)
	if value, ok := tr.Delete(math.MaxUint64); !ok || value != 7 {
		t.Fatal("could not reuse the empty tree")
	}
	checkTree(t, &tr)
}

func checkWordTree(t testing.TB, tr *wordTree, length int) {
	t.Helper()
	checkWordRoutes(t, tr)
	if tr.root == 0 {
		if length != 0 {
			t.Fatal("empty numeric arena with nonzero length")
		}
		return
	}
	seen := make(map[uint32]bool)
	leaves := []uint32{}
	depth, total := -1, 0
	var walk func(uint32, int)
	walk = func(id uint32, level int) {
		if id == 0 || int(((id&^wordLeafID)-1)>>8) >= len(tr.slabs) || int(((id&^wordLeafID)-1)&255) >= len(tr.slabs[((id&^wordLeafID)-1)>>8]) {
			t.Fatalf("invalid node ID %d", id)
		}
		if seen[id] {
			t.Fatal("cyclic or multiply linked numeric node")
		}
		seen[id] = true
		n := tr.node(id)
		if n.n < 1 || n.n > nodeSize || (level > 0 && n.n < minNodeSize) {
			t.Fatalf("bad numeric occupancy %d at depth %d", n.n, level)
		}
		for i := 0; i < int(n.n); i++ {
			if i > 0 && n.keyAt(i-1) >= n.keyAt(i) {
				t.Fatal("unordered numeric node")
			}
			tag := uint8(n.keyAt(i))
			if tr.shared && !n.leaf {
				tag = uint8(n.keyAt(i) >> uint(n.sequence.Load()>>7&63))
			}
			if n.tags[i] != tag {
				t.Fatal("stale numeric tag")
			}
		}
		for i := int(n.n); i < nodeSize; i++ {
			padding := uint8(0)
			if tr.shared && !n.leaf {
				padding = 255
			}
			if n.keyAt(i) != math.MaxUint64 || n.tags[i] != padding {
				t.Fatal("invalid numeric padding")
			}
		}
		if n.leaf {
			if depth < 0 {
				depth = level
			}
			if depth != level {
				t.Fatal("unequal numeric leaf depth")
			}
			if n.directory != 0 {
				t.Fatal("leaf retained a routing directory")
			}
			total += int(n.n)
			leaves = append(leaves, id)
			return
		}
		if level == 0 && n.n < 2 {
			t.Fatal("uncollapsed numeric root")
		}
		for i := 0; i < nodeSize; i++ {
			if n.inner().high[i] != uint32(n.keyAt(i)>>32) {
				t.Fatal("stale numeric high summary")
			}
			if i >= int(n.n) {
				if n.inner().children[i] != 0 {
					t.Fatal("retained numeric child")
				}
				continue
			}
			child := n.inner().children[i]
			walk(child, level+1)
			if n.keyAt(i) != tr.node(child).keyAt(0) {
				t.Fatal("stale numeric minimum")
			}
		}
	}
	walk(tr.root, 0)
	for i, id := range leaves {
		n := tr.node(id)
		var prev, next uint32
		if i > 0 {
			prev = leaves[i-1]
			p := tr.node(prev)
			if p.keyAt(int(p.n)-1) >= n.keyAt(0) {
				t.Fatal("overlapping numeric leaves")
			}
		}
		if i+1 < len(leaves) {
			next = leaves[i+1]
		}
		if n.prev != prev || n.next != next {
			t.Fatal("broken numeric leaf links")
		}
	}
	for _, head := range []uint32{tr.free, tr.freeInner} {
		for id := head; id != 0; id = tr.node(id).directory {
			if seen[id] {
				t.Fatal("live or cyclic free node")
			}
			seen[id] = true
		}
	}
	if total != length {
		t.Fatalf("numeric length %d, counted %d", length, total)
	}
}

func TestWordNodePointerFree(t *testing.T) {
	var check func(reflect.Type)
	check = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				check(typ.Field(i).Type)
			}
		case reflect.Array:
			check(typ.Elem())
		case reflect.Int32, reflect.Uint8, reflect.Uint32, reflect.Uint64, reflect.Bool:
		default:
			t.Fatalf("GC-scanned field %v", typ)
		}
	}
	check(reflect.TypeFor[wordNode]())
	check(reflect.TypeFor[numericColumns]())
	check(reflect.TypeFor[atomicPairs]())
	if unsafe.Sizeof(wordNode{}) != 576 || unsafe.Offsetof(wordNode{}.payload) != 64 {
		t.Fatal("numeric node layout changed")
	}
}
func TestWordValues(t *testing.T) {
	for _, value := range []float64{0, math.Copysign(0, -1), math.Inf(-1), math.Inf(1), math.Float64frombits(0x7ff8000000001234)} {
		var tr Tree[int64, float64]
		for _, key := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
			tr.Insert(key, value)
		}
		if tr.words == nil {
			t.Fatal("numeric specialization not selected")
		}
		for key, got := range tr.All() {
			if math.Float64bits(got) != math.Float64bits(value) {
				t.Fatalf("value bits at %d", key)
			}
			actual, ok := tr.Get(key)
			if !ok || math.Float64bits(actual) != math.Float64bits(value) {
				t.Fatal("lookup changed value bits")
			}
		}
		clone := tr.Clone()
		tr.Clear()
		if tr.words != nil || tr.Len() != 0 || clone.Len() != 5 {
			t.Fatal("numeric clear or clone")
		}
	}
	var tr Tree[uint64, *int]
	value := 1
	tr.Insert(1, &value)
	if tr.words != nil {
		t.Fatal("reference value hidden in numeric storage")
	}
}

func checkWordRoutes(t testing.TB, tr *wordTree) {
	t.Helper()
	if tr.root != 0 && tr.rootNode != tr.node(tr.root) {
		t.Fatal("stale root pointer")
	}
	if len(tr.routes.ids) == 0 {
		return
	}
	for bucket, id := range tr.routes.ids {
		if id == 0 {
			continue
		}
		low := tr.routes.base | uint64(bucket)<<tr.routes.shift
		high := low | ((uint64(1) << tr.routes.shift) - 1)
		for _, key := range []uint64{low, high} {
			want := tr.root
			for step := 0; step < int(tr.routes.depth); step++ {
				n := tr.node(want)
				i := 0
				for i+1 < int(n.n) && n.keys[i+1] <= key {
					i++
				}
				want = n.inner().children[i]
			}
			if id != want {
				t.Fatalf("radix bucket %d endpoint %x routes to %d, want %d", bucket, key, id, want)
			}
		}
	}
}

func TestConcurrentNumericLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 && unsafe.Sizeof(node[uint64, uint64]{}) > 512 {
		t.Fatal("numeric writer node exceeded the small-object scan threshold")
	}
	if unsafe.Offsetof(searchImage[uint64, uint64]{}.tags)+nodeSize > 64 {
		t.Fatal("numeric reader tags spilled out of the hot header")
	}
	if unsafe.Offsetof(atomicPairs{}.entries) != 64 || unsafe.Sizeof(atomicPairs{}) != 576 {
		t.Fatal("atomic pairs are not aligned inside their block")
	}
}

func TestWordOuterLeafRedistribution(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, test := range []struct {
			name   string
			counts []int
			index  int
			split  bool
		}{
			{"left", []int{24, 32, 32, 32, 32}, 2, false},
			{"right", []int{32, 32, 32, 32, 24}, 2, false},
			{"choose_left", []int{24, 32, 32, 32, 28}, 2, false},
			{"choose_right", []int{28, 32, 32, 32, 24}, 2, false},
			{"first", []int{32, 32, 17}, 0, false},
			{"last", []int{24, 32, 32}, 2, false},
			{"nearly_full", []int{31, 32, 32}, 2, true},
			{"full", []int{32, 32, 32, 32, 32}, 2, true},
			{"two", []int{32, 32}, 0, true},
		} {
			t.Run(test.name, func(t *testing.T) {
				tr := wordTree{shared: shared, atomicWrites: shared, keyKind: 1, valueKind: 1}
				tr.root = tr.alloc(false)
				parent := tr.node(tr.root)
				tr.rootNode = parent
				want := make(map[uint64]uint64)
				var previous uint32
				for i, count := range test.counts {
					id := tr.alloc(true)
					leaf := tr.node(id)
					for j := 0; j < count; j++ {
						key := uint64(i*100 + j*2)
						tr.insertLeaf(leaf, j, key, key+1)
						want[key] = key + 1
					}
					leaf.prev = previous
					if previous != 0 {
						tr.node(previous).next = id
					}
					previous = id
					tr.insertChild(parent, i, id)
				}
				key := uint64(test.index*100 + 1)
				tr.makeLeafRoom(parent, test.index, key, key+1)
				want[key] = key + 1
				children := len(test.counts)
				if test.split || !shared {
					children++
				}
				if int(parent.n) != children {
					t.Fatalf("shared=%v: got %d leaves, want %d", shared, parent.n, children)
				}
				checkWordTree(t, &tr, len(want))
				tree := Tree[uint64, uint64]{words: &tr, length: len(want)}
				for key, value := range want {
					if got, ok := tree.Get(key); !ok || got != value {
						t.Fatalf("shared=%v: lookup %d = %d, %v; want %d", shared, key, got, ok, value)
					}
				}
			})
		}
	}
}

func TestWordLeafRedistribution(t *testing.T) {
	for _, order := range []string{"ascending", "descending", "mixed"} {
		t.Run(order, func(t *testing.T) {
			var tree Tree[uint64, uint64]
			const size = 4096
			keys := make([]uint64, size)
			for i := range keys {
				key := uint64(i) * 2
				if order == "descending" {
					key = uint64(size-1-i) * 2
				}
				if order == "mixed" {
					key = mixedKey(uint64(i))
				}
				keys[i] = key
				tree.Insert(key, key^123)
				if i%31 == 0 {
					checkWordTree(t, tree.words, i+1)
				}
			}
			checkWordTree(t, tree.words, size)
			for _, key := range keys {
				if value, ok := tree.Get(key); !ok || value != key^123 {
					t.Fatalf("lookup %d", key)
				}
			}
			for i, key := range keys {
				if i%2 == 0 {
					tree.Delete(key)
				}
			}
			checkWordTree(t, tree.words, size/2)
			for i, key := range keys {
				if i%2 == 0 {
					tree.Insert(key, key^123)
				}
			}
			checkWordTree(t, tree.words, size)
		})
	}
}
