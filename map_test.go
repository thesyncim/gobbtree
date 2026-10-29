package gobbtree

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConcurrentMutation(t *testing.T) {
	var m Map[int, int]
	const workers = 8
	const perWorker = 1500
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Go(func() {
			for i := 0; i < perWorker; i++ {
				k := worker*perWorker + i
				m.Insert(k, k*3)
				if v, ok := m.Get(k); !ok || v != k*3 {
					t.Errorf("read own write %d", k)
				}
			}
			for i := 0; i < perWorker; i += 3 {
				m.Delete(worker*perWorker + i)
			}
		})
	}
	wg.Wait()
	if seqLen(m.All()) != workers*perWorker*2/3 {
		t.Fatal(seqLen(m.All()))
	}
	var snap []Pair[int, int]
	for k, v := range m.All() {
		snap = append(snap, Pair[int, int]{k, v})
	}
	if len(snap) != seqLen(m.All()) {
		t.Fatal("snapshot length")
	}
	for i, e := range snap {
		if e.Value != e.Key*3 || e.Key%perWorker%3 == 0 || (i > 0 && snap[i-1].Key >= e.Key) {
			t.Fatal("snapshot content")
		}
	}
	if words := m.words.Load(); words != nil {
		if got := checkLatchedTree(t, &words.tree); got != seqLen(m.All()) {
			t.Fatalf("tree holds %d entries, want %d", got, seqLen(m.All()))
		}
	} else if got := checkGenericTree(t, &m.generic); got != seqLen(m.All()) {
		t.Fatalf("tree holds %d entries, want %d", got, seqLen(m.All()))
	}
	// Same-key updates must not lose increments.
	m.Insert(-1, 0)
	for worker := 0; worker < workers; worker++ {
		wg.Go(func() {
			for i := 0; i < 1000; i++ {
				m.Update(-1, func(_, v int) int { return v + 1 })
			}
		})
	}
	wg.Wait()
	if v, _ := m.Get(-1); v != workers*1000 {
		t.Fatal("lost update", v)
	}
}
func TestConcurrentDuplicateInsert(t *testing.T) {
	const size, seeded = 32768, 4096
	const mask = uint64(0xa5b63728c9d04e1f)
	var m Map[uint64, uint64]
	for i := uint64(0); i < seeded; i++ {
		key := mixedKey(i)
		m.Insert(key, key^mask)
	}
	m.Get(mixedKey(0)) // Enable optimistic reads before concurrent mutation.
	var inserted atomic.Int64
	var writers sync.WaitGroup
	start := make(chan struct{})
	for worker := uint64(0); worker < 8; worker++ {
		writers.Go(func() {
			<-start
			for i := uint64(0); i < size; i++ {
				key := mixedKey((i*(2*worker+1) + worker*113) % size)
				value := key ^ mask
				old, replaced := m.Insert(key, value)
				if replaced {
					if old != value {
						t.Error("replacement returned an invalid previous value")
					}
				} else {
					inserted.Add(1)
					if old != 0 {
						t.Error("new entry returned a nonzero previous value")
					}
				}
				if got, ok := m.Get(key); !ok || got != value {
					t.Error("inserted value was lost")
				}
			}
		})
	}
	close(start)
	writers.Wait()
	if seqLen(m.All()) != size || inserted.Load() != size-seeded {
		t.Fatalf("duplicate insertion changed the count: len=%d new=%d", seqLen(m.All()), inserted.Load())
	}
	if got := checkMap(t, &m.mapCore); got != size {
		t.Fatalf("tree holds %d entries, want %d", got, size)
	}
}

