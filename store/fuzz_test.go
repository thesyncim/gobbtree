package store

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

// FuzzStore runs a sequence of operations its input spells on a store and a
// model, and checks them against each other: inserts of values from empty
// to several overflow pages, removals, lookups, and scans both ways and
// copied ranges between two keys. The input's first byte sets the budget,
// none or a few dozen pages, so that inserts are refused, the store as it
// was, and holes are taken back. Every page goes back on Clear.
func FuzzStore(f *testing.F) {
	f.Add([]byte{0, 0, 1, 10, 0, 1, 2, 200, 4, 0, 1, 0, 7, 0, 9, 0})
	f.Add([]byte{12, 3, 0, 5, 250, 3, 0, 6, 250, 5, 0, 5, 0, 6, 0, 2, 9})
	f.Add(bytes.Repeat([]byte{0, 7, 3, 99, 1, 77, 2, 5, 4, 7, 2, 3, 6, 1, 2, 0}, 64))
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) == 0 {
			return
		}
		pages := &counting{}
		if b := ops[0]; b != 0 {
			pages.limit.Store(int64(4 + b%60))
		}
		ops = ops[1:]
		s := NewIn(pages)
		m := model{}
		for step := 0; len(ops) >= 4; step, ops = step+1, ops[4:] {
			op, n, arg := ops[0]%8, int(ops[1])|int(ops[2]&1)<<8, int(ops[3])
			k := key(n)
			switch op {
			case 0, 1, 2:
				fuzzInsert(t, s, m, pages, k, value(n, step, arg))
			case 3:
				// From a few hundred bytes to several overflow pages.
				fuzzInsert(t, s, m, pages, k, value(n, step, arg*41))
			case 4, 5:
				_, had := m[string(k)]
				if removed := s.Remove(k); removed != had {
					t.Fatalf("step %d: removal of %d %v, the model held it %v", step, n, removed, had)
				}
				delete(m, string(k))
			case 6:
				want, had := m[string(k)]
				assertGet(t, s, k, want, had)
			case 7:
				low, high := k, key(n+arg)
				if bytes.Compare(low, high) > 0 {
					low, high = high, low
				}
				fuzzRanges(t, s, m, Between(low, high), arg)
			}
			if pages.limit.Load() != 0 && pages.out() > int(pages.limit.Load()) {
				t.Fatalf("%d pages out past the limit", pages.out())
			}
		}
		fuzzRanges(t, s, m, Bounds{}, 0)
		checkLive(t, s)
		assertCleared(t, s, pages)
	})
}

// fuzzInsert inserts k with v into s and m, or, refused by the budget,
// checks that s holds what it held.
func fuzzInsert(t *testing.T, s *Store, m model, pages *counting, k, v []byte) {
	t.Helper()
	was, had := m[string(k)]
	replaced, err := s.Insert(k, v)
	switch {
	case err == nil:
		if replaced != had {
			t.Fatalf("an insert of %q replaced %v, the model held it %v", k, replaced, had)
		}
		m[string(k)] = v
	case errors.Is(err, ErrAlloc) && pages.limit.Load() != 0:
		assertGet(t, s, k, was, had)
	default:
		t.Fatalf("an insert of %q: %v", k, err)
	}
}

// fuzzRanges checks scans of b both ways, and copies from both ends,
// meeting as the bits of pattern say, against m.
func fuzzRanges(t *testing.T, s *Store, m model, b Bounds, pattern int) {
	t.Helper()
	want := m.sorted(b)
	var got [][2][]byte
	for k, v := range s.Scan(b) {
		got = append(got, [2][]byte{bytes.Clone(k), bytes.Clone(v)})
	}
	assertEntries(t, "Scan", got, want)
	got = got[:0]
	for k, v := range s.ScanReverse(b) {
		got = append(got, [2][]byte{bytes.Clone(k), bytes.Clone(v)})
	}
	slices.Reverse(got)
	assertEntries(t, "ScanReverse", got, want)
	copies := s.Range(b)
	var front, back [][2][]byte
	for i := 0; ; i++ {
		var k, v []byte
		var ok bool
		if pattern>>(i%8)&1 == 0 {
			if k, v, ok = copies.Next(); ok {
				front = append(front, [2][]byte{k, v})
			}
		} else if k, v, ok = copies.NextBack(); ok {
			back = append(back, [2][]byte{k, v})
		}
		if !ok {
			break
		}
	}
	slices.Reverse(back)
	assertEntries(t, "Range", append(front, back...), want)
}
