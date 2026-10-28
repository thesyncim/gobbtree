package gobbtree

import (
	"math"
	"reflect"
	"slices"
	"sort"
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
		for i := range tr.slabs {
			nodes += wordSlabSize(i)
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

func TestOwnerReinsertHintInvalidation(t *testing.T) {
	for _, action := range []string{"immediate", "read", "insert", "replace", "delete", "miss", "pop-first", "pop-last", "split-append", "clear"} {
		t.Run(action, func(t *testing.T) {
			tr := NewTree[uint64, uint64]()
			want := make(map[uint64]uint64)
			for i := uint64(0); i < 2048; i++ {
				key := mixedKey(i)
				tr.Insert(key, i)
				want[key] = i
			}
			var key uint64
			found := false
			for k := range tr.All() {
				n := tr.words.node(tr.words.leafFor(k))
				if n.n > minNodeSize && n.lowerEntry(k) > 0 {
					key, found = k, true
					break
				}
			}
			if !found {
				t.Fatal("no eligible deletion")
			}
			tr.Delete(key)
			delete(want, key)
			if tr.words.deletedLeaf == nil {
				t.Fatal("deletion did not retain its position")
			}
			switch action {
			case "read":
				for k, v := range want {
					if got, ok := tr.Get(k); !ok || got != v {
						t.Fatal("read changed after delete")
					}
				}
			case "insert":
				tr.Insert(mixedKey(99999), 17)
				want[mixedKey(99999)] = 17
			case "replace":
				k, _, _ := tr.Last()
				tr.Insert(k, 17)
				want[k] = 17
			case "delete":
				k, _, _ := tr.First()
				tr.Delete(k)
				delete(want, k)
			case "miss":
				tr.Delete(mixedKey(99999))
			case "pop-first":
				k, _, _ := tr.PopFirst()
				delete(want, k)
			case "pop-last":
				k, _, _ := tr.PopLast()
				delete(want, k)
			case "split-append":
				other := tr.SplitOff(1 << 63)
				tr.Append(other)
			case "clear":
				tr.Clear()
				clear(want)
			}
			if old, replaced := tr.Insert(key, 123); replaced || old != 0 {
				t.Fatal("reinsert reported an existing entry")
			}
			want[key] = 123
			if tr.Len() != len(want) {
				t.Fatal("length mismatch")
			}
			for k, v := range want {
				if got, ok := tr.Get(k); !ok || got != v {
					t.Fatalf("key=%x got=%d,%v want=%d", k, got, ok, v)
				}
			}
			checkTree(t, tr)
		})
	}
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
		if id == 0 || int(((id&^wordLeafID)-1)>>8) >= len(tr.slabs) || int(((id&^wordLeafID)-1)&255) >= wordSlabSize(int(((id&^wordLeafID)-1)>>8)) {
			t.Fatalf("invalid node ID %d", id)
		}
		if seen[id] {
			t.Fatal("cyclic or multiply linked numeric node")
		}
		seen[id] = true
		n := tr.node(id)
		// Leaves beside an ascending run may hold fewer than minNodeSize.
		if n.n < 1 || n.n > nodeSize || (level > 0 && !n.leaf && n.n < minNodeSize) {
			t.Fatalf("bad numeric occupancy %d at depth %d", n.n, level)
		}
		for i := 0; i < int(n.n); i++ {
			if i > 0 && n.keyAt(i-1) >= n.keyAt(i) {
				t.Fatal("unordered numeric node")
			}
			// Internal nodes route by key, inline and directory columns;
			// their tags are unused and not maintained.
			if n.leaf && n.tags[i] != uint8(n.keyAt(i)) {
				t.Fatal("stale numeric tag")
			}
			if !n.leaf && n.inner().inline[i] != uint16(n.keyAt(i)>>n.shift) {
				t.Fatal("stale numeric routing column")
			}
		}
		if tr.shared && n.sequence.Load()>>1&63 != uint64(n.n) {
			t.Fatal("stale shared count")
		}
		for i := int(n.n); i < nodeSize; i++ {
			if n.keyAt(i) != math.MaxUint64 || n.leaf && n.tags[i] != 0 {
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
			if n.inner().inline[i] != uint16(n.keyAt(i)>>n.shift) {
				t.Fatal("stale numeric inline summary")
			}
			if i >= int(n.n) {
				if n.inner().children[i] != 0 {
					t.Fatal("retained numeric child")
				}
				continue
			}
			child := n.inner().children[i]
			walk(child, level+1)
			// Separators bound their subtrees from below; an inner
			// node's first separator is its own.
			if c := tr.node(child); n.keyAt(i) > c.keyAt(0) || !c.leaf && n.keyAt(i) != c.keyAt(0) {
				t.Fatal("numeric separator above its subtree")
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
		case reflect.Int32, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Bool:
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
		tr.Clear()
		if tr.words != nil || tr.Len() != 0 {
			t.Fatal("numeric clear")
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
	// Each bucket names the deepest node below the root, down to the leaf
	// parents, that both its endpoints route through, or nothing.
	for bucket, id := range tr.routes.ids {
		low := tr.routes.base | uint64(bucket)<<tr.routes.shift
		high := low | ((uint64(1) << tr.routes.shift) - 1)
		var want uint32
		a, b := tr.root, tr.root
		for step := 0; step < int(tr.routes.depth) && a == b; step++ {
			n := tr.node(a)
			a, b = n.inner().children[routeScan(n, low)], n.inner().children[routeScan(n, high)]
			if a == b {
				want = a
			}
		}
		// Buckets above the highest one filled may stay empty.
		if id != want && (id != 0 || bucket <= tr.routesTop) {
			t.Fatalf("radix bucket %d [%x, %x] routes to %d, want %d", bucket, low, high, id, want)
		}
	}
}

// routeScan routes key through internal node n by a linear scan.
func routeScan(n *wordNode, key uint64) int {
	i := 0
	for i+1 < int(n.n) && n.keys[i+1] <= key {
		i++
	}
	return i
}

func TestConcurrentNumericLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 && unsafe.Sizeof(node[uint64, uint64, natural[uint64]]{}) > 512 {
		t.Fatal("numeric writer node exceeded the small-object scan threshold")
	}
	if unsafe.Offsetof(searchImage[uint64, uint64, natural[uint64]]{}.tags)+nodeSize > 64 {
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

func TestWordLeafSearchBounds(t *testing.T) {
	for distribution := 0; distribution < 6; distribution++ {
		for count := 0; count <= nodeSize; count++ {
			keys := make([]uint64, count)
			for i := range keys {
				switch distribution {
				case 0:
					keys[i] = uint64(i)
				case 1:
					keys[i] = math.MaxUint64 - uint64(i)
				case 2:
					keys[i] = uint64(i) << 47
				case 3:
					keys[i] = 0xa700000000000000 | uint64(i)<<16
				case 4:
					keys[i] = uint64(i)
					if i == count-1 {
						keys[i] = math.MaxUint64
					}
				case 5:
					keys[i] = mixedKey(uint64(i))
				}
			}
			slices.Sort(keys)
			n := &wordNode{leaf: true, n: uint8(count)}
			for i, key := range keys {
				n.entries()[i] = wordEntry{key: key}
			}
			(&wordTree{}).refit(n)
			probes := []uint64{0, math.MaxUint64}
			for _, key := range keys {
				probes = append(probes, key, key-1, key+1)
			}
			for i := 0; i < 100; i++ {
				probes = append(probes, mixedKey(uint64(i+100)))
			}
			for _, key := range probes {
				want := sort.Search(len(keys), func(i int) bool { return keys[i] >= key })
				if got := n.lowerEntry(key); got != want {
					t.Fatalf("distribution=%d count=%d key=%x lower=%d want=%d", distribution, count, key, got, want)
				}
				got := n.exactEntry(key)
				found := got < count && n.entries()[got].key == key
				if found != (want < count && keys[want] == key) {
					t.Fatalf("distribution=%d count=%d key=%x exact=%d", distribution, count, key, got)
				}
			}
		}
	}
}

func TestWordInternalRoutingBounds(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for distribution := 0; distribution < 6; distribution++ {
			for count := 1; count <= nodeSize; count++ {
				keys := make([]uint64, count)
				for i := range keys {
					switch distribution {
					case 0:
						keys[i] = uint64(i)
					case 1:
						keys[i] = math.MaxUint64 - uint64(i)
					case 2:
						keys[i] = uint64(i) << 47
					case 3:
						keys[i] = 0xa700000000000000 | uint64(i)<<16
					case 4:
						keys[i] = uint64(i)
						if i == count-1 {
							keys[i] = math.MaxUint64
						}
					case 5:
						keys[i] = mixedKey(uint64(i))
					}
				}
				slices.Sort(keys)
				n := &wordNode{n: uint8(count)}
				copy(n.keys[:], keys)
				tr := wordTree{shared: shared, atomicWrites: shared}
				tr.refit(n)
				probes := []uint64{0, math.MaxUint64}
				for _, key := range keys {
					probes = append(probes, key, key-1, key+1)
				}
				for i := 0; i < 100; i++ {
					probes = append(probes, mixedKey(uint64(i+100)))
				}
				for _, key := range probes {
					want := max(0, sort.Search(len(keys), func(i int) bool { return keys[i] > key })-1)
					directory := n.directory
					n.directory = 0
					got := tr.route(n, key)
					n.directory = directory
					if got != want {
						t.Fatalf("shared=%v distribution=%d count=%d key=%x inline=%d want=%d", shared, distribution, count, key, got, want)
					}
					if got := tr.route(n, key); got != want {
						t.Fatalf("shared=%v distribution=%d count=%d key=%x route=%d want=%d", shared, distribution, count, key, got, want)
					}
				}
			}
		}
	}
}

func TestRefillDirectoryMatchesFill(t *testing.T) {
	random := uint64(1)
	next := func() uint64 {
		random ^= random << 13
		random ^= random >> 7
		random ^= random << 17
		return random
	}
	for trial := 0; trial < 20000; trial++ {
		n := 3 + int(next()%(nodeSize-2))
		var codes [nodeSize]uint64
		for i := range codes {
			codes[i] = ^uint64(0)
		}
		for i := 0; i < n; i++ {
			codes[i] = next()
		}
		slices.Sort(codes[:n])
		var directory [256]uint8
		fillDirectory(&directory, &codes, n)
		first := 1 + int(next()%uint64(n-2))
		last := min(n-2, first+int(next()%2))
		// Replace interior codes while preserving order against their neighbors.
		low, high := codes[first-1], codes[last+1]
		for i := first; i <= last; i++ {
			codes[i] = low + (high-low)/uint64(last-first+2)*uint64(i-first+1)
		}
		refillDirectory(&directory, &codes, first, last)
		var want [256]uint8
		fillDirectory(&want, &codes, n)
		if directory != want {
			t.Fatalf("trial %d: n=%d first=%d last=%d directory mismatch", trial, n, first, last)
		}
	}
}

func TestWordLowerEntryMatchesBinarySearch(t *testing.T) {
	for count := 0; count <= nodeSize; count++ {
		var n wordNode
		n.leaf = true
		for i := range n.entries() {
			n.entries()[i] = wordEntry{key: ^uint64(0), value: uint64(i) * 0x9e3779b97f4a7c15}
		}
		for i := 0; i < count; i++ {
			n.entries()[i].key = uint64(10 + 10*i)
		}
		if count == nodeSize {
			n.entries()[count-1].key = ^uint64(0) - 1
		}
		n.n = uint8(count)
		probes := []uint64{0, 1, ^uint64(0), ^uint64(0) - 1, ^uint64(0) - 2}
		for i := 0; i < count; i++ {
			k := n.entries()[i].key
			probes = append(probes, k-1, k, k+1)
		}
		for _, key := range probes {
			want := 0
			for want < count && n.entries()[want].key < key {
				want++
			}
			if got := n.lowerEntry(key); got != want {
				t.Fatalf("count %d key %d: got %d want %d", count, key, got, want)
			}
		}
	}
}

func TestFillDirectoryMatchesBucketScan(t *testing.T) {
	random := uint64(7)
	for trial := 0; trial < 20000; trial++ {
		var codes [nodeSize]uint64
		n := int(random % (nodeSize + 1))
		for i := range codes {
			random ^= random << 13
			random ^= random >> 7
			random ^= random << 17
			codes[i] = random
			if trial%3 == 0 {
				codes[i] &= 0x07ff_ffff_ffff_ffff // crowd a few buckets
			}
		}
		slices.Sort(codes[:n])
		for i := n; i < nodeSize; i++ {
			codes[i] = ^uint64(0)
		}
		var got, want [256]uint8
		fillDirectory(&got, &codes, n)
		i := 0
		for bucket := range want {
			for i < n && codes[i] < uint64(bucket)<<56 {
				i++
			}
			want[bucket] = uint8(i)
		}
		if got != want {
			t.Fatalf("trial %d n=%d: directory mismatch", trial, n)
		}
	}
}

// Ascending runs, whole or in clusters, fill leaves rather than leaving each
// a third empty; random keys keep balanced splits.
func TestAscendingRunsFillLeaves(t *testing.T) {
	const n = 1 << 16
	fill := func(key func(uint64) uint64) (tree, concurrent float64) {
		var tr Tree[uint64, uint64]
		var m Map[uint64, uint64]
		for i := range uint64(n) {
			tr.Insert(key(i), i)
			m.Insert(key(i), i)
		}
		leaves := func(w *wordTree) (count int) {
			for id := w.end(false); id != 0; id = w.node(id).next {
				count++
			}
			return count
		}
		return float64(n) / float64(leaves(tr.words)*nodeSize), float64(n) / float64(leaves(&m.words.Load().tree)*nodeSize)
	}
	for _, c := range []struct {
		name string
		key  func(uint64) uint64
		min  float64
	}{
		{"sequential", func(i uint64) uint64 { return i << 1 }, 0.95},
		{"clustered", func(i uint64) uint64 { return mixedKey(i>>10)&^(1<<20-1) | (i&1023)<<1 }, 0.9},
		{"mixed", mixedKey, 0.7},
	} {
		tree, concurrent := fill(c.key)
		if tree < c.min || concurrent < c.min {
			t.Errorf("%s keys fill leaves %.0f%% (Tree) and %.0f%% (Map), want at least %.0f%%", c.name, 100*tree, 100*concurrent, 100*c.min)
		}
	}
}
