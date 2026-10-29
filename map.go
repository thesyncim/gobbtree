package gobbtree

import (
	"cmp"
	"iter"
)

// Map is a concurrent ordered map. Its zero value is ready to use.
//
// It shares one tree as upstream's BBTreeMap does: lookups descend
// optimistically, checking each node's version, writers latch only the
// leaf they change, and splits and merges take only the nodes they change.
// Word-sized keys and values have nodes of their own; other keys and values
// share the protocol over nodes of any type. Scans copy a leaf at a time and
// visit the copy with nothing held.
//
// Do not copy a Map after first use. Read, Update, UpdateOrInsert and DeleteIf
// callbacks run under the entry's latch and must not reenter the same Map.
// Clear is the owner's: no other goroutine may use the Map until it returns.
type Map[K cmp.Ordered, V any] struct{ mapCore[K, V, natural[K]] }

// mapCore implements Map and MapOf for one key ordering.
type mapCore[K any, V any, O keyOrder[K]] struct {
	generic genericTree[K, V, O]
}

// New creates an empty concurrent ordered map.
func New[K cmp.Ordered, V any]() *Map[K, V] { return new(Map[K, V]) }

// IsEmpty reports whether the map held no entry at some moment during the
// call, as upstream's is_empty.
func (m *mapCore[K, V, O]) IsEmpty() bool {
	return m.generic.isEmpty()
}

// Clear removes every entry. As upstream's clear, it is the owner's: no other
// goroutine may use the map until it returns.
func (m *mapCore[K, V, O]) Clear() {
	m.generic.clear()
}
func (m *mapCore[K, V, O]) Get(key K) (V, bool) { return m.generic.get(key) }

func (m *mapCore[K, V, O]) Contains(key K) bool { _, ok := m.Get(key); return ok }
func (m *mapCore[K, V, O]) Insert(key K, value V) (V, bool) {
	return m.generic.insert(key, value, insertReplace, nil)
}
func (m *mapCore[K, V, O]) Delete(key K) (V, bool) {
	_, v, ok := m.generic.remove(seekKey, key, nil)
	return v, ok
}

// Read calls visit under a leaf latch if key exists. Values are copies.
// A panic releases both latches before propagating to the caller.
func (m *mapCore[K, V, O]) Read(key K, visit func(K, V)) bool {
	return m.generic.read(key, visit)
}

// Update atomically replaces key's value with change(stored key, value), as
// upstream's update does, and returns the new value. A panic in change leaves
// the stored value unchanged.
func (m *mapCore[K, V, O]) Update(key K, change func(K, V) V) (V, bool) {
	return m.generic.update(key, change)
}

// UpdateOrInsert inserts value if key is absent, and otherwise stores
// merge(stored key, old, value), as upstream's update_or_insert does. It
// returns the old value and whether key was present. A panic in merge leaves
// the stored value unchanged.
func (m *mapCore[K, V, O]) UpdateOrInsert(key K, value V, merge func(key K, old, value V) V) (V, bool) {
	return m.generic.insert(key, value, insertMerge, merge)
}

// LoadOrStore returns key's value if present, and otherwise stores and
// returns value, as upstream's get_or_insert does. loaded reports that key
// was present.
func (m *mapCore[K, V, O]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	if old, found := m.generic.insert(key, value, insertKeep, nil); found {
		return old, true
	}
	return value, false
}

// DeleteIf removes key if condition accepts its stored key and value, as
// upstream's remove_if does, and returns them. condition may run more than
// once.
func (m *mapCore[K, V, O]) DeleteIf(key K, condition func(K, V) bool) (K, V, bool) {
	return m.generic.remove(seekKey, key, condition)
}

// Take removes key, returning the stored key and its value, as upstream's
// remove_entry does.
func (m *mapCore[K, V, O]) Take(key K) (K, V, bool) {
	return m.generic.remove(seekKey, key, nil)
}

// GetMany fills dst in query order. Each result is individually consistent;
// the batch is not a transaction. It uses no output allocations.
func (m *mapCore[K, V, O]) GetMany(keys []K, dst []Result[V]) {
	if len(dst) < len(keys) {
		panic("gobbtree: GetMany destination too short")
	}
	if len(keys) == 0 {
		return
	}
	if seqlockPlainStores && m.generic.wordCodes() {
		// Whole groups of eight go through the batched lookup.
		start := 0
		for ; start+batchLanes <= len(keys); start += batchLanes {
			m.generic.getMany8((*[batchLanes]K)(keys[start:]), (*[batchLanes]Result[V])(dst[start:]))
		}
		keys, dst = keys[start:], dst[start:]
	}
	for i, key := range keys {
		dst[i].Value, dst[i].OK = m.generic.get(key)
	}
}

// InsertMany applies entries in order. The batch is not a transaction.
func (m *mapCore[K, V, O]) InsertMany(entries []Pair[K, V], dst []Result[V]) {
	if len(dst) < len(entries) {
		panic("gobbtree: InsertMany destination too short")
	}
	for i, e := range entries {
		dst[i].Value, dst[i].OK = m.Insert(e.Key, e.Value)
	}
}

// Range iterates the entries with keys in [lower, upper), ascending, as
// Scan(Between(lower, upper)) does.
func (m *mapCore[K, V, O]) Range(lower, upper K) iter.Seq2[K, V] {
	return m.Scan(Between(lower, upper))
}

