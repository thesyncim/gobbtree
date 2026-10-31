package store

import (
	"errors"
	"iter"
	"math"

	"github.com/thesyncim/gobbtree"
)

// Store is an ordered map of byte strings to byte strings that goroutines
// share, whose keys and values lie in pages its budget counts: each leaf's
// records in heap pages of its own, and each value too long for those in
// overflow pages of its own, all of the store's heap page size. Upstream's
// BBTreeStore (store.rs).
//
//   - An insert writes the value's overflow pages, if it is too long for its
//     record, then finds the key's leaf, held exclusively: a new key's record
//     goes into the leaf's heap, and the key goes in pointing to it; a held
//     key's record takes the new value in place if it fits, or the key
//     points to a new record, and the old one dies. An insert that fails
//     gives the overflow pages back.
//   - A removal takes the key out, while the map holds the leaf; its record
//     dies once the map is done with the key, after the removal.
//   - A lookup or a scan reads records inside the map's callbacks, while it
//     holds their leaves latched shared. As only a goroutine holding a
//     record's leaf exclusively moves, changes or kills it, no record goes
//     while a goroutine reads it.
//
// Nothing it hands out outlives a call: a lookup copies the value out
// (GetInto), and a scan's loop body sees each entry while its leaf is
// latched. The zero Store is not ready to use; New makes one.
type Store struct {
	m    storeMap
	heap recordHeap
}

// Budget is a store's allocator, upstream's Allocator for heap pages: the
// store asks it before it takes pages and tells it when it gives them
// back, each of the store's heap page size. A budget that refuses pages
// past a limit bounds the store's pages; refused, an insert fails with
// ErrAlloc, the store as it was. The pages are Go's, which the collector
// frees once nothing points into them; the map's nodes are the
// collector's too, uncounted. Its methods must be safe to call from many
// goroutines.
type Budget interface {
	// TakePages reports whether the store may take n more pages.
	TakePages(n int) bool
	// GivePages tells the budget that the store gave n pages back.
	GivePages(n int)
}

// unlimited is the budget that never refuses, a store's by default.
type unlimited struct{}

func (unlimited) TakePages(int) bool { return true }
func (unlimited) GivePages(int)      {}

// Config is a store's settings, upstream's node config's heap page size.
type Config struct {
	// HeapPageSize is the size of the store's heap and overflow pages, a
	// power of two from 1 KiB to 64 KiB; 0 for DefaultHeapPageSize.
	HeapPageSize int
}

// Errors an insert fails with, upstream's InsertError; the store holds
// what it held.
var (
	// ErrKeyTooLong is upstream's InsertError::KeyTooLong: the key is longer
	// than MaxKeyLen.
	ErrKeyTooLong = errors.New("store: the key is longer than a store's keys may be")
	// ErrValueTooLong is upstream's InsertError::ValueTooLong: the value is
	// longer than MaxValueLen.
	ErrValueTooLong = errors.New("store: the value is longer than a store's values may be")
	// ErrAlloc is upstream's InsertError::Alloc: the budget had no page to
	// give.
	ErrAlloc = errors.New("store: the budget had no page to give")
)

// New makes a new, empty store with no budget, of the default heap page
// size.
func New() *Store { return NewWithConfig(Config{}, nil) }

// NewIn makes a new, empty store whose pages budget counts, of the default
// heap page size.
func NewIn(budget Budget) *Store { return NewWithConfig(Config{}, budget) }

// NewWithConfig makes a new, empty store of config's heap page size, whose
// pages budget counts; nil for none.
func NewWithConfig(config Config, budget Budget) *Store {
	size := config.HeapPageSize
	if size == 0 {
		size = DefaultHeapPageSize
	}
	if budget == nil {
		budget = unlimited{}
	}
	return &Store{heap: recordHeap{g: newGeometry(size), budget: budget}}
}

// MaxKeyLen is the longest key: a quarter of a heap page. A key's bytes lie
// together in its record, which fits a page's room.
func (s *Store) MaxKeyLen() int { return s.heap.g.pageSize / 4 }

// MaxValueLen is the longest value: math.MaxUint32 bytes, its length's
// limit, or less where an int can't hold that.
func (s *Store) MaxValueLen() int { return int(min(math.MaxUint32, uint64(math.MaxInt))) }

// GetInto appends the value of key to buf and returns it, with whether the
// store holds key; buf unchanged without it.
func (s *Store) GetInto(key, buf []byte) ([]byte, bool) {
	var q queryKey
	found := s.m.Read(newQuery(&q, key), func(stored storeKey, _ struct{}) {
		buf = s.heap.value(stored.record()).appendTo(&s.heap.g, buf)
	})
	return buf, found
}

// Get is a copy of the value of key, with whether the store holds key.
func (s *Store) Get(key []byte) ([]byte, bool) {
	value, found := s.GetInto(key, nil)
	if found && value == nil {
		value = []byte{}
	}
	return value, found
}

// Contains reports whether the store holds key.
func (s *Store) Contains(key []byte) bool {
	var q queryKey
	return s.m.Contains(newQuery(&q, key))
}

