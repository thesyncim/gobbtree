package gobbtree

import (
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestUpperTagBoundaries(t *testing.T) {
	for seed := uint64(0); seed < 8; seed++ {
		var tags [nodeSize]uint8
		for i := range tags {
			tags[i] = uint8(mixedKey(uint64(i) + seed*71))
		}
		slices.Sort(tags[:])
		for count := 0; count <= nodeSize; count++ {
			for key := 0; key < 256; key++ {
				want := 0
				for want < count && tags[want] <= uint8(key) {
					want++
				}
				var node wordNode
				node.storeTags(tags)
				got, mayEqual := upperSharedTag(node.tagWords(), count, uint8(key))
				if got != want || (want > 0 && tags[want-1] == uint8(key) && !mayEqual) {
					t.Fatalf("atomic upper tags count=%d key=%d", count, key)
				}
				if firstSharedTag(node.tagWords(), count, uint64(key)) != firstTagScalar(&tags, count, uint8(key)) {
					t.Fatal("atomic first tags")
				}
				if got := upperTag(&tags, count, uint8(key)); got != want {
					t.Fatalf("seed=%d count=%d key=%d got=%d want=%d", seed, count, key, got, want)
				}
			}
		}
	}
}
func TestSharedWordRouting(t *testing.T) {
	for _, shift := range []uint{0, 16, 32, 48} {
		var m Map[uint64, uint64]
		reference := make(map[uint64]uint64)
		for i := uint64(0); i < 10000; i++ {
			key := mixedKey(i) >> shift
			if shift != 0 {
				key |= uint64(0xabcd) << 48
			}
			value := key ^ i
			old, found := m.Insert(key, value)
			expected, present := reference[key]
			if old != expected || found != present {
				t.Fatal("insert replacement")
			}
			reference[key] = value
		}
		m.Insert(math.MaxUint64, 0)
		reference[math.MaxUint64] = 0
		checkWordTree(t, &m.words.Load().tree, len(reference))
		for key, value := range reference {
			if got, ok := m.Get(key); !ok || got != value {
				t.Fatalf("shift %d key %d", shift, key)
			}
		}
		for i := uint64(10000); i < 20000; i++ {
			key := mixedKey(i)
			got, ok := m.Get(key)
			expected, present := reference[key]
			if got != expected || ok != present {
				t.Fatal("miss/prefix routing")
			}
		}
	}
	if unsafe.Offsetof(wordNode{}.prev)%8 != 0 || unsafe.Offsetof(wordNode{}.tags)%8 != 0 {
		t.Fatal("unaligned atomic view")
	}
}
func TestSharedWordArenaReuse(t *testing.T) {
	var m Map[uint64, uint64]
	const size = 4096
	const mask = uint64(0x123456789)
	for key := uint64(0); key < size; key++ {
		m.Insert(key, key^mask)
	}
	words := m.words.Load()
	m.Get(1) // Activate optimistic readers before pinning a retired view.
	retired := words.view.Load()
	var stop atomic.Bool
	var readers sync.WaitGroup
	readers.Go(func() {
		for !stop.Load() {
			for blockIndex := range retired.blocks {
				b := retired.blocks[blockIndex].Load()
				if b == nil {
					continue
				}
				for i := 0; i < wordSlabSize(blockIndex); i++ {
					n := wordSlabNode(b, uint32(i))
					// Retired readers may still load numeric words while the arena reuses
					// them. All such accesses must stay atomic even across reclamation.
					_ = n.sequence.Load()
					_ = n.loadTags()
					_ = atomic.LoadUint64(&n.keys[0])
				}
			}
		}
	})
	readers.Go(func() {
		for i := uint64(0); !stop.Load(); i++ {
			key := i % (4 * size)
			if value, ok := m.Get(key); ok && value != key^mask {
				t.Error("reused leaf associated the wrong value")
				return
			}
		}
	})
	for round := uint64(0); round < 3; round++ {
		for key := max(uint64(1), round*size); key < (round+1)*size; key++ {
			m.Delete(key)
		}
		runtime.GC()
		for key := (round + 1) * size; key < (round+2)*size; key++ {
			m.Insert(key, key^mask)
		}
	}
	stop.Store(true)
	readers.Wait()
	runtime.KeepAlive(retired)
	checkWordTree(t, &words.tree, m.Len())
}

func TestDeepRoutesAfterBoundaryChanges(t *testing.T) {
	const size = 320000
	var m Map[uint64, uint64]
	for i := uint64(0); i < size; i++ {
		key := i * 4
		m.Insert(key, key)
	}
	words := m.words.Load()
	if words.tree.rootNode.level < 4 {
		t.Fatal("test did not reach the deeper routing directory")
	}
	checkWordRoutes(t, &words.tree)
	var boundaries []uint64
	for slabIndex, base := range words.tree.slabs {
		slab := unsafe.Slice(base, wordSlabSize(slabIndex))
		for i := range slab {
			n := &slab[i]
			if !n.leaf && n.level == 2 && n.n > 0 {
				boundaries = append(boundaries, n.keys[0], n.keys[n.n/2])
			}
		}
	}
	var stop atomic.Bool
	var readers sync.WaitGroup
	for worker := uint64(0); worker < 2; worker++ {
		readers.Go(func() {
			for i := worker; !stop.Load(); i += 2 {
				key := ((i*499)%(size/2)*2 + 1) * 4
				value, ok := m.Get(key)
				if !ok || value != key {
					t.Error("unchanged key lost during directory repair")
					return
				}
			}
		})
	}
	for _, boundary := range boundaries {
		first := (boundary / 4) &^ 1
		for offset := uint64(0); offset < 8; offset += 2 {
			key := (first + offset) * 4
			if key >= size*4 {
				continue
			}
			m.Delete(key)
			m.Insert(key+1, key+1)
		}
	}
	stop.Store(true)
	readers.Wait()
	checkWordTree(t, &words.tree, m.Len())
	for i := uint64(0); i < size; i++ {
		key := i * 4
		value, ok := m.Get(key)
		if ok && value != key {
			t.Fatal("wrong value after repair")
		}
		if !ok {
			value, ok = m.Get(key + 1)
			if !ok || value != key+1 {
				t.Fatal("key missing from both old and new position")
			}
		}
	}
}

func TestFirstNumericReaderDuringWrites(t *testing.T) {
	for round := 0; round < 32; round++ {
		var m Map[uint64, uint64]
		var group sync.WaitGroup
		for worker := uint64(0); worker < 4; worker++ {
			group.Go(func() {
				for i := worker; i < 1024; i += 4 {
					m.Insert(i, i)
				}
			})
		}
		group.Go(func() {
			for key := uint64(0); key < 1024; key++ {
				if value, ok := m.Get(key); ok && value != key {
					t.Error("first reader observed incomplete entry")
				}
			}
		})
		group.Wait()
		for key := uint64(0); key < 1024; key++ {
			if value, ok := m.Get(key); !ok || value != key {
				t.Fatal("entry lost across reader activation")
			}
		}
	}
}

func TestReaderActivationInvalidatesUncachedValueRead(t *testing.T) {
	var m Map[uint64, uint64]
	const key, oldValue, newValue = uint64(7), uint64(11), uint64(12)
	m.Insert(key, oldValue)
	words := m.words.Load()
	// A Get can load its version before activation and then capture the ready
	// arena view published immediately before the cached view.
	version := words.version.Load()
	if version&1 != 0 {
		t.Fatal("initial structural epoch is odd")
	}
	if value, found := m.Get(key); !found || value != oldValue {
		t.Fatal("initial cached read")
	}
	view := words.view.Load()
	if view.cache == nil || view.uncached == nil || !view.uncached.ready {
		t.Fatal("activation did not retain the ready arena view")
	}
	// With one key, the root is the leaf addressed by both published views.
	leaf := wordSlabNode(view.uncached.blocks[0].Load(), 0)
	func() {
		words.structure.RLock()
		defer words.structure.RUnlock()
		leaf.mu.Lock()
		defer leaf.mu.Unlock()
		// Stage storeValue between its live and cached value stores, retaining
		// the writer's leaf latch. This needs no concurrent scheduling or hook.
		atomic.StoreUint64(&leaf.entries()[0].value, newValue)
		defer words.storeValue(view.root, leaf, 0, newValue)
		sequence := leaf.sequence.Load()
		value := atomic.LoadUint64(&leaf.entries()[0].value)
		accepted := sequence&1 == 0 && leaf.sequence.Load() == sequence && words.version.Load() == version
		if value != newValue {
			t.Fatal("staged arena read did not see the replacement")
		}
		// This later public Get can still observe the prior cached copy. The
		// earlier arena read therefore must fail Get's final epoch validation.
		if value, found := m.Get(key); !found || value != oldValue {
			t.Fatal("staged cached read did not see the prior value")
		}
		if accepted {
			t.Fatal("pre-activation reader accepted the new live value before a later Get returned the old cached value")
		}
	}()
	if value, found := m.Get(key); !found || value != newValue {
		t.Fatal("completed replacement left the cache stale")
	}
}

func TestSmallNumericReadCache(t *testing.T) {
	for _, mutation := range []struct {
		name  string
		apply func(*Map[uint64, uint64])
	}{
		{"insert", func(m *Map[uint64, uint64]) { m.Insert(1000, 1000) }},
		{"delete", func(m *Map[uint64, uint64]) { m.Delete(75) }},
		{"load", func(m *Map[uint64, uint64]) { m.LoadOrStore(1000, 1000) }},
		{"merge", func(m *Map[uint64, uint64]) { m.UpdateOrInsert(1000, 1000, func(a, b uint64) uint64 { return b }) }},
		{"predicate", func(m *Map[uint64, uint64]) { m.DeleteIf(75, func(v uint64) bool { return true }) }},
		{"first", func(m *Map[uint64, uint64]) { m.PopFirst() }},
		{"last", func(m *Map[uint64, uint64]) { m.PopLast() }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			var m Map[uint64, uint64]
			for key := uint64(0); key < 200; key++ {
				m.Insert(key, key)
			}
			for key := uint64(0); key < 200; key++ {
				if v, ok := m.Get(key); !ok || v != key {
					t.Fatal("initial read")
				}
			}
			words := m.words.Load()
			cache := words.view.Load().cache
			if cache == nil {
				t.Fatal("small read cache missing")
			}
			m.Insert(50, 500)
			m.Update(51, func(v uint64) uint64 { return 510 })
			m.UpdateOrInsert(52, 520, func(a, b uint64) uint64 { return b })
			for key := uint64(50); key <= 52; key++ {
				if v, ok := m.Get(key); !ok || v != key*10 {
					t.Fatal("stale cached value")
				}
			}
			if words.view.Load().cache != cache {
				t.Fatal("value replacement retired read cache")
			}
			mutation.apply(&m)
			if words.view.Load().cache != nil {
				t.Fatal("key mutation kept stale cache")
			}
			for _, entry := range m.Snapshot() {
				if v, ok := m.Get(entry.Key); !ok || v != entry.Value {
					t.Fatal("lookup after cache retirement")
				}
			}
			runtime.KeepAlive(cache)
		})
	}
}

