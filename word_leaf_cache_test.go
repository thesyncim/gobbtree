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

// Leaf-cached batch and single reads validate plain loads against leaf
// shifts. Writers retire the cache on every write, so a helper reinstalls a
// fresh one under the structural lock, as reader activation does. Writers
// toggle sparse keys between stable ones: no split, merge or index-0 change,
// so cached leaf routes stay correct while their contents move.
func TestLeafCachedReadsDuringLeafShifts(t *testing.T) {
	const stable = 1 << 14
	var m Map[uint64, uint64]
	// Descending inserts leave every leaf room for the writers' keys, which
	// must not change the structure: ascending runs fill leaves.
	for i := uint64(stable); i > 0; i-- {
		m.Insert(4*(i-1), 4*(i-1)*3+1)
	}
	m.Get(0)
	words := m.words.Load()
	var leaves *wordLeafCache
	install := func() {
		words.structure.Lock()
		defer words.structure.Unlock()
		current := words.view.Load()
		if current.leaves != nil {
			return
		}
		if leaves == nil {
			leaves = newWordLeafCache(current.routes, int64(m.Len()))
		}
		// Without structural changes, filled cells keep routing correctly.
		view := *current
		view.uncached = current
		view.cache = nil
		view.leaves = leaves
		words.view.Store(&view)
	}
	install()
	if words.view.Load().leaves == nil {
		t.Fatal("no leaf cache")
	}
	stop := make(chan struct{})
	var background sync.WaitGroup
	background.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				install()
			}
		}
	})
	for w := uint64(0); w < 2; w++ {
		background.Go(func() {
			x := w*0x9e3779b97f4a7c15 + 1
			for {
				select {
				case <-stop:
					return
				default:
				}
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				k := 4*(x%stable) + 2
				if k%64 != 2 {
					continue
				}
				if _, found := m.Delete(k); !found {
					m.Insert(k, 0)
				}
			}
		})
	}
	var readers sync.WaitGroup
	for r := uint64(0); r < 3; r++ {
		readers.Go(func() {
			x := r*0x2545f4914f6cdd1d + 7
			var batch [8]uint64
			var results [8]Result[uint64]
			for i := 0; i < 100000; i++ {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				for j := range batch {
					batch[j] = 4 * ((x + uint64(j)*977) % stable)
				}
				m.GetMany(batch[:], results[:])
				for j, k := range batch {
					if !results[j].OK || results[j].Value != k*3+1 {
						t.Errorf("GetMany(%d) = %d, %v during leaf shifts", k, results[j].Value, results[j].OK)
						return
					}
				}
				if v, ok := m.Get(batch[0]); !ok || v != batch[0]*3+1 {
					t.Errorf("Get(%d) = %d, %v during leaf shifts", batch[0], v, ok)
					return
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	background.Wait()
}

// Splits and merges retire the leaf cache before moving entries; reads that
// already hold it must fail their epoch validation instead of trusting a
// leaf mid-move. Writers insert and remove dense runs between stable keys.
func TestLeafCachedReadsDuringSplits(t *testing.T) {
	const stable = 1 << 13
	var m Map[uint64, uint64]
	for i := uint64(0); i < stable; i++ {
		m.Insert(64*i, 64*i*3+1)
	}
	m.Get(0)
	words := m.words.Load()
	install := func() {
		words.structure.Lock()
		defer words.structure.Unlock()
		current := words.view.Load()
		if current == nil || current.leaves != nil {
			return
		}
		view := *current
		view.uncached = current
		view.cache = nil
		view.leaves = newWordLeafCache(view.routes, int64(m.Len()))
		words.view.Store(&view)
	}
	stop := make(chan struct{})
	var background sync.WaitGroup
	background.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				install()
			}
		}
	})
	background.Go(func() {
		x := uint64(1)
		for {
			select {
			case <-stop:
				return
			default:
			}
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			base := 64 * (x % stable)
			for k := base + 1; k < base+64; k++ {
				m.Insert(k, 0)
			}
			for k := base + 1; k < base+64; k++ {
				m.Delete(k)
			}
		}
	})
	var readers sync.WaitGroup
	for r := uint64(0); r < 3; r++ {
		readers.Go(func() {
			x := r*0x2545f4914f6cdd1d + 7
			var batch [8]uint64
			var results [8]Result[uint64]
			for i := 0; i < 50000; i++ {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				for j := range batch {
					batch[j] = 64 * ((x + uint64(j)) % stable)
				}
				m.GetMany(batch[:], results[:])
				for j, k := range batch {
					if !results[j].OK || results[j].Value != k*3+1 {
						t.Errorf("GetMany(%d) = %d, %v during splits", k, results[j].Value, results[j].OK)
						return
					}
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	background.Wait()
}
