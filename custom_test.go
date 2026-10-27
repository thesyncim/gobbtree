package gobbtree

import (
	"cmp"
	"fmt"
	"iter"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
)

// textKey orders like string, so Tree[string] is its oracle.
type textKey struct{ s string }

func (k textKey) Compare(o textKey) int    { return strings.Compare(k.s, o.s) }
func (k textKey) Window(offset int) uint64 { return StringWindow(k.s, offset) }
func (k textKey) String() string           { return k.s }
func textKeys(keys []string) (out []textKey) {
	return mapSlice(keys, func(s string) textKey { return textKey{s} })
}

// descKey has a numeric representation but orders in reverse; it must never
// reach the numeric arenas, which encode the natural order.
type descKey uint64

func (k descKey) Compare(o descKey) int    { return cmp.Compare(o, k) }
func (k descKey) Window(offset int) uint64 { return Uint64Window(^uint64(k), offset) }

// foldKey is a string type whose order ignores ASCII case.
type foldKey string

func (k foldKey) Compare(o foldKey) int {
	return strings.Compare(strings.ToLower(string(k)), strings.ToLower(string(o)))
}
func (k foldKey) Window(offset int) uint64 { return StringWindow(strings.ToLower(string(k)), offset) }

// tenantKey orders by tenant, then by name: four big-endian bytes followed
// by the name's bytes.
type tenantKey struct {
	tenant uint32
	name   string
}

func (k tenantKey) Compare(o tenantKey) int {
	if c := cmp.Compare(k.tenant, o.tenant); c != 0 {
		return c
	}
	return strings.Compare(k.name, o.name)
}
func (k tenantKey) Window(offset int) uint64 {
	if offset >= 4 {
		return StringWindow(k.name, offset-4)
	}
	// Bytes offset..3 of the tenant, then the name's first 4+offset bytes.
	head := uint64(k.tenant)<<32 | StringWindow(k.name, 0)>>32
	return head<<(8*offset) | StringWindow(k.name, 4)>>(64-8*offset)
}

// tieKey keeps the contract with a constant encoding: every window ties, so
// Compare settles every search.
type tieKey int

func (k tieKey) Compare(o tieKey) int { return cmp.Compare(k, o) }
func (k tieKey) Window(int) uint64    { return 0 }

func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

// checkOrdered runs random operations against a sorted reference built only
// from Compare, then checks every ordered view of the tree.
func checkOrdered[K Key[K]](t *testing.T, keys []K, ops int, seed uint64) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 1))
	var tr TreeOf[K, int]
	var m MapOf[K, int]
	var ref []K
	values := map[int]int{}
	find := func(k K) (int, bool) {
		return slices.BinarySearchFunc(ref, k, func(a, b K) int { return a.Compare(b) })
	}
	index := func(k K) int {
		for i, x := range keys {
			if x.Compare(k) == 0 {
				return i
			}
		}
		panic("unknown key")
	}
	for op := 0; op < ops; op++ {
		k := keys[r.IntN(len(keys))]
		i, found := find(k)
		switch r.IntN(4) {
		case 0, 1:
			v := r.Int()
			old, replaced := tr.Insert(k, v)
			mold, mreplaced := m.Insert(k, v)
			if replaced != found || mreplaced != found || (found && (old != values[index(k)] || mold != old)) {
				t.Fatalf("insert %v: replaced %v/%v, want %v", k, replaced, mreplaced, found)
			}
			if !found {
				ref = slices.Insert(ref, i, k)
			}
			values[index(k)] = v
		case 2:
			old, deleted := tr.Delete(k)
			mold, mdeleted := m.Delete(k)
			if deleted != found || mdeleted != found || (found && (old != values[index(k)] || mold != old)) {
				t.Fatalf("delete %v: %v/%v, want %v", k, deleted, mdeleted, found)
			}
			if found {
				ref = slices.Delete(ref, i, i+1)
			}
		default:
			v, ok := tr.Get(k)
			mv, mok := m.Get(k)
			if ok != found || mok != found || (found && (v != values[index(k)] || mv != v)) {
				t.Fatalf("get %v: %v/%v, want %v", k, ok, mok, found)
			}
			lower, _, ok := tr.LowerBound(k)
			if ok != (i < len(ref)) || (ok && lower.Compare(ref[i]) != 0) {
				t.Fatalf("lower bound %v", k)
			}
		}
	}
	checkCore(t, &tr.treeCore)
	if tr.words != nil || m.words.Load() != nil {
		t.Fatal("custom keys reached a numeric arena")
	}
	if tr.Len() != len(ref) || m.Len() != len(ref) {
		t.Fatalf("len %d/%d, want %d", tr.Len(), m.Len(), len(ref))
	}
	got := slices.Collect(tr.Keys())
	var mgot []K
	for k := range m.All() {
		mgot = append(mgot, k)
	}
	for i := range ref {
		if got[i].Compare(ref[i]) != 0 || mgot[i].Compare(ref[i]) != 0 {
			t.Fatalf("order at %d: %v/%v, want %v", i, got[i], mgot[i], ref[i])
		}
	}
	if len(ref) > 2 {
		lo, hi := ref[len(ref)/4], ref[3*len(ref)/4]
		var scanned []K
		m.Scan(lo, hi, func(k K, _ int) bool { scanned = append(scanned, k); return true })
		if len(scanned) != 3*len(ref)/4-len(ref)/4 {
			t.Fatalf("scan visited %d", len(scanned))
		}
	}
}