func TestSmallNumericCacheDuringValueUpdates(t *testing.T) {
	var m Map[uint64, uint64]
	const size = 4096
	for i := uint64(0); i < size; i++ {
		key := mixedKey(i)
		m.Insert(key, key)
	}
	m.Get(mixedKey(0))
	cache := m.words.Load().view.Load().cache
	var group sync.WaitGroup
	for worker := uint64(0); worker < 6; worker++ {
		group.Go(func() {
			for round := uint64(0); round < 4; round++ {
				for i := uint64(0); i < size; i++ {
					key := mixedKey((i + worker*113) % size)
					if worker < 2 {
						m.Insert(key, key^(round&1))
					} else if value, found := m.Get(key); !found || value&^1 != key&^1 {
						t.Error("cache fill raced with value replacement")
						return
					}
				}
			}
		})
	}
	group.Wait()
	for i := uint64(0); i < size; i++ {
		key := mixedKey(i)
		m.Insert(key, key+7)
		if value, found := m.Get(key); !found || value != key+7 {
			t.Fatal("completed replacement left a stale cached value")
		}
	}
	if cache == nil || m.words.Load().view.Load().cache != cache {
		t.Fatal("value-only writes must preserve the cache")
	}
}

func TestSharedLeafTagEdits(t *testing.T) {
	for count := 0; count < nodeSize; count++ {
		var original [nodeSize]uint8
		for i := 0; i < count; i++ {
			original[i] = uint8(i*17 + 5)
		}
		for position := 0; position <= count; position++ {
			for busy := uint64(0); busy < 2; busy++ {
				n := wordNode{leaf: true, n: uint8(count + 1)}
				n.storeTags(original)
				epoch := 7*wordSequenceStep | busy
				n.sequence.Store(epoch | uint64(count)<<1)
				n.insertTag(position, 0x1234ff)
				var expected [nodeSize]uint8
				for i := 0; i <= count; i++ {
					switch {
					case i < position:
						expected[i] = original[i]
					case i == position:
						expected[i] = 255
					default:
						expected[i] = original[i-1]
					}
				}
				if n.loadTags() != expected || n.sequence.Load() != epoch|uint64(count+1)<<1 {
					t.Fatalf("insert tags: count=%d position=%d busy=%d", count, position, busy)
				}
				n.n--
				n.deleteTag(position)
				if n.loadTags() != original || n.sequence.Load() != epoch|uint64(count)<<1 {
					t.Fatalf("delete tags: count=%d position=%d busy=%d", count, position, busy)
				}
			}
		}
	}
}

