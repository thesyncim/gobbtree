package gobbtree

import "testing"

func TestUnusedMapAllocations(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T, *Map[uint64, uint64])
	}{
		{"Get", func(t *testing.T, m *Map[uint64, uint64]) {
			if value, ok := m.Get(7); ok || value != 0 {
				t.Fatal("unexpected entry")
			}
		}},
		{"Contains", func(t *testing.T, m *Map[uint64, uint64]) {
			if m.Contains(7) {
				t.Fatal("unexpected entry")
			}
		}},
		{"Delete", func(t *testing.T, m *Map[uint64, uint64]) {
			if value, ok := m.Delete(7); ok || value != 0 {
				t.Fatal("unexpected deletion")
			}
		}},
		{"GetManyEmpty", func(t *testing.T, m *Map[uint64, uint64]) { m.GetMany(nil, nil) }},
		{"GetMany", func(t *testing.T, m *Map[uint64, uint64]) {
			keys := [2]uint64{7, 8}
			dst := [3]Result[uint64]{{1, true}, {2, true}, {3, true}}
			m.GetMany(keys[:], dst[:])
			if dst[0] != (Result[uint64]{}) || dst[1] != (Result[uint64]{}) || dst[2] != (Result[uint64]{3, true}) {
				t.Fatal("batch results or trailing destination changed")
			}
		}},
		{"Clear", func(t *testing.T, m *Map[uint64, uint64]) { m.Clear() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := new(Map[uint64, uint64])
			allocs := testing.AllocsPerRun(100, func() {
				*m = Map[uint64, uint64]{}
				tc.run(t, m)
			})
			if allocs != 0 {
				t.Fatalf("unused map: %g allocations", allocs)
			}
			m.Insert(7, 99)
			if value, ok := m.Get(7); !ok || value != 99 {
				t.Fatal("first insertion after empty operation was lost")
			}
		})
	}
}

func TestEmptyBatchKeepsPopulatedMap(t *testing.T) {
	var m Map[uint64, uint64]
	m.Insert(7, 99)
	if allocs := testing.AllocsPerRun(100, func() { m.GetMany(nil, nil) }); allocs != 0 {
		t.Fatalf("empty batch: %g allocations", allocs)
	}
	if value, ok := m.Get(7); !ok || value != 99 {
		t.Fatal("empty batch changed the map")
	}
}
