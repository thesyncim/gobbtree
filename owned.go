package gobbtree

import (
	"fmt"
	"iter"
	"strings"
)

// Operations upstream's single-owner map and set offer as traits: Clone,
// PartialEq/Eq, PartialOrd/Ord, Debug and consuming iteration (IntoIterator).

// Clone returns an independent copy of t, as upstream's Clone does. Keys
// and values are copied as Insert copies them.
func (t *Tree[K, V]) Clone() *Tree[K, V] { return &Tree[K, V]{*t.clone()} }

// Clone returns an independent copy of t, as upstream's Clone does.
func (t *TreeOf[K, V]) Clone() *TreeOf[K, V] { return &TreeOf[K, V]{*t.clone()} }

// Clone returns an independent copy of s, as upstream's Clone does.
func (s *Set[K]) Clone() *Set[K] { return &Set[K]{setCore[K, natural[K]]{*s.tree.clone()}} }

// Clone returns an independent copy of s, as upstream's Clone does.
func (s *SetOf[K]) Clone() *SetOf[K] { return &SetOf[K]{setCore[K, custom[K]]{*s.tree.clone()}} }

func (t *treeCore[K, V, O]) clone() *treeCore[K, V, O] {
	out := new(treeCore[K, V, O])
	// Ascending inserts append to the rightmost leaf.
	for k, v := range t.All() {
		out.Insert(k, v)
	}
	return out
}

// EqualFunc reports whether t and other hold equal keys with values eq
// accepts, as upstream's PartialEq does.
func (t *Tree[K, V]) EqualFunc(other *Tree[K, V], eq func(V, V) bool) bool {
	return t.equalFunc(&other.treeCore, eq)
}

// EqualFunc reports whether t and other hold equal keys with values eq
// accepts, as upstream's PartialEq does.
func (t *TreeOf[K, V]) EqualFunc(other *TreeOf[K, V], eq func(V, V) bool) bool {
	return t.equalFunc(&other.treeCore, eq)
}

// CompareFunc orders t and other by their entries in key order, values
// compared by cmp, as upstream's Ord does: the first differing entry decides,
// and a prefix orders first.
func (t *Tree[K, V]) CompareFunc(other *Tree[K, V], cmp func(V, V) int) int {
	return t.compareFunc(&other.treeCore, cmp)
}

// CompareFunc orders t and other as Tree.CompareFunc does.
func (t *TreeOf[K, V]) CompareFunc(other *TreeOf[K, V], cmp func(V, V) int) int {
	return t.compareFunc(&other.treeCore, cmp)
}

// Equal reports whether s and other hold equal keys, as upstream's
// PartialEq does.
func (s *Set[K]) Equal(other *Set[K]) bool { return s.tree.equalFunc(&other.tree, sameUnit) }

// Equal reports whether s and other hold equal keys.
func (s *SetOf[K]) Equal(other *SetOf[K]) bool { return s.tree.equalFunc(&other.tree, sameUnit) }

// Compare orders s and other by their keys in order, as upstream's Ord
// does: the first differing key decides, and a prefix orders first.
func (s *Set[K]) Compare(other *Set[K]) int { return s.tree.compareFunc(&other.tree, compareUnit) }

// Compare orders s and other as Set.Compare does.
func (s *SetOf[K]) Compare(other *SetOf[K]) int {
	return s.tree.compareFunc(&other.tree, compareUnit)
}

func sameUnit(struct{}, struct{}) bool   { return true }
func compareUnit(struct{}, struct{}) int { return 0 }

func (t *treeCore[K, V, O]) equalFunc(other *treeCore[K, V, O], eq func(V, V) bool) bool {
	if t == other {
		return true
	}
	if t.length != other.length {
		return false
	}
	next, stop := iter.Pull2(other.All())
	defer stop()
	for k, v := range t.All() {
		ok, ov, _ := next()
		if compareKeys[O](k, ok) != 0 || !eq(v, ov) {
			return false
		}
	}
	return true
}

func (t *treeCore[K, V, O]) compareFunc(other *treeCore[K, V, O], cmp func(V, V) int) int {
	if t == other {
		return 0
	}
	next, stop := iter.Pull2(other.All())
	defer stop()
	for k, v := range t.All() {
		ok, ov, more := next()
		if !more {
			return 1
		}
		if c := compareKeys[O](k, ok); c != 0 {
			return c
		}
		if c := cmp(v, ov); c != 0 {
			return c
		}
	}
	if _, _, more := next(); more {
		return -1
	}
	return 0
}

// String formats t's entries in key order, as upstream's Debug does.
func (t *treeCore[K, V, O]) String() string { return formatEntries(t.All()) }

// String formats m's entries in key order, read like a scan, as upstream's
// Debug does.
func (m *mapCore[K, V, O]) String() string { return formatEntries(m.All()) }

// String formats s's keys in order, as upstream's Debug does.
func (s *setCore[K, O]) String() string {
	var b strings.Builder
	b.WriteByte('[')
	for k := range s.All() {
		if b.Len() > 1 {
			b.WriteByte(' ')
		}
		fmt.Fprint(&b, k)
	}
	b.WriteByte(']')
	return b.String()
}

func formatEntries[K, V any](entries iter.Seq2[K, V]) string {
	var b strings.Builder
	b.WriteString("map[")
	first := true
	for k, v := range entries {
		if !first {
			b.WriteByte(' ')
		}
		first = false
		fmt.Fprintf(&b, "%v:%v", k, v)
	}
	b.WriteByte(']')
	return b.String()
}

// Drain empties t and iterates the entries it held in key order, as
// upstream's into_iter does. Stopping early drops the rest, as dropping
// upstream's iterator does.
func (t *treeCore[K, V, O]) Drain() iter.Seq2[K, V] {
	held := new(treeCore[K, V, O])
	*held = *t
	t.Clear()
	return held.All()
}

// Drain empties s and iterates the keys it held in order, as upstream's
// into_iter does.
func (s *setCore[K, O]) Drain() iter.Seq[K] { return setKeys(s.tree.Drain()) }

// Drain removes and yields m's entries in key order, as upstream's into_iter
// does for an owned map. Each entry is removed atomically as it is yielded;
// stopping early clears the rest, as dropping upstream's iterator does.
// Entries other goroutines insert meanwhile may or may not be yielded.
func (m *mapCore[K, V, O]) Drain() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for key := range m.All() {
			k, v, ok := m.Take(key)
			if ok && !yield(k, v) {
				m.Clear()
				return
			}
		}
	}
}