func TestNumericBuilderPublication(t *testing.T) {
	const size = 8192
	for round := 0; round < 4; round++ {
		var m Map[uint64, uint64]
		for i := uint64(0); i < size; i++ {
			key := mixedKey(i)
			m.Insert(key, key^123)
		}
		for i := uint64(0); i < size; i += 2 {
			m.Delete(mixedKey(i))
		}
		if m.words.Load().tree.atomicWrites {
			t.Fatal("construction unexpectedly enabled optimistic readers")
		}
		var group sync.WaitGroup
		for worker := uint64(0); worker < 2; worker++ {
			group.Go(func() {
				for i := uint64(size) + worker; i < size+2048; i += 2 {
					key := mixedKey(i)
					m.Insert(key, key^123)
					if i%3 == 0 {
						m.Delete(key)
					}
				}
			})
			group.Go(func() {
				for i := 1 + worker*2; i < size; i += 4 {
					key := mixedKey(i)
					if value, ok := m.Get(key); !ok || value != key^123 {
						t.Error("reader activation lost an unchanged entry")
						return
					}
				}
			})
		}
		group.Wait()
		checkWordTree(t, &m.words.Load().tree, m.Len())
	}
}

// Optimistic readers must never return a torn or shifted entry while writers
// move neighboring entries inside the same leaves. Even keys stay present with
// fixed values; odd keys between them are deleted and reinserted continuously.
func TestOptimisticReadsDuringLeafShifts(t *testing.T) {
	const keys = 1 << 14
	var m Map[uint64, uint64]
	for k := uint64(0); k < keys; k += 2 {
		m.Insert(k, k*3+1)
	}
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for w := uint64(0); w < 3; w++ {
		writers.Go(func() {
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
				k := x%keys | 1
				if _, found := m.Delete(k); !found {
					m.Insert(k, k*3+1)
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
			for i := 0; i < 200000; i++ {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				k := x % keys &^ 1
				if value, found := m.Get(k); !found || value != k*3+1 {
					t.Errorf("Get(%d) = %d, %v during leaf shifts", k, value, found)
					return
				}
				if i%8 == 0 {
					for j := range batch {
						batch[j] = (k + uint64(j)*2) % keys
					}
					m.GetMany(batch[:], results[:])
					for j, result := range results {
						if !result.OK || result.Value != batch[j]*3+1 {
							t.Errorf("GetMany(%d) = %d, %v during leaf shifts", batch[j], result.Value, result.OK)
							return
						}
					}
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	writers.Wait()
}

// Textual leaves with word values are read optimistically from the live node.
// Stable keys must keep their values while their zero-extended twins, which
// tie their windows, are deleted and reinserted beside them.
func TestOptimisticTextReadsDuringLeafShifts(t *testing.T) {
	const keys = 1 << 12
	stable := make([]string, keys)
	var m Map[string, uint64]
	for i := range stable {
		stable[i] = fmt.Sprintf("key:%06d", i*7919%keys)
		m.Insert(stable[i], uint64(i)*3+1)
	}
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for w := uint64(0); w < 3; w++ {
		writers.Go(func() {
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
				twin := stable[x%keys] + "\x00"
				if _, found := m.Delete(twin); !found {
					m.Insert(twin, 0)
				}
			}
		})
	}
	var readers sync.WaitGroup
	for r := uint64(0); r < 3; r++ {
		readers.Go(func() {
			x := r*0x2545f4914f6cdd1d + 7
			for i := 0; i < 200000; i++ {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				k := x % keys
				key := strings.Clone(stable[k])
				if value, found := m.Get(key); !found || value != k*3+1 {
					t.Errorf("Get(%q) = %d, %v during leaf shifts", key, value, found)
					return
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	writers.Wait()
}
