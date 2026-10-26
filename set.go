package gobbtree

import (
	"cmp"
	"iter"
)

// Set is an ordered set for a single owner, corresponding to upstream's
// unsync::BBTreeSet. Its zero value is ready to use. Do not copy it after use.
type Set[K cmp.Ordered] struct{ tree Tree[K, struct{}] }

func NewSet[K cmp.Ordered]() *Set[K]  { return new(Set[K]) }
func (s *Set[K]) Len() int            { return s.tree.Len() }
func (s *Set[K]) IsEmpty() bool       { return s.tree.IsEmpty() }
func (s *Set[K]) Clear()              { s.tree.Clear() }
func (s *Set[K]) Contains(key K) bool { return s.tree.Contains(key) }
func (s *Set[K]) Get(key K) (K, bool) { k, _, ok := s.tree.GetKeyValue(key); return k, ok }
func (s *Set[K]) Insert(key K) bool   { _, found := s.tree.Insert(key, struct{}{}); return !found }
func (s *Set[K]) Delete(key K) bool   { _, found := s.tree.Delete(key); return found }

func (s *Set[K]) Take(key K) (K, bool) {
	old, ok := s.Get(key)
	if ok {
		s.Delete(key)
	}
	return old, ok
}

// Replace adds key, replacing the stored key when an equal key exists.
func (s *Set[K]) Replace(key K) (K, bool) { old, ok := s.Take(key); s.Insert(key); return old, ok }

func (s *Set[K]) First() (K, bool)    { k, _, ok := s.tree.First(); return k, ok }
func (s *Set[K]) Last() (K, bool)     { k, _, ok := s.tree.Last(); return k, ok }
func (s *Set[K]) PopFirst() (K, bool) { k, _, ok := s.tree.PopFirst(); return k, ok }
func (s *Set[K]) PopLast() (K, bool)  { k, _, ok := s.tree.PopLast(); return k, ok }
func (s *Set[K]) All() iter.Seq[K]    { return s.tree.Keys() }
func (s *Set[K]) Reverse() iter.Seq[K] {
	return func(yield func(K) bool) {
		for k := range s.tree.Reverse() {
			if !yield(k) {
				return
			}
		}
	}
}
func (s *Set[K]) Scan(lower, upper K, visit func(K) bool) {
	s.tree.Scan(lower, upper, func(k K, _ struct{}) bool { return visit(k) })
}
func (s *Set[K]) ScanReverse(lower, upper K, visit func(K) bool) {
	s.tree.ScanReverse(lower, upper, func(k K, _ struct{}) bool { return visit(k) })
}

func (s *Set[K]) ContainsMany(keys []K, dst []bool) {
	if len(dst) < len(keys) {
		panic("gobbtree: ContainsMany destination too short")
	}
	for i, k := range keys {
		dst[i] = s.Contains(k)
	}
}

func (s *Set[K]) InsertMany(keys []K, dst []bool) {
	if len(dst) < len(keys) {
		panic("gobbtree: InsertMany destination too short")
	}
	for i, k := range keys {
		dst[i] = s.Insert(k)
	}
}

func (s *Set[K]) Clone() *Set[K]         { return &Set[K]{tree: *s.tree.Clone()} }
func (s *Set[K]) Append(other *Set[K])   { s.tree.Append(&other.tree) }
func (s *Set[K]) SplitOff(key K) *Set[K] { return &Set[K]{tree: *s.tree.SplitOff(key)} }
func (s *Set[K]) Retain(keep func(K) bool) {
	s.tree.Retain(func(k K, _ struct{}) bool { return keep(k) })
}
func (s *Set[K]) ExtractIf(lower, upper K, predicate func(K) bool) iter.Seq[K] {
	return func(yield func(K) bool) {
		for k := range s.tree.ExtractIf(lower, upper, func(k K, _ struct{}) bool { return predicate(k) }) {
			if !yield(k) {
				return
			}
		}
	}
}
func (s *Set[K]) WriteFile(path string) error { return s.tree.WriteFile(path) }

func (s *Set[K]) Union(other *Set[K]) iter.Seq[K]               { return s.merge(other, 7) }
func (s *Set[K]) Intersection(other *Set[K]) iter.Seq[K]        { return s.merge(other, 4) }
func (s *Set[K]) Difference(other *Set[K]) iter.Seq[K]          { return s.merge(other, 1) }
func (s *Set[K]) SymmetricDifference(other *Set[K]) iter.Seq[K] { return s.merge(other, 3) }

// The two sorted streams are merged lazily; bits select left-only, right-only,
// and common elements. Early termination stops both source iterators.
func (s *Set[K]) merge(other *Set[K], selectKinds uint8) iter.Seq[K] {
	return func(yield func(K) bool) {
		nextA, stopA := iter.Pull(s.All())
		defer stopA()
		nextB, stopB := iter.Pull(other.All())
		defer stopB()
		a, hasA := nextA()
		b, hasB := nextB()
		for hasA || hasB {
			kind := uint8(4)
			if !hasB || (hasA && cmp.Compare(a, b) < 0) {
				kind = 1
			} else if !hasA || cmp.Compare(a, b) > 0 {
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

func (s *Set[K]) IsSubset(other *Set[K]) bool {
	if s.Len() > other.Len() {
		return false
	}
	for range s.Difference(other) {
		return false
	}
	return true
}
func (s *Set[K]) IsSuperset(other *Set[K]) bool { return other.IsSubset(s) }
func (s *Set[K]) IsDisjoint(other *Set[K]) bool {
	for range s.Intersection(other) {
		return false
	}
	return true
}