// RangeReverse is Range in descending order.
func (m *mapCore[K, V, O]) RangeReverse(lower, upper K) iter.Seq2[K, V] {
	return m.ScanReverse(Between(lower, upper))
}

// Ascend iterates the entries with keys at or above from, ascending.
func (m *mapCore[K, V, O]) Ascend(from K) iter.Seq2[K, V] { return m.Scan(AtLeast(from)) }

// Descend iterates the entries with keys at or below from, descending.
func (m *mapCore[K, V, O]) Descend(from K) iter.Seq2[K, V] { return m.ScanReverse(AtMost(from)) }

// Scan iterates the entries with keys in b, ascending, as upstream's scan
// does, with range_cloned's contract: a few leaves are copied under their
// latches, or validated without latches for numeric maps, and visited with
// no latch held, so the loop body may use the map. (Upstream's scan runs its
// closure under each leaf's read latch.) It is not a snapshot: an entry present
// throughout is visited exactly once; entries inserted or removed meanwhile
// may or may not be. Iterating it panics on bounds upstream rejects (see
// Bounds).
func (m *mapCore[K, V, O]) Scan(b Bounds[K]) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		checkBounds[K, O](&b)
		m.scan(&b, false, yield)
	}
}

// ScanReverse is Scan in descending order, as upstream's scan_rev.
func (m *mapCore[K, V, O]) ScanReverse(b Bounds[K]) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		checkBounds[K, O](&b)
		m.scan(&b, true, yield)
	}
}

// All iterates every entry in ascending order, as Scan does.
func (m *mapCore[K, V, O]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) { m.scan(&Bounds[K]{}, false, yield) }
}

// Reverse iterates every entry in descending order, as Scan does.
func (m *mapCore[K, V, O]) Reverse() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) { m.scan(&Bounds[K]{}, true, yield) }
}

// scan visits the keys in b.
func (m *mapCore[K, V, O]) scan(b *Bounds[K], reverse bool, visit func(K, V) bool) {
	m.generic.walk(b, reverse, visit)
}

// First returns the smallest entry, read like a scan.
func (m *mapCore[K, V, O]) First() (key K, value V, ok bool) {
	for key, value = range m.All() {
		return key, value, true
	}
	return key, value, false
}

// FirstIn returns the smallest entry with a key in b, read like a scan, as
// upstream's first_copied.
func (m *mapCore[K, V, O]) FirstIn(b Bounds[K]) (key K, value V, ok bool) {
	for key, value = range m.Scan(b) {
		return key, value, true
	}
	return key, value, false
}

// Last returns the largest entry, read like a scan.
func (m *mapCore[K, V, O]) Last() (key K, value V, ok bool) {
	m.generic.walk(&Bounds[K]{}, true, func(k K, v V) bool {
		key, value, ok = k, v, true
		return false
	})
	return key, value, ok
}
func (m *mapCore[K, V, O]) PopFirst() (K, V, bool) {
	var zero K
	return m.generic.remove(seekFirst, zero, nil)
}
func (m *mapCore[K, V, O]) PopLast() (K, V, bool) {
	var zero K
	return m.generic.remove(seekLast, zero, nil)
}

// ReadMany calls read with each present key's index, stored key and value,
// under that entry's latch, as upstream's read_many does. read must not use
// the map. Each read is consistent on its own; the batch is not a snapshot.
func (m *mapCore[K, V, O]) ReadMany(keys []K, read func(i int, key K, value V)) {
	for i, key := range keys {
		m.Read(key, func(k K, v V) { read(i, k, v) })
	}
}

// UpdateOrInsertMany applies UpdateOrInsert to each entry in order, as
// upstream's update_or_insert_many does, storing each previous value in dst.
// The batch is not a transaction.
func (m *mapCore[K, V, O]) UpdateOrInsertMany(entries []Pair[K, V], merge func(key K, old, value V) V, dst []Result[V]) {
	if len(dst) < len(entries) {
		panic("gobbtree: UpdateOrInsertMany destination too short")
	}
	for i, e := range entries {
		dst[i].Value, dst[i].OK = m.UpdateOrInsert(e.Key, e.Value, merge)
	}
}

// LoadOrStoreFunc returns key's value if present, and otherwise stores and
// returns make(). make may run although another writer stores key first;
// its value is then discarded and the stored one returned.
func (m *mapCore[K, V, O]) LoadOrStoreFunc(key K, make func() V) (V, bool) {
	if v, ok := m.Get(key); ok {
		return v, true
	}
	return m.LoadOrStore(key, make())
}

// UpdateRange replaces each value with keys in b by change(key, value), in
// key order. Each replacement is atomic, as Update is; the walk is not a
// snapshot, like Scan.
func (m *mapCore[K, V, O]) UpdateRange(b Bounds[K], change func(K, V) V) {
	for k := range m.Scan(b) {
		m.Update(k, change)
	}
}

// UpdateAll replaces every value by change(key, value), as UpdateRange does.
func (m *mapCore[K, V, O]) UpdateAll(change func(K, V) V) {
	for k := range m.All() {
		m.Update(k, change)
	}
}

// InsertAll inserts every entry of seq, as upstream's Extend does.
func (m *mapCore[K, V, O]) InsertAll(seq iter.Seq2[K, V]) {
	for k, v := range seq {
		m.Insert(k, v)
	}
}
