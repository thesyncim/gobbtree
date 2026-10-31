package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

// A store against a map, on a budget that counts its pages and may refuse
// them past a limit, as upstream's tests/store.rs, whose tests these are.
// Every page the store gives back, and every record's bytes as it dies, are
// scribbled over first (scribble), so that a read of either after reads no
// key or value it held.
func TestMain(m *testing.M) {
	scribble = true
	os.Exit(m.Run())
}

// counting is upstream's Counting: the pages out, and at most limit of them
// out at once; 0 for any. The map's nodes are the collector's here, so heap
// pages, overflow pages among them, are the one kind it counts.
type counting struct {
	heap  atomic.Int64
	limit atomic.Int64
}

func (c *counting) out() int { return int(c.heap.Load()) }

func (c *counting) TakePages(n int) bool {
	if limit := c.limit.Load(); limit != 0 && c.heap.Load()+int64(n) > limit {
		return false
	}
	c.heap.Add(int64(n))
	return true
}

func (c *counting) GivePages(n int) {
	if c.heap.Add(-int64(n)) < 0 {
		panic("a page given back twice, or never taken")
	}
}

// page is the default heap page size, the leaves'.
const page = DefaultHeapPageSize

// key is the key numbered n: how many bytes n takes, then those bytes, then
// filler, from 2 to 40 bytes, some within an 8-byte window and some not. No
// two numbers share a key.
func key(n int) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	skipped := 0
	for skipped < 7 && b[skipped] == 0 {
		skipped++
	}
	k := append([]byte{byte(8 - skipped)}, b[skipped:]...)
	return append(k, bytes.Repeat([]byte{'k'}, n%32)...)
}

func value(n, version, length int) []byte {
	v := make([]byte, length)
	for i := range v {
		v[i] = byte(n + version*7 + i)
	}
	return v
}

// rng is upstream's deterministic xorshift generator.
type rng uint64

func (r *rng) below(bound int) int {
	*r ^= *r << 13
	*r ^= *r >> 7
	*r ^= *r << 17
	return int(uint64(*r) % uint64(bound))
}

// heapPerEntryByte is the heap bytes of pages per byte of the entries' keys
// and values.
func heapPerEntryByte(pages *counting, entryBytes int) float64 {
	return float64(pages.out()*page) / float64(entryBytes)
}

// mustGet is s's value of key, failing t on an error.
func mustInsert(t testing.TB, s *Store, key, value []byte) bool {
	t.Helper()
	replaced, err := s.Insert(key, value)
	if err != nil {
		t.Fatalf("insert of %q: %v", key, err)
	}
	return replaced
}

func assertGet(t testing.TB, s *Store, key, want []byte, present bool) {
	t.Helper()
	got, ok := s.Get(key)
	if ok != present || present && (!bytes.Equal(got, want) || got == nil) {
		t.Fatalf("Get(%q) = %d bytes %v, want %d bytes %v", key, len(got), ok, len(want), present)
	}
}

// assertCleared clears s and checks that every page went back.
func assertCleared(t testing.TB, s *Store, pages *counting) {
	t.Helper()
	s.Clear()
	if out := pages.out(); out != 0 {
		t.Fatalf("%d pages out after Clear: every page goes back", out)
	}
}

// checkLive checks each page that holds a live record: its live count is
// the bytes of the records the store's keys point to in it, and an orphan's
// has its orphan bit.
func checkLive(t testing.TB, s *Store) {
	t.Helper()
	live := map[*heapPage]int{}
	for stored := range s.m.All() {
		r := stored.record()
		if !bytes.Equal(r.key(), stored.bytes()) || r.offset() < recordsOffset || r.offset()+r.size(&s.heap.g) > s.heap.g.heapObject {
			t.Fatalf("a key's record at %d lies outside its page", r.offset())
		}
		live[r.page()] += r.size(&s.heap.g)
	}
	for p, bytes := range live {
		if got := p.liveBytes(); got != bytes {
			t.Fatalf("a page counts %d live bytes, its records %d", got, bytes)
		}
	}
}

