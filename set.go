package gobbtree

import (
	"cmp"
	"iter"
)

// Set is an ordered set for a single owner, corresponding to upstream's
// unsync::BBTreeSet. Its zero value is ready to use. Do not copy it after use.
type Set[K cmp.Ordered] struct{ setCore[K, natural[K]] }

// setCore implements Set and SetOf for one key ordering.
type setCore[K any, O keyOrder[K]] struct{ tree treeCore[K, struct{}, O] }

func NewSet[K cmp.Ordered]() *Set[K] { return new(Set[K]) }

func (s *Set[K]) Append(other *Set[K])                          { s.tree.appendTree(&other.tree) }
func (s *Set[K]) SplitOff(key K) *Set[K]                        { return &Set[K]{*s.splitOff(key)} }
func (s *Set[K]) Union(other *Set[K]) iter.Seq[K]               { return s.merge(&other.setCore, 7) }
func (s *Set[K]) Intersection(other *Set[K]) iter.Seq[K]        { return s.merge(&other.setCore, 4) }
func (s *Set[K]) Difference(other *Set[K]) iter.Seq[K]          { return s.merge(&other.setCore, 1) }
func (s *Set[K]) SymmetricDifference(other *Set[K]) iter.Seq[K] { return s.merge(&other.setCore, 3) }
func (s *Set[K]) IsSubset(other *Set[K]) bool                   { return s.isSubset(&other.setCore) }
func (s *Set[K]) IsSuperset(other *Set[K]) bool                 { return other.isSubset(&s.setCore) }
func (s *Set[K]) IsDisjoint(other *Set[K]) bool                 { return s.isDisjoint(&other.setCore) }
func (s *Set[K]) WriteFile(path string) error                   { return CreateFile(path, s.tree.All()) }

func (s *setCore[K, O]) Len() int            { return s.tree.Len() }
func (s *setCore[K, O]) IsEmpty() bool       { return s.tree.IsEmpty() }
func (s *setCore[K, O]) Clear()              { s.tree.Clear() }
func (s *setCore[K, O]) Contains(key K) bool { return s.tree.Contains(key) }
func (s *setCore[K, O]) Get(key K) (K, bool) { k, _, ok := s.tree.GetKeyValue(key); return k, ok }
func (s *setCore[K, O]) Insert(key K) bool   { _, found := s.tree.Insert(key, struct{}{}); return !found }
func (s *setCore[K, O]) Delete(key K) bool   { _, found := s.tree.Delete(key); return found }

func (s *setCore[K, O]) Take(key K) (K, bool) {
	old, ok := s.Get(key)
	if ok {
		s.Delete(key)
	}
	return old, ok
}

// Replace adds key, replacing the stored key when an equal key exists.
func (s *setCore[K, O]) Replace(key K) (K, bool) {
	old, ok := s.Take(key)
	s.Insert(key)
	return old, ok
}

func (s *setCore[K, O]) First() (K, bool)    { k, _, ok := s.tree.First(); return k, ok }
func (s *setCore[K, O]) Last() (K, bool)     { k, _, ok := s.tree.Last(); return k, ok }
func (s *setCore[K, O]) PopFirst() (K, bool) { k, _, ok := s.tree.PopFirst(); return k, ok }
func (s *setCore[K, O]) PopLast() (K, bool)  { k, _, ok := s.tree.PopLast(); return k, ok }
func (s *setCore[K, O]) All() iter.Seq[K]    { return s.tree.Keys() }
func (s *setCore[K, O]) Reverse() iter.Seq[K] {
	return func(yield func(K) bool) {
		for k := range s.tree.Reverse() {
			if !yield(k) {
				return
			}
		}
	}
}

// Range iterates the keys in [lower, upper), ascending.
func (s *setCore[K, O]) Range(lower, upper K) iter.Seq[K] { return setKeys(s.tree.Range(lower, upper)) }

