package gobbtree

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// nodeRoutedKey is a defined uint64. The word engine accepts only built-in
// word types, so maps of it run on the generic engine.
type nodeRoutedKey uint64

// checkGenericTree checks t's structure, at rest, and returns its entries:
// every node is unheld; leaves are at one depth; counts keep their minimums
// below the root; keys ascend within their fences; separator codes are
// their windows; an internal node's own separator is its parent's; and
// slots past a count hold nothing.
func checkGenericTree[K any, V any, O keyOrder[K]](t testing.TB, tree *genericTree[K, V, O]) int {
	t.Helper()
	root := tree.root.Load()
	if root == nil {
		return 0
	}
	leafDepth := -1
	var check func(h *gheader, low, high *K, depth int) int
	check = func(h *gheader, low, high *K, depth int) int {
		if word := h.latch.Load(); word&(latchExclusive|latchSharedMask) != 0 {
			t.Fatalf("node held at rest: %x", word)
		}
		count := int(h.count.Load())
		if count > nodeSize {
			t.Fatalf("count %d", count)
		}
		inside := func(k K) {
			if low != nil && compareKeys[O](k, *low) < 0 || high != nil && compareKeys[O](k, *high) >= 0 {
				t.Fatalf("key %v outside its fences", k)
			}
		}
		if h.leaf {
			if leafDepth < 0 {
				leafDepth = depth
			} else if depth != leafDepth {
				t.Fatalf("leaf at depth %d, others at %d", depth, leafDepth)
			}
			if h != root && count < leafMinLen {
				t.Fatalf("leaf holds %d entries", count)
			}
			n := asLeaf[K, V](h)
			for i := 0; i < count; i++ {
				inside(n.keys[i])
				if n.codes[i] != windowKey[O](n.keys[i], 0) {
					t.Fatalf("leaf code %d is stale", i)
				}
				if i > 0 && compareKeys[O](n.keys[i-1], n.keys[i]) >= 0 {
					t.Fatalf("leaf keys out of order at %d", i)
				}
			}
			for i := count; i < nodeSize; i++ {
				if n.codes[i] != codePad || !reflect.ValueOf(&n.keys[i]).Elem().IsZero() || !reflect.ValueOf(&n.values[i]).Elem().IsZero() {
					t.Fatalf("leaf slot %d past count %d holds an entry", i, count)
				}
			}
			return count
		}
		n := asInner[K](h)
		if h == root && count < 2 || h != root && count < innerMinLen {
			t.Fatalf("internal node holds %d children", count)
		}
		if own := n.seps[0].Load(); (own == nil) != (low == nil) || own != nil && compareKeys[O](*own, *low) != 0 {
			t.Fatal("internal node's own separator is not its parent's")
		}
		total := 0
		for i := 0; i < count; i++ {
			sep := n.seps[i].Load()
			if i > 0 {
				if sep == nil {
					t.Fatalf("separator %d missing", i)
				}
				if n.codes[i].Load() != windowKey[O](*sep, 0) {
					t.Fatalf("separator %d code is stale", i)
				}
				inside(*sep)
				if prev := n.seps[i-1].Load(); prev != nil && compareKeys[O](*prev, *sep) >= 0 {
					t.Fatalf("separators out of order at %d", i)
				}
			}
			childLow, childHigh := low, high
			if i > 0 {
				childLow = sep
			}
			if i+1 < count {
				childHigh = n.seps[i+1].Load()
			}
			child := n.children[i].Load()
			if child == nil {
				t.Fatalf("child %d missing", i)
			}
			total += check(child, childLow, childHigh, depth+1)
		}
		for i := count; i < nodeSize; i++ {
			if n.seps[i].Load() != nil || n.children[i].Load() != nil || n.codes[i].Load() != codePad {
				t.Fatalf("internal slot %d past count %d holds a child", i, count)
			}
		}
		return total
	}
	return check(root, nil, nil, 0)
}

// genericKey spreads keys over lengths around the eight-byte window, with
// long shared prefixes, so codes tie and separators are compared whole.
func genericKey(i uint64) string {
	switch i % 4 {
	case 0:
		return fmt.Sprintf("%x", i)
	case 1:
		return fmt.Sprintf("user:%08x", i)
	case 2:
		return fmt.Sprintf("user:0000%x", i)
	}
	return fmt.Sprintf("user:00000000:%016x:tail", mixedKey(i))
}

