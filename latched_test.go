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
func checkLatchedTree(t testing.TB, tr *latchedTree) int {
	t.Helper()
	root := tr.root.Load()
	if root == 0 {
		return 0
	}
	entries, last := 0, uint32(0)
	var walk func(id uint32, low, high uint64, bounded bool, isRoot bool)
	walk = func(id uint32, low, high uint64, bounded bool, isRoot bool) {
		n := tr.node(id)
		if n == nil {
			t.Fatalf("node %#x outside the arena", id)
		}
		if word := n.sequence.Load(); word&(latchExclusive|latchSharedMask) != 0 {
			t.Fatalf("node %#x still held: %#x", id, word)
		}
		count := int(n.n)
		if id&wordLeafID != 0 {
			if !n.leaf || count > nodeSize || !isRoot && count < leafMinLen {
				t.Fatalf("leaf %#x: %d entries", id, count)
			}
			for i := 0; i < nodeSize; i++ {
				key := n.entries()[i].key
				if i >= count {
					if key != ^uint64(0) {
						t.Fatalf("leaf %#x slot %d not padded", id, i)
					}
					continue
				}
				if n.tags[i] != uint8(key) {
					t.Fatalf("leaf %#x tag %d", id, i)
				}
				if i > 0 && n.entries()[i-1].key >= key || key < low || bounded && key >= high {
					t.Fatalf("leaf %#x key %d out of order or bounds [%d, %d)", id, key, low, high)
				}
			}
			entries += count
			last = id
			return
		}
		if n.leaf || count > nodeSize || count < 2 || !isRoot && count < innerMinLen {
			t.Fatalf("internal node %#x: %d children", id, count)
		}
		if !isRoot && n.keys[0] != low {
			t.Fatalf("internal node %#x first separator %d, its own %d", id, n.keys[0], low)
		}
		for i := 0; i < count; i++ {
			childLow, childHigh, childBounded := low, high, bounded
			if i > 0 {
				childLow = n.keys[i]
				if n.keys[i] <= n.keys[i-1] && i > 1 || n.keys[i] < low || bounded && n.keys[i] >= high {
					t.Fatalf("internal node %#x separator %d out of order", id, i)
				}
			}
			if i+1 < count {
				childHigh, childBounded = n.keys[i+1], true
			}
			walk(n.inner().children[i], childLow, childHigh, childBounded, false)
		}
		for i := count; i < nodeSize; i++ {
			if n.keys[i] != ^uint64(0) || n.inner().children[i] != 0 {
				t.Fatalf("internal node %#x slot %d not cleared", id, i)
			}
		}
	}
	walk(root, 0, 0, false, true)
	if got := tr.rightmost.Load(); got != last {
		t.Fatalf("appends go to %#x, the last leaf is %#x", got, last)
	}
	return entries
}

// latchedLeaves counts a quiescent latched tree's leaves.
func latchedLeaves(tr *latchedTree) int {
	var count func(id uint32) int
	count = func(id uint32) int {
		if id&wordLeafID != 0 {
			return 1
		}
		n, total := tr.node(id), 0
		for i := 0; i < int(n.n); i++ {
			total += count(n.inner().children[i])
		}
		return total
	}
	if root := tr.root.Load(); root != 0 {
		return count(root)
	}
	return 0
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
			if got := checkLatchedTree(t, &m.words.Load().tree); got != want {
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
			if got := checkLatchedTree(t, &m.words.Load().tree); got != span*writers {
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
			tr := &m.words.Load().tree
			if got := checkLatchedTree(t, tr); got != 0 {
				t.Fatalf("drained tree holds %d", got)
			}
			if tr.root.Load()&wordLeafID == 0 || !m.IsEmpty() {
				t.Fatal("drained tree's root is not an empty leaf")
			}
		}
		stop.Store(true)
		readers.Wait()
	}
}
