package gobbtree

import (
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
	if m.Len() != workers*perWorker*2/3 {
		t.Fatal(m.Len())
	}
	snap := m.Snapshot()
	if len(snap) != m.Len() {
		t.Fatal("snapshot length")
	}
	for i, e := range snap {
		if e.Value != e.Key*3 || e.Key%perWorker%3 == 0 || (i > 0 && snap[i-1].Key >= e.Key) {
			t.Fatal("snapshot content")
		}
	}
	if words := m.words.Load(); words != nil {
		checkWordTree(t, &words.tree, m.Len())
	} else {
		m.tree.length = m.Len()
		checkTree(t, &m.tree)
	}
	// Same-key updates must not lose increments.
	m.Insert(-1, 0)
	for worker := 0; worker < workers; worker++ {
		wg.Go(func() {
			for i := 0; i < 1000; i++ {
				m.Update(-1, func(v int) int { return v + 1 })
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
	if m.Len() != size || inserted.Load() != size-seeded {
		t.Fatalf("duplicate insertion changed the count: len=%d new=%d", m.Len(), inserted.Load())
	}
	checkWordTree(t, &m.words.Load().tree, size)
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
				m.Scan(0, 4000, func(k, v int) bool {
					if k <= previous || k != v {
						t.Error("invalid concurrent scan")
					}
					previous = k
					return k < 150
				})
				previous = 4001
				m.ScanReverse(0, 4000, func(k, v int) bool {
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
		checkWordTree(t, &words.tree, m.Len())
	} else {
		m.tree.length = m.Len()
		checkTree(t, &m.tree)
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
	m.UpdateOrInsert("a", 4, func(a, b int) int { return a + b })
	m.UpdateOrInsert("b", 7, func(a, b int) int { t.Fatal("merge on absent"); return 0 })
	if !m.Read("a", func(k string, v int) {
		if k != "a" || v != 5 {
			t.Fatal("read callback")
		}
	}) {
		t.Fatal("read")
	}
	if _, ok := m.DeleteIf("a", func(v int) bool { return v == 9 }); ok {
		t.Fatal("delete predicate")
	}
	if v, ok := m.DeleteIf("a", func(v int) bool { return v == 5 }); !ok || v != 5 {
		t.Fatal("delete if")
	}
	for _, call := range []func(){
		func() { m.Read("b", func(string, int) { panic("test") }) },
		func() { m.Update("b", func(int) int { panic("test") }) },
		func() { m.UpdateOrInsert("b", 1, func(int, int) int { panic("test") }) },
		func() { m.DeleteIf("b", func(int) bool { panic("test") }) },
		func() { m.Scan("a", "z", func(string, int) bool { panic("test") }) },
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
	m.InsertMany([]Entry[string, int]{{"x", 1}, {"x", 2}, {"z", 9}}, dst)
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
	if count != m.Len() {
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
