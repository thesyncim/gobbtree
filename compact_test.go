package gobbtree

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestCompactSearchDifferential(t *testing.T) {
	rng := rand.New(rand.NewPCG(123, 456))
	for n := 0; n <= nodeSize; n++ {
		for distribution := 0; distribution < 5; distribution++ {
			var keys [nodeSize]uint64
			for i := range keys {
				keys[i] = ^uint64(0)
			}
			for i := 0; i < n; i++ {
				switch distribution {
				case 0:
					keys[i] = rng.Uint64()
				case 1:
					keys[i] = uint64(7)<<32 | uint64(rng.Uint32())
				case 2:
					keys[i] = uint64(rng.IntN(2))<<32 | uint64(rng.Uint32())
				case 3:
					keys[i] = ^uint64(0) - uint64(rng.IntN(128))
				case 4:
					keys[i] = rng.Uint64()<<8 | 0x42
				}
			}
			slices.Sort(keys[:n])
			var high [nodeSize]uint32
			var tags [nodeSize]uint8
			for i, k := range keys {
				high[i] = uint32(k >> 32)
				tags[i] = uint8(k)
			}
			queries := []uint64{0, 1, ^uint64(0), uint64(7) << 32, uint64(8) << 32}
			for _, key := range keys[:n] {
				queries = append(queries, key, key-1, key+1)
			}
			for i := 0; i < 128; i++ {
				queries = append(queries, rng.Uint64())
			}
			for _, key := range queries {
				want := 0
				for want < n && keys[want] < key {
					want++
				}
				if got := lowerCompact(&keys, &high, n, key); got != want {
					t.Fatalf("n=%d distribution=%d q=%x: %d != %d", n, distribution, key, got, want)
				}
				got := findCode(&keys, &tags, n, key)
				present := got < n && keys[got] == key
				expected := want < n && keys[want] == key
				if present != expected {
					t.Fatalf("tag search n=%d distribution=%d q=%x: %v != %v", n, distribution, key, present, expected)
				}
			}
		}
	}
}
func TestOptimisticWideNumeric(t *testing.T) {
	rng := rand.New(rand.NewPCG(901, 902))
	m := New[uint64, uint64]()
	model := make(map[uint64]uint64)
	for step := 0; step < 30000; step++ {
		key := mixedKey(rng.Uint64N(4096))
		switch rng.IntN(4) {
		case 0, 1:
			value := rng.Uint64()
			old, had := model[key]
			if got, ok := m.Insert(key, value); got != old || ok != had {
				t.Fatal("insert mismatch")
			}
			model[key] = value
		case 2:
			old, had := model[key]
			if got, ok := m.Delete(key); got != old || ok != had {
				t.Fatal("delete mismatch")
			}
			delete(model, key)
		}
		want, had := model[key]
		if got, ok := m.Get(key); got != want || ok != had {
			t.Fatalf("Get(%x): (%x,%v) != (%x,%v)", key, got, ok, want, had)
		}
	}
	for key, want := range model {
		if got, ok := m.Get(key); !ok || got != want {
			t.Fatal("final lookup mismatch")
		}
	}
}

func TestNumericDirectoryBoundaries(t *testing.T) {
	tr := NewTree[uint64, uint64]()
	m := New[uint64, uint64]()
	model := make(map[uint64]uint64)
	for i := uint64(0); i < 4096; i++ {
		key := mixedKey(i)
		tr.Insert(key, i)
		m.Insert(key, i)
		model[key] = i
	}
	for bucket := uint64(0); bucket < 256; bucket++ {
		base := bucket << 56
		keys := []uint64{base - 1, base, base + 1}
		var dst [3]Result[uint64]
		m.GetMany(keys, dst[:])
		for i, key := range keys {
			want, ok := model[key]
			if got, found := tr.Get(key); got != want || found != ok {
				t.Fatalf("tree bucket %d q=%x", bucket, key)
			}
			if got, found := m.Get(key); got != want || found != ok {
				t.Fatalf("map bucket %d q=%x", bucket, key)
			}
			if dst[i].Value != want || dst[i].OK != ok {
				t.Fatalf("batch bucket %d q=%x", bucket, key)
			}
		}
	}
}