func TestInsertsReadsReplacesAndRemoves(t *testing.T) {
	s := New()
	assertGet(t, s, []byte("apple"), nil, false)
	if replaced := mustInsert(t, s, []byte("apple"), []byte("red")); replaced {
		t.Fatal("a new key replaced a value")
	}
	mustInsert(t, s, []byte("banana"), []byte("yellow"))
	assertGet(t, s, []byte("apple"), []byte("red"), true)
	// A value no longer goes in place; a longer one in a new record.
	if !mustInsert(t, s, []byte("apple"), []byte("re")) {
		t.Fatal("a held key's value wasn't replaced")
	}
	assertGet(t, s, []byte("apple"), []byte("re"), true)
	if !mustInsert(t, s, []byte("apple"), []byte("green and red")) {
		t.Fatal("a held key's value wasn't replaced")
	}
	assertGet(t, s, []byte("apple"), []byte("green and red"), true)
	buf, ok := s.GetInto([]byte("banana"), []byte("x"))
	if !ok || string(buf) != "xyellow" {
		t.Fatalf("GetInto appended to %q, %v", buf, ok)
	}
	if buf, ok := s.GetInto([]byte("cherry"), []byte("x")); ok || string(buf) != "x" {
		t.Fatalf("GetInto of a missing key gave %q, %v", buf, ok)
	}
	if !s.Contains([]byte("banana")) || !s.Remove([]byte("banana")) || s.Remove([]byte("banana")) || s.Contains([]byte("banana")) {
		t.Fatal("banana's removal")
	}
	mustInsert(t, s, nil, nil)
	assertGet(t, s, []byte{}, []byte{}, true)
}

// Lookups copy values out, and inserts in place write them, allocating at
// most a query's key: the tree's lookups call its keys' methods through
// their dictionary, so the compiler can't keep a key the store passes
// them, which points to a struct of the caller's slice, on the stack.
func TestLookupsAllocateAtMostTheQuery(t *testing.T) {
	s := New()
	k, v := []byte("apple"), []byte("red")
	mustInsert(t, s, k, v)
	buf, missing := make([]byte, 0, 16), []byte("cherry")
	for _, c := range []struct {
		name string
		f    func()
	}{
		{"GetInto", func() { buf, _ = s.GetInto(k, buf[:0]) }},
		{"Contains", func() { s.Contains(k) }},
		{"Insert in place", func() { s.Insert(k, v) }},
		{"Remove of a missing key", func() { s.Remove(missing) }},
	} {
		n := testing.AllocsPerRun(100, c.f)
		if n > 1 {
			t.Errorf("%s: %.0f allocations", c.name, n)
		}
		t.Logf("%s: %.0f allocations", c.name, n)
	}
}

