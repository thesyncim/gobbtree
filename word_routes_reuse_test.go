package gobbtree

import "testing"

func TestWordRoutesReuseOnDomainChange(t *testing.T) {
	const base = uint64(1) << 60
	var owner Tree[uint64, uint64]
	var store interface {
		Insert(uint64, uint64) (uint64, bool)
		Delete(uint64) (uint64, bool)
		Get(uint64) (uint64, bool)
	} = &owner
	for i := uint64(0); i < 4096; i++ {
		store.Insert(base+i*2, i)
	}
	store.Get(base) // Activate optimistic readers before changing the domain.
	words := owner.words
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
				t.Fatalf("rebuilt domain lost key %d", i)
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
	// Trees need no allocations once the persistent arena is built.
	if allocations > 0 {
		t.Fatalf("domain change allocated %g objects, want none", allocations)
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