func TestConcurrentReadsScansAndStructure(t *testing.T) {
	m := New[int, int]()
	for i := 0; i < 2000; i++ {
		m.Insert(i, i)
	}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Go(func() {
			for round := 0; round < 10; round++ {
				for i := w; i < 4000; i += 4 {
					m.Insert(i, i)
					m.Delete(i)
				}
			}
		})
	}
	for w := 0; w < 3; w++ {
		wg.Go(func() {
			dst := make([]Result[int], 4)
			for round := 0; round < 500; round++ {
				m.Get(round)
				m.GetMany([]int{0, 9, 1500, 3999}, dst)
				previous := -1
				visitRange(m.Range(0, 4000), func(k, v int) bool {
					if k <= previous || k != v {
						t.Error("invalid concurrent scan")
					}
					previous = k
					return k < 150
				})
				previous = 4001
				visitRange(m.RangeReverse(0, 4000), func(k, v int) bool {
					if k >= previous || k != v {
						t.Error("invalid concurrent reverse")
					}
					previous = k
					return k > 3900
				})
			}
		})
	}
	wg.Wait()
	if words := m.words.Load(); words != nil {
		if got := checkLatchedTree(t, &words.tree); got != seqLen(m.All()) {
			t.Fatalf("tree holds %d entries, want %d", got, seqLen(m.All()))
		}
	} else if got := checkGenericTree(t, &m.generic); got != seqLen(m.All()) {
		t.Fatalf("tree holds %d entries, want %d", got, seqLen(m.All()))
	}
}
func TestMapCallbacksAndBatch(t *testing.T) {
	var m Map[string, int]
	actual, loaded := m.LoadOrStore("a", 1)
	if actual != 1 || loaded {
		t.Fatal("load store")
	}
	actual, loaded = m.LoadOrStore("a", 2)
	if actual != 1 || !loaded {
		t.Fatal("load store existing")
	}
	m.UpdateOrInsert("a", 4, func(k string, a, b int) int {
		if k != "a" {
			t.Fatal("merge key")
		}
		return a + b
	})
	m.UpdateOrInsert("b", 7, func(_ string, a, b int) int { t.Fatal("merge on absent"); return 0 })
	if !m.Read("a", func(k string, v int) {
		if k != "a" || v != 5 {
			t.Fatal("read callback")
		}
	}) {
		t.Fatal("read")
	}
	if _, _, ok := m.DeleteIf("a", func(_ string, v int) bool { return v == 9 }); ok {
		t.Fatal("delete predicate")
	}
	if k, v, ok := m.DeleteIf("a", func(k string, v int) bool { return k == "a" && v == 5 }); !ok || k != "a" || v != 5 {
		t.Fatal("delete if")
	}
	for _, call := range []func(){
		func() { m.Read("b", func(string, int) { panic("test") }) },
		func() { m.Update("b", func(string, int) int { panic("test") }) },
		func() { m.UpdateOrInsert("b", 1, func(string, int, int) int { panic("test") }) },
		func() { m.DeleteIf("b", func(string, int) bool { panic("test") }) },
		func() { visitRange(m.Range("a", "z"), func(string, int) bool { panic("test") }) },
		func() {
			for range m.All() {
				panic("test")
			}
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("missing panic")
				}
			}()
			call()
		}()
		if v, ok := m.Get("b"); !ok || v != 7 {
			t.Fatal("panic mutated value")
		}
	}
	dst := make([]Result[int], 3)
	m.InsertMany([]Pair[string, int]{{"x", 1}, {"x", 2}, {"z", 9}}, dst)
	if dst[1] != (Result[int]{1, true}) {
		t.Fatal(dst)
	}
	m.GetMany([]string{"b", "x", "absent"}, dst)
	if !slices.Equal(dst, []Result[int]{{7, true}, {2, true}, {0, false}}) {
		t.Fatal(dst)
	}
	count := 0
	for range m.All() {
		count++
	}
	if count != seqLen(m.All()) {
		t.Fatal("all")
	}
	k, _, ok := m.PopFirst()
	if !ok || k != "b" {
		t.Fatal("pop first")
	}
	k, _, ok = m.PopLast()
	if !ok || k != "z" {
		t.Fatal("pop last")
	}
	m.Clear()
	if !m.IsEmpty() {
		t.Fatal("clear")
	}
}

// Upserts, conditional deletes and LoadOrStore on non-numeric keys run under
// leaf latches beside structural writers; no increment or removal may be
// lost.
func TestMapLeafReadModifyWriteConcurrent(t *testing.T) {
	var m Map[string, int]
	const keys, workers, rounds = 300, 6, 40
	key := func(i int) string { return fmt.Sprintf("k%05d", i*7) }
	add := func(_ string, old, value int) int { return old + value }
	var wg sync.WaitGroup
	var stored, removed atomic.Int64
	for w := range workers {
		wg.Go(func() {
			for r := range rounds {
				for i := range keys {
					m.UpdateOrInsert(key((i+w*31+r)%keys), 1, add)
				}
			}
		})
	}
	// Splits and merges around the counters.
	wg.Go(func() {
		for r := range rounds * 4 {
			for i := range keys {
				churn := fmt.Sprintf("k%05d", i*7+1+r%5)
				if _, loaded := m.LoadOrStore(churn, -1); !loaded {
					stored.Add(1)
				}
				if _, _, ok := m.DeleteIf(churn, func(k string, v int) bool { return k == churn && v == -1 }); ok {
					removed.Add(1)
				}
			}
		}
	})
	wg.Wait()
	for i := range keys {
		if v, _ := m.Get(key(i)); v != workers*rounds {
			t.Fatalf("%s = %d, want %d", key(i), v, workers*rounds)
		}
	}
	if stored.Load() != removed.Load() || seqLen(m.All()) != keys {
		t.Fatalf("stored %d removed %d len %d", stored.Load(), removed.Load(), seqLen(m.All()))
	}
}

// The numeric map's upserts and conditional deletes finish under leaf
// latches too, before and after readers make writers lane-free.
func TestNumericMapLeafReadModifyWriteConcurrent(t *testing.T) {
	for _, readers := range []bool{false, true} {
		var m Map[uint64, int64]
		const keys, workers, rounds = 300, 6, 40
		key := func(i int) uint64 { return uint64(i) * 7 }
		add := func(_ uint64, old, value int64) int64 { return old + value }
		if readers {
			m.Insert(1<<40, 0)
			m.Get(1 << 40)
			m.Delete(1 << 40)
		}
		var wg sync.WaitGroup
		var stored, removed atomic.Int64
		for w := range workers {
			wg.Go(func() {
				for r := range rounds {
					for i := range keys {
						m.UpdateOrInsert(key((i+w*31+r)%keys), 1, add)
						if readers {
							m.Get(key(i))
						}
					}
				}
			})
		}
		wg.Go(func() {
			for r := range rounds * 4 {
				for i := range keys {
					churn := key(i) + 1 + uint64(r%5)
					if _, loaded := m.LoadOrStore(churn, -1); !loaded {
						stored.Add(1)
					}
					if _, _, ok := m.DeleteIf(churn, func(k uint64, v int64) bool { return k == churn && v == -1 }); ok {
						removed.Add(1)
					}
				}
			}
		})
		wg.Wait()
		for i := range keys {
			if v, _ := m.Get(key(i)); v != workers*rounds {
				t.Fatalf("readers %v: %d = %d, want %d", readers, key(i), v, workers*rounds)
			}
		}
		if stored.Load() != removed.Load() || seqLen(m.All()) != keys {
			t.Fatalf("readers %v: stored %d removed %d len %d", readers, stored.Load(), removed.Load(), seqLen(m.All()))
		}
	}
}