// Keys longer than a quarter page are refused; values of any length go in,
// those too long for their record to fit in a page's room into overflow
// pages of their own, all full but for what is left over, which the record
// keeps if it fits, else a last page. Long values are replaced and removed,
// and the store clears holding some.
func TestRefusesLongKeysAndTakesValuesOfAnyLength(t *testing.T) {
	s := New()
	maxKey := s.MaxKeyLen()
	if _, err := s.Insert(make([]byte, maxKey+1), nil); !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("a key past the cap: %v", err)
	}
	// A page's room, past its header of a pointer and two uint32s and the
	// allocator's 8 bytes (1,000 bytes with 8-byte pointers), holds a
	// record of nine bytes of lengths, offset and generation, the key, and a
	// 4-byte link before what the record keeps of a long value; an overflow
	// page's, past its link and the allocator's 8 bytes, the value's bytes
	// (1,008). Upstream's rooms are 1,008 and 1,016 and its link 8 bytes:
	// the boundaries are its, by these constants.
	g := &s.heap.g
	room, overflowRoom := g.room, g.overflowRoom
	if room != page-8-16 || overflowRoom != page-8-8 {
		t.Fatalf("rooms %d and %d", room, overflowRoom)
	}
	for _, c := range []struct{ keyLen, valueLen, overflowPages int }{
		{20, 0, 0},
		{20, room - 9 - 20, 0},
		// Left over: all of it, which doesn't fit with the link.
		{20, room - 9 - 20 + 1, 1},
		{20, overflowRoom, 1},
		{20, overflowRoom + room - 9 - 20 - linkSize, 1},
		{20, overflowRoom + room - 9 - 20 - linkSize + 1, 2},
		{20, 10*overflowRoom + 500, 10},
		{maxKey, room - 9 - maxKey, 0},
		{maxKey, room - 9 - maxKey + 1, 1},
		{maxKey, 3*overflowRoom + room - 9 - maxKey - linkSize, 3},
	} {
		pages := &counting{}
		s := NewIn(pages)
		k, v := bytes.Repeat([]byte{1}, c.keyLen), value(c.keyLen, c.valueLen, c.valueLen)
		if mustInsert(t, s, k, v) {
			t.Fatal("a new key replaced a value")
		}
		if got := pages.out(); got != 1+c.overflowPages {
			t.Fatalf("a %d-byte key's %d-byte value took %d pages, want %d", c.keyLen, c.valueLen, got, 1+c.overflowPages)
		}
		assertGet(t, s, k, v, true)
		var scanned [][]byte
		for _, found := range s.Scan(Bounds{}) {
			scanned = append(scanned, bytes.Clone(found))
		}
		if len(scanned) != 1 || !bytes.Equal(scanned[0], v) {
			t.Fatalf("the scan gave %d values", len(scanned))
		}
		assertCleared(t, s, pages)
	}
	pages := &counting{}
	s = NewIn(pages)
	const count = 64
	for n := range count {
		length := [3]int{5000, 10, 3 * page}[n%3]
		if mustInsert(t, s, key(n), value(n, 0, length)) {
			t.Fatal("a new key replaced a value")
		}
	}
	for n := range count {
		length := [3]int{10, 4 * page, 0}[n%3]
		if !mustInsert(t, s, key(n), value(n, 1, length)) {
			t.Fatal("a held key's value wasn't replaced")
		}
		if n%5 == 0 && !s.Remove(key(n)) {
			t.Fatalf("Remove(%d) missed", n)
		}
	}
	for n := range count {
		length := [3]int{10, 4 * page, 0}[n%3]
		assertGet(t, s, key(n), value(n, 1, length), n%5 != 0)
	}
	checkLive(t, s)
	assertCleared(t, s, pages)
}

// A long value whose overflow pages the budget refuses partway goes in
// nowhere, with the pages it took given back, the store as it was.
func TestALongValuePastTheBudgetTakesNoPage(t *testing.T) {
	pages := &counting{}
	s := NewIn(pages)
	mustInsert(t, s, []byte("apple"), []byte("red"))
	mustInsert(t, s, []byte("banana"), value(0, 0, 2*page))
	out := pages.out()
	pages.limit.Store(int64(out + 3))
	for _, k := range []string{"apple", "banana", "cherry"} {
		if _, err := s.Insert([]byte(k), value(1, 1, 8*page)); !errors.Is(err, ErrAlloc) {
			t.Fatalf("an insert of %s past the budget: %v", k, err)
		}
		if pages.out() != out {
			t.Fatalf("%d pages out after a refused insert, %d before", pages.out(), out)
		}
	}
	assertGet(t, s, []byte("apple"), []byte("red"), true)
	assertGet(t, s, []byte("banana"), value(0, 0, 2*page), true)
	assertGet(t, s, []byte("cherry"), nil, false)
	assertCleared(t, s, pages)
}