// Insert inserts key with value, or gives the key it holds already value in
// place of its own, and reports whether it replaced a value. Both are
// copied into the store's pages.
//
// It fails with ErrKeyTooLong if the key is longer than MaxKeyLen, with
// ErrValueTooLong if the value is longer than MaxValueLen, and with
// ErrAlloc if the budget has no page for the insert. The store holds what
// it held then.
func (s *Store) Insert(key, value []byte) (replaced bool, err error) {
	if len(key) > s.MaxKeyLen() {
		return false, ErrKeyTooLong
	}
	if uint64(len(value)) > uint64(s.MaxValueLen()) {
		return false, ErrValueTooLong
	}
	h := &s.heap
	link, ok := h.writeOverflow(len(key), value)
	if !ok {
		return false, ErrAlloc
	}
	var q queryKey
	found, ok := s.m.UpdateOrInsertWithLeaf(newQuery(&q, key), struct{}{},
		func(leaf storeLeaf, stored storeKey, _ *struct{}) bool {
			return h.overwrite(leaf, indexOf(leaf, stored), key, value, link)
		},
		func(leaf storeLeaf, query storeKey, _ struct{}) (storeKey, bool) {
			r, ok := h.write(leaf, key, value, link)
			if !ok {
				return query, false
			}
			// The record has the new key's bytes, and the key goes into
			// the leaf, which the map holds exclusively, next.
			return storeKey{r.at}, true
		})
	if !ok {
		// No record links the overflow pages.
		h.freeOverflow(link)
		return false, ErrAlloc
	}
	return found, nil
}

// indexOf is the index of stored among leaf's keys, by its pointer.
func indexOf(leaf storeLeaf, stored storeKey) int {
	for i, k := range leaf.Keys() {
		if k.p == stored.p {
			return i
		}
	}
	panic("store: a held key is among its leaf's keys")
}

// Remove removes key, and reports whether the store held it.
func (s *Store) Remove(key []byte) bool {
	var q queryKey
	stored, _, ok := s.m.DeleteWithLeaf(newQuery(&q, key), func(leaf storeLeaf, stored storeKey, _ struct{}) {
		s.heap.beforeRemoval(leaf, stored)
	})
	if !ok {
		return false
	}
	// The map reads the key until the removal returns: its record dies only
	// now.
	s.heap.killRecord(stored.record())
	return true
}

// Scan iterates the entries whose keys lie in b, in key order, with their
// key and value, running the loop body while each entry's leaf is latched
// shared, as upstream's scan runs its closure: the body must not use the
// store. A value with overflow pages is copied into a buffer the scan
// keeps, for the body to see in one piece. Both slices are valid only
// until the body returns. As a map's scan, it is not a snapshot.
//
// It panics as upstream's does, as std's BTreeMap::range: if b starts after
// it ends, or starts and ends at the same key, both excluded.
func (s *Store) Scan(b Bounds) iter.Seq2[[]byte, []byte] {
	return func(yield func(key, value []byte) bool) { s.scan(b, false, yield) }
}

// ScanReverse is Scan in reverse key order: from the end of b to its start.
func (s *Store) ScanReverse(b Bounds) iter.Seq2[[]byte, []byte] {
	return func(yield func(key, value []byte) bool) { s.scan(b, true, yield) }
}

func (s *Store) scan(b Bounds, reverse bool, yield func(key, value []byte) bool) {
	var lower, upper queryKey
	entries := s.m.Scan(b.storeBounds(&lower, &upper))
	if reverse {
		entries = s.m.ScanReverse(b.storeBounds(&lower, &upper))
	}
	var gathered []byte
	for stored := range entries {
		r := stored.record()
		if !yield(r.key(), s.heap.value(r).gather(&s.heap.g, &gathered)) {
			return
		}
	}
}

// Range is copies of the entries whose keys lie in b, key and value, in
// key order from the front, in reverse from the back, a few at a time
// (RangeCopies). As Scan, it is not a snapshot.
//
// It panics as Scan does.
func (s *Store) Range(b Bounds) *RangeCopies {
	// Checks the range as a scan does, copying nothing.
	for range s.Scan(b) {
		break
	}
	return newRangeCopies(s, b.owned())
}

// Clear removes every entry and gives every page back to the budget, as
// dropping upstream's store does, and leaves the store empty. It is the
// owner's: no other goroutine may use the store until it returns.
func (s *Store) Clear() {
	h := &s.heap
	// The chains go with the map's leaves. Orphans and overflow pages go as
	// their records die, which only killing them all does.
	if h.killsAtClear() {
		for stored := range s.m.All() {
			h.killRecord(stored.record())
		}
	}
	s.m.Clear()
	h.overflowed.Store(false)
}

// storeBounds is b over store keys that query b's keys, valid while lower
// and upper are.
func (b *Bounds) storeBounds(lower, upper *queryKey) gobbtree.Bounds[storeKey] {
	var sb gobbtree.Bounds[storeKey]
	switch b.lowerKind {
	case included:
		sb = gobbtree.AtLeast(newQuery(lower, b.lower))
	case excluded:
		sb = gobbtree.Above(newQuery(lower, b.lower))
	}
	switch b.upperKind {
	case included:
		sb = sb.AtMost(newQuery(upper, b.upper))
	case excluded:
		sb = sb.Below(newQuery(upper, b.upper))
	}
	return sb
}