// RangeReverse iterates the keys in [lower, upper), descending.
func (s *setCore[K, O]) RangeReverse(lower, upper K) iter.Seq[K] {
	return setKeys(s.tree.RangeReverse(lower, upper))
}

// Scan iterates the keys in b, ascending, as upstream's range does.
func (s *setCore[K, O]) Scan(b Bounds[K]) iter.Seq[K] { return setKeys(s.tree.Scan(b)) }

// ScanReverse is Scan in descending order.
func (s *setCore[K, O]) ScanReverse(b Bounds[K]) iter.Seq[K] { return setKeys(s.tree.ScanReverse(b)) }

// Ascend iterates the keys at or above from, ascending.
func (s *setCore[K, O]) Ascend(from K) iter.Seq[K] { return setKeys(s.tree.Ascend(from)) }

// Descend iterates the keys at or below from, descending.
func (s *setCore[K, O]) Descend(from K) iter.Seq[K] { return setKeys(s.tree.Descend(from)) }

func setKeys[K any](entries iter.Seq2[K, struct{}]) iter.Seq[K] {
	return func(yield func(K) bool) {
		for k := range entries {
			if !yield(k) {
				return
			}
		}
	}
}

func (s *setCore[K, O]) ContainsMany(keys []K, dst []bool) {
	if len(dst) < len(keys) {
		panic("gobbtree: ContainsMany destination too short")
	}
	for i, k := range keys {
		dst[i] = s.Contains(k)
	}
}

func (s *setCore[K, O]) InsertMany(keys []K, dst []bool) {
	if len(dst) < len(keys) {
		panic("gobbtree: InsertMany destination too short")
	}
	for i, k := range keys {
		dst[i] = s.Insert(k)
	}
}

func (s *setCore[K, O]) splitOff(key K) *setCore[K, O] {
	return &setCore[K, O]{tree: *s.tree.splitOff(key)}
}
func (s *setCore[K, O]) Retain(keep func(K) bool) {
	s.tree.Retain(func(k K, _ struct{}) bool { return keep(k) })
}
func (s *setCore[K, O]) ExtractIf(b Bounds[K], predicate func(K) bool) iter.Seq[K] {
	extracted := s.tree.ExtractIf(b, func(k K, _ struct{}) bool { return predicate(k) })
	return func(yield func(K) bool) {
		for k := range extracted {
			if !yield(k) {
				return
			}
		}
	}
}

// The two sorted streams are merged lazily; bits select left-only, right-only,
// and common elements. Early termination stops both source iterators.
func (s *setCore[K, O]) merge(other *setCore[K, O], selectKinds uint8) iter.Seq[K] {
	return func(yield func(K) bool) {
		nextA, stopA := iter.Pull(s.All())
		defer stopA()
		nextB, stopB := iter.Pull(other.All())
		defer stopB()
		a, hasA := nextA()
		b, hasB := nextB()
		for hasA || hasB {
			kind := uint8(4)
			if !hasB || (hasA && compareKeys[O](a, b) < 0) {
				kind = 1
			} else if !hasA || compareKeys[O](a, b) > 0 {
				kind = 2
			}
			key := a
			if kind == 2 {
				key = b
			}
			if selectKinds&kind != 0 && !yield(key) {
				return
			}
			if kind != 2 {
				a, hasA = nextA()
			}
			if kind != 1 {
				b, hasB = nextB()
			}
		}
	}
}

func (s *setCore[K, O]) isSubset(other *setCore[K, O]) bool {
	if s.Len() > other.Len() {
		return false
	}
	for range s.merge(other, 1) {
		return false
	}
	return true
}
func (s *setCore[K, O]) isDisjoint(other *setCore[K, O]) bool {
	for range s.merge(other, 4) {
		return false
	}
	return true
}

// InsertAll inserts every key of seq, as upstream's Extend does.
func (s *setCore[K, O]) InsertAll(seq iter.Seq[K]) {
	for k := range seq {
		s.Insert(k)
	}
}