// model is a map of the store's entries, upstream's BTreeMap.
type model map[string][]byte

// sorted is the model's entries whose keys lie in b, in key order.
func (m model) sorted(b Bounds) [][2][]byte {
	var entries [][2][]byte
	for k, v := range m {
		if b.holds([]byte(k)) {
			entries = append(entries, [2][]byte{[]byte(k), v})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i][0], entries[j][0]) < 0 })
	return entries
}

// Random inserts, replacements and removals, against a map, with values
// from empty to a few heap pages: leaves split, merge and compact, their
// records move between leaves' chains, and long values' overflow pages
// move with their records.
func TestMatchesAModel(t *testing.T) {
	for _, size := range []int{DefaultHeapPageSize, 4 * DefaultHeapPageSize} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			steps := 60000
			if size != DefaultHeapPageSize || testing.Short() {
				steps = 15000
			}
			matchModel(t, size, steps)
		})
	}
}

func matchModel(t *testing.T, size, steps int) {
	pages := &counting{}
	s := NewWithConfig(Config{HeapPageSize: size}, pages)
	m := model{}
	r := rng(0x9E37_79B9_7F4A_7C15)
	const keys = 3000
	for step := range steps {
		n := r.below(keys)
		k := key(n)
		switch r.below(10) {
		case 0, 1, 2, 3, 4, 5:
			var length int
			switch r.below(16) {
			case 0, 1:
				length = r.below(page)
			case 2:
				length = r.below(4 * page)
			case 3:
				length = 0
			default:
				length = r.below(64)
			}
			v := value(n, step, length)
			_, had := m[string(k)]
			if replaced := mustInsert(t, s, k, v); replaced != had {
				t.Fatalf("step %d: insert of %d replaced %v, the model held it %v", step, n, replaced, had)
			}
			m[string(k)] = v
		case 6, 7, 8:
			_, had := m[string(k)]
			if removed := s.Remove(k); removed != had {
				t.Fatalf("step %d: removal of %d %v, the model held it %v", step, n, removed, had)
			}
			delete(m, string(k))
		default:
			want, had := m[string(k)]
			assertGet(t, s, k, want, had)
		}
		if step%(steps/16) == 0 {
			assertRangesMatch(t, s, m, &r)
			checkLive(t, s)
		}
	}
	for k, v := range m {
		assertGet(t, s, []byte(k), v, true)
	}
	checkLive(t, s)
	for k := range m {
		if !s.Remove([]byte(k)) {
			t.Fatalf("Remove(%q) missed", k)
		}
	}
	assertCleared(t, s, pages)
}

