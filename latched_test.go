package gobbtree

import (
	"iter"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

// seqLen counts the entries of seq, for maps without Len.
func seqLen[K, V any](seq iter.Seq2[K, V]) int {
	n := 0
	for range seq {
		n++
	}
	return n
}

// checkLatchedTree checks a latched tree no other goroutine changes, as
// upstream's check_invariants, and returns its entry count: keys sorted
// within the bounds their separators give, padding past each count, tags,
// minimum occupancy below the root, every latch let go, and the last leaf.
// checkMap checks m's tree and returns its entry count.
func checkMap[K any, V any, O keyOrder[K]](t testing.TB, m *mapCore[K, V, O]) int {
	t.Helper()
	return checkGenericTree(t, &m.generic)
}

// TestLatchedChurn runs writers that own disjoint keys through inserts,
// replacements and removals that split and merge leaves, grow and lower the
// root, beside readers and scans that check every value they see; then the
// tree must hold exactly each writer's model and its invariants.
func TestLatchedChurn(t *testing.T) {
	keyings := map[string]func(uint64) uint64{
		"sequential": func(i uint64) uint64 { return i },
		"clustered":  func(i uint64) uint64 { return mixedKey(i>>6)&^(1<<16-1) | i&63 },
		"mixed":      mixedKey,
	}
	for name, keyAt := range keyings {
		t.Run(name, func(t *testing.T) {
			const writers, span, rounds = 4, 3000, 40000
			m := New[uint64, uint64]()
			value := func(key, gen uint64) uint64 { return key*31 + gen<<40 }
			models := make([]map[uint64]uint64, writers)
			var stop atomic.Bool
			var readers, wg sync.WaitGroup
			for r := 0; r < 3; r++ {
				readers.Go(func() {
					rng := rand.New(rand.NewPCG(uint64(r), 7))
					for !stop.Load() {
						key := keyAt(rng.Uint64N(span * writers))
						if v, ok := m.Get(key); ok && (v&(1<<40-1)) != key*31&(1<<40-1) {
							t.Errorf("get %d = %#x", key, v)
							return
						}
						if r == 0 {
							var last uint64
							n := 0
							for k, v := range m.Scan(AtLeast(key)) {
								if n > 0 && k <= last || (v&(1<<40-1)) != k*31&(1<<40-1) {
									t.Errorf("scan from %d: %d after %d, value %#x", key, k, last, v)
									return
								}
								last = k
								if n++; n == 70 {
									break
								}
							}
						}
					}
				})
			}
			for w := 0; w < writers; w++ {
				models[w] = make(map[uint64]uint64)
				wg.Go(func() {
					rng := rand.New(rand.NewPCG(uint64(w), 11))
					model := models[w]
					for i := 0; i < rounds; i++ {
						key := keyAt(rng.Uint64N(span)*writers + uint64(w))
						switch op := rng.IntN(10); {
						case op < 5:
							old, found := m.Insert(key, value(key, uint64(i)))
							want, had := model[key]
							if found != had || found && old != want {
								t.Errorf("insert %d: %#x %v, model %#x %v", key, old, found, want, had)
								return
							}
							model[key] = value(key, uint64(i))
						case op < 9:
							old, found := m.Delete(key)
							want, had := model[key]
							if found != had || found && old != want {
								t.Errorf("delete %d: %#x %v, model %#x %v", key, old, found, want, had)
								return
							}
							delete(model, key)
						default:
							v, found := m.Get(key)
							want, had := model[key]
							if found != had || found && v != want {
								t.Errorf("get %d: %#x %v, model %#x %v", key, v, found, want, had)
								return
							}
						}
					}
				})
			}
			wg.Wait()
			stop.Store(true)
			readers.Wait()
			want := 0
			for _, model := range models {
				want += len(model)
				for k, v := range model {
					if got, ok := m.Get(k); !ok || got != v {
						t.Fatalf("key %d: %#x %v, want %#x", k, got, ok, v)
					}
				}
			}
			if got := checkMap(t, &m.mapCore); got != want {
				t.Fatalf("tree holds %d entries, models %d", got, want)
			}
			if got := seqLen(m.All()); got != want {
				t.Fatalf("All visits %d entries, models %d", got, want)
			}
		})
	}
}

// TestLatchedFillAndDrain fills and empties the tree concurrently, so that
// leaves merge and move entries, internal levels merge and the root lowers
// to a leaf, while readers check values and pops take the ends.
func TestLatchedFillAndDrain(t *testing.T) {
	for _, keyAt := range []func(uint64) uint64{func(i uint64) uint64 { return i }, mixedKey} {
		const writers, span = 4, 20000
		m := New[uint64, uint64]()
		var stop atomic.Bool
		var readers sync.WaitGroup
		for r := 0; r < 2; r++ {
			readers.Go(func() {
				for i := uint64(0); !stop.Load(); i++ {
					key := keyAt(i % (span * writers))
					if v, ok := m.Get(key); ok && v != ^key {
						t.Errorf("get %d = %#x", key, v)
						return
					}
				}
			})
		}
		for round := 0; round < 3; round++ {
			var wg sync.WaitGroup
			for w := 0; w < writers; w++ {
				wg.Go(func() {
					for i := uint64(0); i < span; i++ {
						key := keyAt(i*writers + uint64(w))
						if _, found := m.Insert(key, ^key); found {
							t.Errorf("insert %d found it", key)
							return
						}
					}
				})
			}
			wg.Wait()
			if got := checkMap(t, &m.mapCore); got != span*writers {
				t.Fatalf("filled tree holds %d", got)
			}
			var popped atomic.Int64
			for w := 0; w < writers; w++ {
				wg.Go(func() {
					rng := rand.New(rand.NewPCG(uint64(w), uint64(round)))
					order := rng.Perm(span)
					for _, i := range order {
						key := keyAt(uint64(i)*writers + uint64(w))
						if v, found := m.Delete(key); !found {
							// A pop took it.
							continue
						} else if v != ^key {
							t.Errorf("delete %d = %#x", key, v)
							return
						}
						if w == 0 && i%97 == 0 {
							if k, v, ok := m.PopFirst(); ok {
								if v != ^k {
									t.Errorf("pop %d = %#x", k, v)
								}
								popped.Add(1)
							}
							if k, v, ok := m.PopLast(); ok {
								if v != ^k {
									t.Errorf("pop %d = %#x", k, v)
								}
								popped.Add(1)
							}
						}
					}
				})
			}
			wg.Wait()
			if got := checkMap(t, &m.mapCore); got != 0 {
				t.Fatalf("drained tree holds %d", got)
			}
			if root := m.generic.root.Load(); root != nil && !root.leaf || !m.IsEmpty() {
				t.Fatal("drained tree's root is not an empty leaf")
			}
		}
		stop.Store(true)
		readers.Wait()
	}
}

// TestLatchedHintsThroughStructure fills the route hints, then changes the
// structure under them: removals that merge leaves and lower the root, and
// inserts that split them and grow it. Every key must read, replace and
// remove correctly through hints made stale, for every key distribution.
func TestLatchedHintsThroughStructure(t *testing.T) {
	keyings := map[string]func(uint64) uint64{
		"sequential": func(i uint64) uint64 { return i * 2 },
		"clustered":  func(i uint64) uint64 { return mixedKey(i>>10)&^(1<<20-1) | (i&1023)<<1 },
		"mixed":      mixedKey,
	}
	for name, keyAt := range keyings {
		t.Run(name, func(t *testing.T) {
			const n = 60000
			m := New[uint64, uint64]()
			want := map[uint64]uint64{}
			check := func(stage string) {
				t.Helper()
				for i := uint64(0); i < n; i++ {
					k := keyAt(i)
					v, ok := m.Get(k)
					w, had := want[k]
					if ok != had || v != w {
						t.Fatalf("%s: get %d = %d %v, want %d %v", stage, k, v, ok, w, had)
					}
				}
				keys := make([]uint64, 0, 64)
				dst := make([]Result[uint64], 64)
				for i := uint64(0); i+64 <= n; i += 64 {
					keys = keys[:0]
					for j := uint64(0); j < 64; j++ {
						keys = append(keys, keyAt((i+j)*7919%n))
					}
					m.GetMany(keys, dst)
					for j, k := range keys {
						w, had := want[k]
						if dst[j].OK != had || dst[j].Value != w {
							t.Fatalf("%s: GetMany %d = %v, want %d %v", stage, k, dst[j], w, had)
						}
					}
				}
				if got := checkMap(t, &m.mapCore); got != len(want) {
					t.Fatalf("%s: tree holds %d, want %d", stage, got, len(want))
				}
			}
			for i := uint64(0); i < n; i++ {
				m.Insert(keyAt(i), i)
				want[keyAt(i)] = i
			}
			check("built")
			// Replace through the hints.
			for i := uint64(0); i < n; i += 3 {
				k := keyAt(i)
				if old, ok := m.Insert(k, ^i); !ok || old != want[k] {
					t.Fatalf("replace %d: %d %v", k, old, ok)
				}
				want[k] = ^i
			}
			check("replaced")
			// Remove most keys: leaves merge, levels go, the root lowers.
			for i := uint64(0); i < n; i++ {
				if i%17 == 0 {
					continue
				}
				k := keyAt(i)
				if old, ok := m.Delete(k); !ok || old != want[k] {
					t.Fatalf("delete %d: %d %v", k, old, ok)
				}
				delete(want, k)
			}
			check("drained")
			// Refill in another order: leaves split and the root grows.
			for i := uint64(n); i > 0; i-- {
				k := keyAt(i - 1)
				_, had := want[k]
				if _, ok := m.Insert(k, i); ok != had {
					t.Fatalf("refill %d: replaced %v, had %v", k, ok, had)
				}
				want[k] = i
			}
			check("refilled")
		})
	}
}

// TestTreeHintsRandomized runs random inserts, deletes and lookups on a Tree
// against a model, checking the tree as hints go stale.
func TestTreeHintsRandomized(t *testing.T) {
	tr := NewTree[uint64, uint64]()
	ref := map[uint64]uint64{}
	rng := rand.New(rand.NewPCG(31, 41))
	for i := 0; i < 200000; i++ {
		k := rng.Uint64N(3000)
		switch op := rng.IntN(10); {
		case op < 5 || i < 3000:
			old, ok := tr.Insert(k, uint64(i))
			if w, had := ref[k]; ok != had || ok && old != w {
				t.Fatalf("op %d: insert %d = %d %v, want %d %v", i, k, old, ok, w, had)
			}
			ref[k] = uint64(i)
		case op < 8:
			old, ok := tr.Delete(k)
			if w, had := ref[k]; ok != had || ok && old != w {
				t.Fatalf("op %d: delete %d = %d %v, want %d %v", i, k, old, ok, w, had)
			}
			delete(ref, k)
		default:
			v, ok := tr.Get(k)
			if w, had := ref[k]; ok != had || v != w {
				t.Fatalf("op %d: get %d = %d %v, want %d %v", i, k, v, ok, w, had)
			}
		}
		if i%1000 == 0 {
			checkTree(t, tr)
		}
	}
}
