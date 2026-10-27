package gobbtree

import "testing"

func TestWordRoutesReuseOnDomainChange(t *testing.T) {
	const base = uint64(1) << 60
	for _, shared := range []bool{false, true} {
		var owner Tree[uint64, uint64]
		var concurrent Map[uint64, uint64]
		var store interface {
			Insert(uint64, uint64) (uint64, bool)
			Delete(uint64) (uint64, bool)
			Get(uint64) (uint64, bool)
		} = &owner
		if shared {
			store = &concurrent
		}
		for i := uint64(0); i < 4096; i++ {
			store.Insert(base+i*2, i)
		}
		store.Get(base) // Activate optimistic readers before changing the domain.
		words := owner.words
		if shared {
			words = &concurrent.words.Load().tree
		}
		before := words.routes
		if len(before.ids) == 0 {
			t.Fatal("test did not populate cached routes")
		}
		for round := 0; round < 4; round++ {
			store.Insert(0, 1)
			if words.routes.base == before.base && words.routes.shift == before.shift {
				t.Fatal("test did not change the route domain")
			}
			if len(words.routes.ids) != len(before.ids) || &words.routes.ids[0] != &before.ids[0] {
				t.Fatal("equal-sized route domain rebuild allocated new storage")
			}
			for i := uint64(0); i < 4096; i++ {
				if value, ok := store.Get(base + i*2); !ok || value != i {
					t.Fatalf("shared=%v: rebuilt domain lost key %d", shared, i)
				}
			}
			if value, ok := store.Delete(0); !ok || value != 1 {
				t.Fatal("could not remove temporary domain boundary")
			}
			if words.routes.base != before.base || words.routes.shift != before.shift || &words.routes.ids[0] != &before.ids[0] {
				t.Fatal("restoring the route domain did not retain storage")
			}
			checkWordTree(t, words, 4096)
		}
		allocations := testing.AllocsPerRun(20, func() {
			store.Insert(0, 1)
			store.Delete(0)
		})
		// Shared maps publish a new immutable view for each changed domain.
		// Owner trees need no allocations once the persistent arena is built.
		limit := float64(0)
		if shared {
			limit = 2
		}
		if allocations > limit {
			t.Fatalf("shared=%v: domain change allocated %g objects, want at most %g", shared, allocations, limit)
		}
	}
}

func TestSharedWordRouteDomainRebuildReaders(t *testing.T) {
	const base = uint64(1) << 60
	var m Map[uint64, uint64]
	for i := uint64(0); i < 4096; i++ {
		m.Insert(base+i*2, i)
	}
	m.Get(base)
	done := make(chan struct{})
	exited := make(chan struct{})
	failed := make(chan uint64, 1)
	go func() {
		defer close(exited)
		for i := uint64(0); ; i++ {
			select {
			case <-done:
				return
			default:
			}
			index := i * 499 & 4095
			if value, ok := m.Get(base + index*2); !ok || value != index {
				failed <- index
				return
			}
		}
	}()
	for i := 0; i < 1000; i++ {
		m.Insert(0, 1)
		m.Delete(0)
	}
	close(done)
	<-exited
	select {
	case index := <-failed:
		t.Fatalf("route-domain rebuild lost unchanged key %d", index)
	default:
	}
}

// Ascending keys put the root's last separator far below the largest key;
// the route domain must still reach it, or every key above it descends.
func TestRouteDomainCoversLargestKey(t *testing.T) {
	var tr Tree[uint64, uint64]
	const n = 300000
	for i := range uint64(n) {
		tr.Insert(i<<1, i)
	}
	r := &tr.words.routes
	if len(r.ids) == 0 {
		t.Fatal("no route table")
	}
	if last := uint64(n-1) << 1; (last-r.base)>>r.shift >= uint64(len(r.ids)) {
		t.Fatalf("largest key %d is outside the route domain [%d, +%d<<%d)", last, r.base, len(r.ids), r.shift)
	}
}