// assertRangesMatch checks scans both ways, and copies from both ends
// meeting, of a random range, against m.
func assertRangesMatch(t testing.TB, s *Store, m model, r *rng) {
	t.Helper()
	low, high := key(r.below(3000)), key(r.below(3000))
	if bytes.Compare(low, high) > 0 {
		low, high = high, low
	}
	b := Between(low, high)
	want := m.sorted(b)
	var scanned [][2][]byte
	for k, v := range s.Scan(b) {
		scanned = append(scanned, [2][]byte{bytes.Clone(k), bytes.Clone(v)})
	}
	assertEntries(t, "Scan", scanned, want)
	scanned = scanned[:0]
	for k, v := range s.ScanReverse(b) {
		scanned = append(scanned, [2][]byte{bytes.Clone(k), bytes.Clone(v)})
	}
	slices.Reverse(scanned)
	assertEntries(t, "ScanReverse", scanned, want)
	copies := s.Range(b)
	var front, back [][2][]byte
	for {
		var k, v []byte
		var ok bool
		if r.below(2) == 0 {
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

func assertEntries(t testing.TB, what string, got, want [][2][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s gave %d entries, want %d", what, len(got), len(want))
	}
	for i := range got {
		if !bytes.Equal(got[i][0], want[i][0]) || !bytes.Equal(got[i][1], want[i][1]) {
			t.Fatalf("%s's entry %d is %q, want %q", what, i, got[i][0], want[i][0])
		}
	}
}

// Inserts past the budget's limit fail, the store holding what it held.
// Removals take holes back without a page, by compacting, so that most new
// records of the removed ones' size fit again: not all, as the room a
// leaf's chain frees stays that leaf's.
//
// How many fit again turns on how many pages each leaf's chain spans, as a
// leaf's holes in other leaves' pages, under its strays, and its head's
// room are its alone. Upstream's 1 KiB leaves hold about 50 store keys,
// and at its 100-byte values its 55 heap pages chain about 7 to a leaf, 8
// leaves; Go's leaves hold 32 keys whatever their size, 20 leaves chaining
// about 3 pages each at those values, which takes back 36% to 61% over the
// seeds below, 41% at upstream's, where upstream takes back 54% to 89%,
// 62% at its own. So upstream's case runs as it is, and asserts all but
// how many fit again; values of 230 bytes give Go's chains upstream's
// pages a leaf, about 7 (9 leaves of 64 pages), and assert upstream's
// more than half over every seed.
func TestASpentBudgetRefusesInsertsAndChangesNothing(t *testing.T) {
	t.Run("upstream", func(t *testing.T) {
		takenBack, removed, leaves := spendBudget(t, 0x2545_F491_4F6C_DD1D, 100)
		if takenBack == 0 {
			t.Fatal("no removed key went back in")
		}
		t.Logf("%d of %d removed keys back in, %d leaves", takenBack, removed, leaves)
	})
	t.Run("upstream's pages a leaf", func(t *testing.T) {
		for _, seed := range []uint64{0x2545_F491_4F6C_DD1D, 1, 2, 3, 4, 5, 6, 7} {
			takenBack, removed, leaves := spendBudget(t, seed, 230)
			if takenBack*2 <= removed {
				t.Fatalf("seed %#x: %d of %d removed keys back in", seed, takenBack, removed)
			}
			t.Logf("seed %#x: %d of %d removed keys back in, %d leaves", seed, takenBack, removed, leaves)
		}
	})
}

// spendBudget fills a store of 64 pages with values of valueLen bytes
// until its budget refuses, removes every other key, and puts them back;
// how many went back in, of how many removed, and how many leaves held the
// keys once full.
func spendBudget(t *testing.T, seed uint64, valueLen int) (takenBack, removedKeys, leaves int) {
	pages := &counting{}
	const limit, steps, keys = 64, 3000, 2000
	pages.limit.Store(limit)
	s := NewIn(pages)
	m := model{}
	refused := 0
	r := rng(seed)
	for step := range steps {
		n := r.below(keys)
		k, v := key(n), value(n, step, valueLen)
		was, had := m[string(k)]
		switch replaced, err := s.Insert(k, v); {
		case err == nil:
			if replaced != had {
				t.Fatalf("step %d: insert replaced %v, the model held it %v", step, replaced, had)
			}
			m[string(k)] = v
		case errors.Is(err, ErrAlloc):
			refused++
			assertGet(t, s, k, was, had)
		default:
			t.Fatal(err)
		}
		if pages.out() > limit {
			t.Fatalf("%d pages out past the limit", pages.out())
		}
	}
	if refused == 0 {
		t.Fatal("the budget never ran out")
	}
	for k, v := range m {
		assertGet(t, s, []byte(k), v, true)
	}
	leaves = countLeaves(s)
	var removed [][]byte
	for i, e := range m.sorted(Bounds{}) {
		if i%2 == 0 {
			removed = append(removed, e[0])
		}
	}
	for _, k := range removed {
		if !s.Remove(k) {
			t.Fatalf("Remove(%q) missed", k)
		}
		delete(m, string(k))
	}
	for _, k := range removed {
		v := value(0, 1, valueLen)
		switch _, err := s.Insert(k, v); {
		case err == nil:
			m[string(k)] = v
			takenBack++
		case errors.Is(err, ErrAlloc):
			assertGet(t, s, k, nil, false)
		default:
			t.Fatal(err)
		}
		if pages.out() > limit {
			t.Fatalf("%d pages out past the limit", pages.out())
		}
	}
	for k, v := range m {
		assertGet(t, s, []byte(k), v, true)
	}
	checkLive(t, s)
	assertCleared(t, s, pages)
	return takenBack, len(removed), leaves
}

// countLeaves is how many leaves hold s's keys, told apart by their
// annexes, which an insert's callback sees, and refuses, changing nothing.
func countLeaves(s *Store) int {
	var keys [][]byte
	for k := range s.Scan(Bounds{}) {
		keys = append(keys, bytes.Clone(k))
	}
	leaves := map[*leafHeap]bool{}
	for _, k := range keys {
		var q queryKey
		s.m.UpdateOrInsertWithLeaf(newQuery(&q, k), struct{}{},
			func(leaf storeLeaf, _ storeKey, _ *struct{}) bool {
				leaves[leaf.Annex()] = true
				return false
			},
			func(_ storeLeaf, k storeKey, _ struct{}) (storeKey, bool) { return k, false })
	}
	return len(leaves)
}

// Under churn, replacing and removing entries at random over a fixed key
// set, the holes go back: the heap stays under twice what the records
// take, as a leaf compacts once half its chain is holes, and keeps a head
// page with room.
func TestHolesGoBackUnderChurn(t *testing.T) {
	pages := &counting{}
	s := NewIn(pages)
	m := model{}
	r := rng(0x1234_5678_9ABC_DEF1)
	entryBytes := func() int {
		total := 0
		for k, v := range m {
			total += 9 + len(k) + len(v)
		}
		return total
	}
	for round := range 8 {
		for step := range 20000 {
			n := r.below(4000)
			k := key(n)
			if r.below(4) == 0 {
				s.Remove(k)
				delete(m, string(k))
			} else {
				v := value(n, step, 16+r.below(96))
				mustInsert(t, s, k, v)
				m[string(k)] = v
			}
		}
		ratio := heapPerEntryByte(pages, entryBytes())
		if ratio >= 2.0 {
			t.Fatalf("round %d: %.2f heap bytes a record byte", round, ratio)
		}
		t.Logf("round %d: %.2f heap bytes a record byte", round, ratio)
	}
	for k, v := range m {
		assertGet(t, s, []byte(k), v, true)
	}
	checkLive(t, s)
	assertCleared(t, s, pages)
}

// Goroutines insert, read and remove entries apart, some of whose values
// take overflow pages.
func TestGoroutinesShareAStore(t *testing.T) {
	const goroutines, each = 4, 5000
	valueLen := func(n int) int {
		if n%16 == 0 {
			return page + n%(3*page)
		}
		return n % 200
	}
	pages := &counting{}
	s := NewIn(pages)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range each {
				n := i*goroutines + g
				k, v := key(n), value(n, 0, valueLen(n))
				if _, err := s.Insert(k, v); err != nil {
					t.Error(err)
					return
				}
				if got, ok := s.Get(k); !ok || !bytes.Equal(got, v) {
					t.Errorf("goroutine %d: Get(%d) missed its own insert", g, n)
					return
				}
				if i%3 == 0 && !s.Remove(k) {
					t.Errorf("goroutine %d: Remove(%d) missed", g, n)
					return
				}
			}
		})
	}
	wg.Wait()
	for n := range goroutines * each {
		present := (n/goroutines)%3 != 0
		var want []byte
		if present {
			want = value(n, 0, valueLen(n))
		}
		assertGet(t, s, key(n), want, present)
	}
	checkLive(t, s)
	assertCleared(t, s, pages)
}