func TestGenericChurn(t *testing.T) {
	type value struct {
		key  string
		hits []int
	}
	var m Map[string, value]
	const keys, workers = 4096, 8
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Go(func() {
			// Each worker owns the keys equal to it modulo workers, so its
			// model of them is exact.
			present := map[uint64]bool{}
			x := uint64(worker)*0x9e3779b97f4a7c15 + 1
			for round := 0; round < rounds; round++ {
				for j := 0; j < keys; j++ {
					x = x*6364136223846793005 + 1442695040888963407
					i := uint64(worker) + workers*((x>>33)%(keys/workers))
					k := genericKey(i)
					switch x >> 61 {
					case 0, 1:
						if _, ok := m.Delete(k); ok != present[i] {
							t.Errorf("delete %q found %v, want %v", k, ok, present[i])
						}
						present[i] = false
					case 2:
						if _, v, ok := m.Take(k); ok != present[i] || ok && v.key != k {
							t.Errorf("took %q: %v holding %q, want %v", k, ok, v.key, present[i])
						}
						present[i] = false
					case 3:
						if _, ok := m.UpdateOrInsert(k, value{k, []int{1}}, func(_ string, old, _ value) value {
							return value{old.key, append(old.hits[:len(old.hits):len(old.hits)], 1)}
						}); ok != present[i] {
							t.Errorf("upsert %q found %v, want %v", k, ok, present[i])
						}
						present[i] = true
					default:
						if _, ok := m.Insert(k, value{k, []int{0}}); ok != present[i] {
							t.Errorf("insert %q found %v, want %v", k, ok, present[i])
						}
						present[i] = true
					}
					if v, ok := m.Get(k); ok != present[i] || ok && v.key != k {
						t.Errorf("get %q: %v holding %q, want %v", k, ok, v.key, present[i])
					}
					if v, ok := m.Get(genericKey((x >> 20) % keys)); ok && v.key != genericKey((x>>20)%keys) {
						t.Errorf("read %q holding %q", genericKey((x>>20)%keys), v.key)
					}
				}
			}
		})
	}
	wg.Go(func() {
		for round := 0; round < rounds; round++ {
			var last string
			first := true
			for k, v := range m.All() {
				if !first && k <= last || v.key != k {
					t.Errorf("scan: %q after %q holding %q", k, last, v.key)
					return
				}
				last, first = k, false
			}
		}
	})
	wg.Wait()
	if got, want := checkGenericTree(t, &m.generic), seqLen(m.All()); got != want {
		t.Fatalf("tree holds %d entries, scan %d", got, want)
	}
}

// Filling and draining in order grows and shrinks the tree through every
// height, splitting and merging at the borders.
func TestGenericFillAndDrain(t *testing.T) {
	var m Map[string, int]
	const n = 20000
	for _, order := range []string{"ascending", "descending", "mixed"} {
		key := func(i int) int {
			switch order {
			case "descending":
				return n - 1 - i
			case "mixed":
				return int(mixedKey(uint64(i)) % n)
			}
			return i
		}
		present := map[int]bool{}
		for i := 0; i < n; i++ {
			k := key(i)
			m.Insert(genericKey(uint64(k)), k)
			present[k] = true
		}
		if got := checkGenericTree(t, &m.generic); got != len(present) {
			t.Fatalf("%s fill: %d entries, want %d", order, got, len(present))
		}
		removed := 0
		for i := 0; i < n && removed < len(present); i++ {
			if i%2 == 0 {
				if _, _, ok := m.PopFirst(); !ok {
					t.Fatalf("%s: PopFirst found nothing with %d left", order, len(present)-removed)
				}
				removed++
			} else if _, ok := m.Delete(genericKey(uint64(key(i)))); ok {
				removed++
			}
			if i%997 == 0 {
				if got := checkGenericTree(t, &m.generic); got != len(present)-removed {
					t.Fatalf("%s drain: %d entries, want %d", order, got, len(present)-removed)
				}
			}
		}
		for {
			if _, _, ok := m.PopLast(); !ok {
				break
			}
		}
		if !m.IsEmpty() || checkGenericTree(t, &m.generic) != 0 {
			t.Fatalf("%s: not empty after draining", order)
		}
	}
}

// Lookups of word keys copy values without latching their leaf; a copy torn
// by a writer must never be returned. Values here span many words, with
// pointers and an interface, and each is consistent only whole.
func TestGenericOptimisticValues(t *testing.T) {
	type wide struct {
		id    uint64
		name  *string
		check []uint64
		any   any
		pad   [6]uint64
		self  uint64
	}
	make := func(k, gen uint64) wide {
		name := fmt.Sprint(k, ":", gen)
		w := wide{id: k, name: &name, check: []uint64{k, gen}, any: gen, self: k ^ gen}
		for i := range w.pad {
			w.pad[i] = gen
		}
		return w
	}
	consistent := func(k uint64, w wide) bool {
		gen := w.self ^ k
		if w.id != k || *w.name != fmt.Sprint(k, ":", gen) || len(w.check) != 2 || w.check[0] != k || w.check[1] != gen || w.any.(uint64) != gen {
			return false
		}
		for _, p := range w.pad {
			if p != gen {
				return false
			}
		}
		return true
	}
	var m Map[uint64, wide]
	const keys = 2048
	for k := uint64(0); k < keys; k++ {
		m.Insert(k, make(k, 0))
	}
	ops := 200000
	if testing.Short() {
		ops = 20000
	}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Go(func() {
			x := uint64(w + 1)
			for i := 0; i < ops; i++ {
				x = x*6364136223846793005 + 1442695040888963407
				k := (x >> 33) % keys
				if x>>62 == 0 {
					m.Delete(k)
				}
				m.Insert(k, make(k, x>>40))
			}
		})
	}
	for r := 0; r < 4; r++ {
		wg.Go(func() {
			x := uint64(r + 100)
			for i := 0; i < 2*ops; i++ {
				x = x*6364136223846793005 + 1442695040888963407
				k := (x >> 33) % keys
				if v, ok := m.Get(k); ok && !consistent(k, v) {
					t.Errorf("key %d: torn value %+v", k, v)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := checkGenericTree(t, &m.generic); got != seqLen(m.All()) {
		t.Fatalf("tree holds %d entries, scan %d", got, seqLen(m.All()))
	}
}
