package gobbtree

import (
	"cmp"
	"slices"
	"testing"
)

func TestCloneEqualCompare(t *testing.T) {
	same := func(a, b string) bool { return a == b }
	a := NewTree[int, string]()
	for i := range 2000 {
		a.Insert(i*3, "v")
	}
	b := a.Clone()
	if !a.EqualFunc(b, same) || a.CompareFunc(b, cmp.Compare) != 0 {
		t.Fatal("clone differs")
	}
	b.Insert(1, "x")
	if a.Len() != 2000 || a.Contains(1) {
		t.Fatal("clone shares state")
	}
	if a.EqualFunc(b, same) || a.CompareFunc(b, cmp.Compare) <= 0 || b.CompareFunc(a, cmp.Compare) >= 0 {
		t.Fatal("key 1 orders b first")
	}
	b.Delete(1)
	b.Insert(0, "w")
	if a.EqualFunc(b, same) || a.CompareFunc(b, cmp.Compare) >= 0 {
		t.Fatal("value w orders b last")
	}
	b.Insert(0, "v")
	b.Insert(1<<20, "v")
	if a.CompareFunc(b, cmp.Compare) >= 0 || b.CompareFunc(a, cmp.Compare) <= 0 {
		t.Fatal("prefix orders first")
	}

	s := NewSet[string]()
	s.InsertAll(slices.Values([]string{"b", "a", "c"}))
	c := s.Clone()
	if !s.Equal(c) || s.Compare(c) != 0 || s.String() != "[a b c]" {
		t.Fatalf("set clone %v", c)
	}
	c.Delete("c")
	if s.Equal(c) || s.Compare(c) <= 0 {
		t.Fatal("set prefix")
	}
	f := NewSetOf[foldKey]()
	f.Insert("A")
	g := f.Clone()
	g.Insert("a")
	if !f.Equal(g) {
		t.Fatal("custom keys equal by Compare")
	}
	if got := a.Clone().String(); got[:12] != "map[0:v 3:v " {
		t.Fatalf("format %q", got[:12])
	}
}

func TestDrain(t *testing.T) {
	tr := NewTree[int, int]()
	m := New[string, int]()
	s := NewSet[int]()
	for i := range 500 {
		tr.Insert(i, i)
		m.Insert(hexKey(uint64(i)), i)
		s.Insert(i)
	}
	seq := tr.Drain()
	if tr.Len() != 0 {
		t.Fatal("tree not emptied")
	}
	tr.Insert(-1, -1)
	var got []int
	for k := range seq {
		got = append(got, k)
		if len(got) == 10 {
			break
		}
	}
	if !slices.Equal(got, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}) || tr.Len() != 1 {
		t.Fatalf("drained %v, %d left", got, tr.Len())
	}
	n := 0
	for k, v := range m.Drain() {
		if unhexKey(k) != uint64(v) || v != n {
			t.Fatalf("map drain %s %d", k, v)
		}
		if n++; n == 100 {
			break
		}
	}
	if seqLen(m.All()) != 0 {
		t.Fatalf("map kept %d", seqLen(m.All()))
	}
	if keys := slices.Collect(s.Drain()); len(keys) != 500 || s.Len() != 0 {
		t.Fatal("set drain")
	}
}

func TestEntryAPI(t *testing.T) {
	tr := NewTree[string, int]()
	if v := tr.Entry("a").OrInsert(1); v != 1 {
		t.Fatal("or insert vacant")
	}
	if v := tr.Entry("a").OrInsert(9); v != 1 {
		t.Fatal("or insert occupied")
	}
	if v := tr.Entry("a").AndModify(func(v int) int { return v + 10 }).OrInsert(0); v != 11 {
		t.Fatalf("and modify %d", v)
	}
	if v := tr.Entry("b").AndModify(func(v int) int { return v + 10 }).OrInsertWithKey(func(k string) int { return len(k) }); v != 1 {
		t.Fatal("or insert with key")
	}
	if v := tr.Entry("c").OrDefault(); v != 0 || !tr.Contains("c") {
		t.Fatal("or default")
	}
	e := tr.Entry("d")
	if e.Occupied() || e.Key() != "d" {
		t.Fatal("vacant")
	}
	if e = e.InsertEntry(4); !e.Occupied() {
		t.Fatal("insert entry")
	}
	if old, ok := tr.Entry("d").Insert(5); !ok || old != 4 {
		t.Fatal("occupied insert")
	}
	first, ok := tr.FirstEntry()
	if !ok || first.Key() != "a" {
		t.Fatal("first entry")
	}
	if k, v, ok := first.Remove(); !ok || k != "a" || v != 11 || tr.Contains("a") {
		t.Fatal("remove")
	}
	if last, ok := tr.LastEntry(); !ok || last.Key() != "d" {
		t.Fatal("last entry")
	}
	ft := NewTreeOf[foldKey, int]()
	ft.Insert("Bob", 1)
	if k := ft.Entry("BOB").Key(); k != "Bob" {
		t.Fatalf("stored key %q", k)
	}
}
