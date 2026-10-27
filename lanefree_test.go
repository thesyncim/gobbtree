package gobbtree

import (
	"sync"
	"testing"
)

// Lane-free writers share leaves with each other and with structural
// writers. Each writer owns its keys, so the final contents and length are
// exactly the union of what the writers believe they hold.
func TestLaneFreeWritersMatchOwners(t *testing.T) {
	const writers, span = 8, 1 << 12
	var m Map[uint64, uint64]
	for i := uint64(0); i < writers*span; i += 3 {
		m.Insert(i, i)
	}
	m.Get(0) // activate readers: writes are lane-free from here on
	owned := make([]map[uint64]bool, writers)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 2; r++ {
		readers.Go(func() {
			for i := uint64(r); ; i += 7 {
				select {
				case <-stop:
					return
				default:
				}
				if v, ok := m.Get(i % (writers * span)); ok && v != i%(writers*span) {
					t.Errorf("Get(%d) = %d", i%(writers*span), v)
					return
				}
			}
		})
	}
	var wg sync.WaitGroup
	for w := range writers {
		owned[w] = map[uint64]bool{}
		for i := uint64(0); i < writers*span; i += 3 {
			if i%writers == uint64(w) {
				owned[w][i] = true
			}
		}
		wg.Go(func() {
			x := uint64(w)*0x9e3779b97f4a7c15 + 1
			for range 20000 {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				// Dense runs of a writer's own keys split and merge leaves.
				base := (x % span) * writers
				for k := base + uint64(w); k < base+8*writers && k < writers*span; k += writers {
					if owned[w][k] {
						if v, ok := m.Delete(k); !ok || v != k {
							t.Errorf("Delete(%d) = %d, %v", k, v, ok)
							return
						}
						delete(owned[w], k)
					} else {
						if _, replaced := m.Insert(k, k); replaced {
							t.Errorf("Insert(%d) replaced", k)
							return
						}
						owned[w][k] = true
					}
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	want := 0
	for w := range writers {
		want += len(owned[w])
		for k := range owned[w] {
			if v, ok := m.Get(k); !ok || v != k {
				t.Fatalf("lost %d", k)
			}
		}
	}
	got := 0
	for k := range m.All() {
		if !owned[k%writers][k] {
			t.Fatalf("stray %d", k)
		}
		got++
	}
	if got != want || m.Len() != want {
		t.Fatalf("contents %d, Len %d, want %d", got, m.Len(), want)
	}
}

// Clear drops a tree that lane-free writers may be writing into.
func TestLaneFreeWritersDuringClear(t *testing.T) {
	var m Map[uint64, uint64]
	for i := range uint64(4096) {
		m.Insert(i, i)
	}
	m.Get(0)
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range uint64(20000) {
				k := (i*7 + uint64(w)) % 4096
				m.Insert(k, k)
				m.Delete(k ^ 1)
				if i%5000 == 0 && w == 0 {
					m.Clear()
				}
			}
		})
	}
	wg.Wait()
	n := 0
	for k, v := range m.All() {
		if k != v {
			t.Fatalf("%d=%d", k, v)
		}
		n++
	}
	if n != m.Len() {
		t.Fatalf("contents %d, Len %d", n, m.Len())
	}
}
