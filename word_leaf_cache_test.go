package gobbtree

import (
	"math"
	"sync"
	"testing"
)

func TestNumericLeafRouteCache(t *testing.T) {
	for _, shift := range []uint{0, 32, 48} {
		var m Map[uint64, uint64]
		const size = 20000
		reference := make(map[uint64]bool)
		for i := uint64(0); i < size; i++ {
			key := mixedKey(i) >> shift
			m.Insert(key, key)
			reference[key] = true
		}
		m.Get(0)
		words := m.words.Load()
		words.structure.Lock()
		view := *words.view.Load()
		if view.uncached == nil {
			view.uncached = words.view.Load()
		}
		view.cache = nil
		view.leaves = newWordLeafCache(view.routes, int64(m.Len()))
		words.view.Store(&view)
		words.structure.Unlock()
		if view.leaves == nil {
			t.Fatal("test did not reach a cached parent level")
		}
		var readers sync.WaitGroup
		for worker := uint64(0); worker < 4; worker++ {
			readers.Go(func() {
				var keys [8]uint64
				var values [8]Result[uint64]
				for i := worker; i < 2*size; i += 4 {
					key := mixedKey(i%size) >> shift
					if worker == 0 {
						m.Insert(key, key^((i/4)&1))
					}
					for j := range keys {
						index := (i + uint64(j)*4) % size
						if j == 7 {
							index += size
						}
						keys[j] = mixedKey(index) >> shift
					}
					m.GetMany(keys[:], values[:])
					for j, result := range values {
						if result.OK != reference[keys[j]] || result.OK && result.Value&^1 != keys[j]&^1 {
							t.Error("cached batch during value replacement")
							return
						}
					}
				}
			})
		}
		readers.Wait()
		used := 0
		for index := range view.leaves.cells {
			cell := &view.leaves.cells[index]
			if cell.head.Load() <= 2 {
				continue
			}
			used++
			low := view.leaves.base | uint64(index)<<view.leaves.shift
			high := low | ((uint64(1) << view.leaves.shift) - 1)
			// Check whole-bucket endpoints and every quantization boundary,
			// including the ambiguous intervals that require ordinary descent.
			steps := min(uint64(1024), uint64(1)<<view.leaves.shift)
			for q := uint64(0); q < steps; q++ {
				key := low | q<<max(0, int(view.leaves.shift)-10)
				before, after := key, key
				if key > low {
					before--
				}
				if key < high {
					after++
				}
				for _, probe := range []uint64{before, key, after, high} {
					id, _ := view.leaves.lookup(probe)
					if id == 0 {
						continue
					}
					want := words.tree.root
					for !words.tree.node(want).leaf {
						n := words.tree.node(want)
						want = n.inner().children[words.tree.route(n, probe)]
					}
					if id != want {
						t.Fatalf("shift=%d key=%x leaf=%d want=%d", shift, probe, id, want)
					}
				}
			}
		}
		if used == 0 {
			t.Fatal("no leaf routes published")
		}
		for i := uint64(0); i < size; i++ {
			key := mixedKey(i) >> shift
			m.Insert(key, key+1)
			if value, found := m.Get(key); !found || value != key+1 {
				t.Fatal("cached value update")
			}
		}
		if words.view.Load().leaves != view.leaves {
			t.Fatal("replacement retired routes")
		}
		for _, key := range []uint64{0, math.MaxUint64} {
			m.Insert(key, key)
			if value, found := m.Get(key); !found || value != key {
				t.Fatal("mutation after caching")
			}
		}
		if words.view.Load().leaves != nil {
			t.Fatal("key insertion retained routes")
		}
	}
}

func TestLargeMapUsesOnlyRouteCache(t *testing.T) {
	var m Map[uint64, uint64]
	for i := uint64(0); i <= smallReadCacheLimit; i++ {
		m.Insert(mixedKey(i), i)
	}
	for i := uint64(0); i < 8192; i++ {
		if value, found := m.Get(mixedKey(i)); !found || value != i {
			t.Fatal("large cached read")
		}
	}
	var keys [13]uint64
	var values [13]Result[uint64]
	for i := range keys {
		keys[i] = mixedKey(uint64(i))
	}
	m.GetMany(keys[:], values[:])
	if allocations := testing.AllocsPerRun(100, func() { m.GetMany(keys[:], values[:]) }); allocations != 0 {
		t.Fatalf("batch allocations: %g", allocations)
	}
	for i, result := range values {
		if !result.OK || result.Value != uint64(i) {
			t.Fatal("large batch result")
		}
	}
	view := m.words.Load().view.Load()
	if view.cache != nil || view.leaves == nil {
		t.Fatal("large map duplicated entry storage")
	}
}