func distinctStrings(r *rand.Rand, n int, prefix string) []string {
	seen := map[string]bool{}
	var out []string
	for len(out) < n {
		s := fmt.Sprintf("%s%x", prefix, r.Uint64()>>r.IntN(64))
		if r.IntN(8) == 0 {
			s += "\x00"
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func TestCustomKeysMatchReference(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	t.Run("text", func(t *testing.T) { checkOrdered(t, textKeys(distinctStrings(r, 3000, "")), 30000, 1) })
	t.Run("long prefix", func(t *testing.T) {
		checkOrdered(t, textKeys(distinctStrings(r, 3000, strings.Repeat("/tenant/shared", 8))), 30000, 2)
	})
	t.Run("desc", func(t *testing.T) {
		checkOrdered(t, mapSlice(make([]int, 3000), func(int) descKey { return descKey(r.Uint64()) }), 30000, 3)
	})
	t.Run("tenant", func(t *testing.T) {
		names := distinctStrings(r, 600, "user-")
		var keys []tenantKey
		for tenant := uint32(0); tenant < 5; tenant++ {
			for _, n := range names {
				keys = append(keys, tenantKey{tenant << 24 >> uint(tenant), n})
			}
		}
		checkOrdered(t, keys, 30000, 4)
	})
	t.Run("ties", func(t *testing.T) {
		checkOrdered(t, mapSlice(make([]int, 2000), func(int) tieKey { return tieKey(r.IntN(1 << 30)) }), 20000, 5)
	})
}

func TestCustomKeysMatchStringTree(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 9))
	words := distinctStrings(r, 5000, "k")
	var want Tree[string, int]
	var got TreeOf[textKey, int]
	for i, w := range words {
		want.Insert(w, i)
		got.Insert(textKey{w}, i)
	}
	for _, w := range words[:2500] {
		want.Delete(w)
		got.Delete(textKey{w})
	}
	next, stop := iter.Pull2(got.All())
	defer stop()
	for k, v := range want.All() {
		gk, gv, ok := next()
		if !ok || gk.s != k || gv != v {
			t.Fatalf("got %q=%d, want %q=%d", gk.s, gv, k, v)
		}
	}
	if _, _, ok := next(); ok {
		t.Fatal("extra entries")
	}
}

func TestDescKeysIterateInReverse(t *testing.T) {
	s := NewSetOf[descKey]()
	for i := descKey(0); i < 1000; i++ {
		s.Insert(i)
	}
	if first, _ := s.First(); first != 999 {
		t.Fatalf("first %d", first)
	}
	prev := descKey(1000)
	for k := range s.All() {
		if k >= prev {
			t.Fatalf("%d after %d", k, prev)
		}
		prev = k
	}
}

func TestFoldKeysKeepStoredKey(t *testing.T) {
	tr := NewTreeOf[foldKey, int]()
	tr.Insert("Bob", 1)
	if old, replaced := tr.Insert("bOB", 2); !replaced || old != 1 {
		t.Fatalf("replace returned %d %v", old, replaced)
	}
	if k, v, _ := tr.GetKeyValue("BOB"); k != "Bob" || v != 2 || tr.Len() != 1 {
		t.Fatalf("stored %q=%d len %d", k, v, tr.Len())
	}
}

func TestTenantKeyWindowMatchesEncoding(t *testing.T) {
	k := tenantKey{0x01020304, "abcdefghij"}
	encoding := append([]byte{1, 2, 3, 4}, k.name...)
	for offset := 0; offset < len(encoding)+9; offset++ {
		if got, want := k.Window(offset), BytesWindow(encoding, offset); got != want {
			t.Fatalf("offset %d: %x, want %x", offset, got, want)
		}
	}
}

func TestCustomSetAlgebra(t *testing.T) {
	var a, b SetOf[foldKey]
	for _, k := range []foldKey{"a", "B", "c"} {
		a.Insert(k)
	}
	for _, k := range []foldKey{"b", "C", "d"} {
		b.Insert(k)
	}
	if got := slices.Collect(a.Intersection(&b)); !slices.Equal(got, []foldKey{"B", "c"}) {
		t.Fatalf("intersection %q", got)
	}
	if got := slices.Collect(a.SymmetricDifference(&b)); !slices.Equal(got, []foldKey{"a", "d"}) {
		t.Fatalf("symmetric difference %q", got)
	}
	upper := a.SplitOff("b")
	if a.Len() != 1 || upper.Len() != 2 || !upper.IsSubset(&b) {
		t.Fatal("split off")
	}
}

func TestCustomMapConcurrent(t *testing.T) {
	m := NewMapOf[tenantKey, int]()
	const writers, per = 4, 2000
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range per {
				m.Insert(tenantKey{uint32(w), fmt.Sprintf("name-%06d", i)}, i)
				if i%3 == 0 {
					m.Delete(tenantKey{uint32(w), fmt.Sprintf("name-%06d", i/2)})
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := range per {
				if v, ok := m.Get(tenantKey{uint32(w), fmt.Sprintf("name-%06d", i)}); ok && v != i {
					t.Errorf("read %d for %d", v, i)
					return
				}
			}
		}()
	}
	wg.Wait()
	var prev *tenantKey
	n := 0
	for k := range m.All() {
		if prev != nil && prev.Compare(k) >= 0 {
			t.Fatalf("%v after %v", k, *prev)
		}
		prev = &k
		n++
	}
	if n != m.Len() {
		t.Fatalf("iterated %d, len %d", n, m.Len())
	}
}
