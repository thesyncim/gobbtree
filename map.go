package gobbtree

import (
	"cmp"
	"iter"
	"sync/atomic"
)

// Map is a concurrent ordered map. Its zero value is ready to use.
//
// With word-sized keys and values it shares one tree as upstream's
// BBTreeMap does: lookups descend optimistically, checking each node's
// version, writers latch only the leaf they change, and splits and merges
// take only the nodes they change. Other keys lock single leaves under a
// structural lock that splits and merges take exclusively. Scans copy a leaf
// at a time and visit the copy with nothing held.
//
// Do not copy a Map after first use. Read, Update, UpdateOrInsert and DeleteIf
// callbacks run under the entry's latch and must not reenter the same Map.
// Clear is the owner's: no other goroutine may use the Map until it returns.
type Map[K cmp.Ordered, V any] struct{ mapCore[K, V, natural[K]] }

// mapCore implements Map and MapOf for one key ordering.
type mapCore[K any, V any, O keyOrder[K]] struct {
	version      atomic.Uint64
	root         atomic.Pointer[searchImage[K, V, O]]
	routes       atomic.Pointer[publishedRoutes[K, V, O]]
	words        atomic.Pointer[wordMap]
	wordDisabled atomic.Bool
	// Keep writer bookkeeping off the cache line loaded by every lookup.
	_         [28]byte
	structure structuralLock
	tree      treeCore[K, V, O]
	count     counter
}

// New creates an empty concurrent ordered map.
func New[K cmp.Ordered, V any]() *Map[K, V] { return new(Map[K, V]) }

// IsEmpty reports whether the map held no entry at some moment during the
// call, as upstream's is_empty.
func (m *mapCore[K, V, O]) IsEmpty() bool {
	if words := m.words.Load(); words != nil {
		return words.isEmpty()
	}
	return m.count.Load() == 0
}

// Clear removes every entry. As upstream's clear, it is the owner's: no other
// goroutine may use the map until it returns.
func (m *mapCore[K, V, O]) Clear() {
	if words := m.words.Load(); words != nil {
		words.clear()
		return
	}
	if m.structure.state.Load() == nil {
		return
	}
	m.structure.Lock()
	defer m.structure.Unlock()
	m.version.Add(1)
	m.tree.Clear()
	m.root.Store(nil)
	m.routes.Store(nil)
	m.count.Store(0)
	m.version.Add(1)
}
func (m *mapCore[K, V, O]) Get(key K) (V, bool) {
	words := m.words.Load()
	if words == nil && !m.wordDisabled.Load() {
		if m.count.Load() == 0 {
			var zero V
			return zero, false
		}
		words = m.initNumericMap()
	}
	if words != nil {
		value, found := words.get(numericKey[O](key))
		if checkedMapWordValues {
			if !found {
				value = 0
			}
			if decoded, ok := any(value).(V); ok {
				return decoded, found
			}
		} else if words.valueKind == 1 {
			if !found {
				value = 0
			}
			return any(value).(V), found
		}
		return mapWordValue[V](value, found, words.valueKind)
	}
	return m.optimisticGet(key)
}

func (m *mapCore[K, V, O]) Contains(key K) bool { _, ok := m.Get(key); return ok }
func (m *mapCore[K, V, O]) Insert(key K, value V) (V, bool) {
	words := m.words.Load()
	if words == nil && !m.wordDisabled.Load() {
		words = m.initNumericMap()
	}
	if words != nil {
		old, found := words.insert(numericKey[O](key), packWord(value))
		if checkedMapWordValues {
			if !found {
				old = 0
			}
			if decoded, ok := any(old).(V); ok {
				return decoded, found
			}
		} else if words.valueKind == 1 {
			if !found {
				old = 0
			}
			return any(old).(V), found
		}
		return mapWordValue[V](old, found, words.valueKind)
	}
	return m.insertGeneric(key, value)
}
func (m *mapCore[K, V, O]) insertGeneric(key K, value V) (V, bool) {
	lane := m.structure.lane(structuralLaneCode[O](key))
	lane.RLock()
	if m.tree.root != nil {
		n := m.tree.root.findLeaf(key)
		n.leaf.mu.Lock()
		i, equal := n.exact(key)
		if equal {
			old := n.leaf.values[i]
			n.setValue(i, value)
			n.leaf.mu.Unlock()
			lane.RUnlock()
			return old, true
		}
		if n.n < nodeSize {
			if i == n.n && n.n > 0 && compareKeys[O](key, n.keys[n.n-1]) < 0 {
				i = n.lower(key)
			}
			if i > 0 {
				n.advanceSequence()
				n.insertLeaf(i, m.tree.storedKey(key), value)
				m.count.Add(1)
				n.advanceSequence()
				n.leaf.mu.Unlock()
				lane.RUnlock()
				var v V
				return v, false
			}
		}
		n.leaf.mu.Unlock()
	}
	lane.RUnlock()
	m.structure.Lock()
	defer m.structure.Unlock()
	held := m.structuralStart(key)
	defer m.structuralEnd(held)
	m.tree.length = int(m.count.Load())
	old, replaced := m.tree.Insert(key, value)
	if !replaced {
		m.count.Add(1)
	}
	return old, replaced
}
func (m *mapCore[K, V, O]) Delete(key K) (V, bool) {
	words := m.words.Load()
	if words == nil && !m.wordDisabled.Load() {
		if m.count.Load() == 0 {
			var zero V
			return zero, false
		}
		words = m.initNumericMap()
	}
	if words != nil {
		old, found := words.delete(numericKey[O](key))
		if checkedMapWordValues {
			if !found {
				old = 0
			}
			if decoded, ok := any(old).(V); ok {
				return decoded, found
			}
		} else if words.valueKind == 1 {
			if !found {
				old = 0
			}
			return any(old).(V), found
		}
		return mapWordValue[V](old, found, words.valueKind)
	}
	return m.deleteGeneric(key)
}
func (m *mapCore[K, V, O]) deleteGeneric(key K) (V, bool) {
	lane := m.structure.lane(structuralLaneCode[O](key))
	lane.RLock()
	if m.tree.root == nil {
		lane.RUnlock()
		var v V
		return v, false
	}
	n := m.tree.root.findLeaf(key)
	n.leaf.mu.Lock()
	i, equal := n.exact(key)
	if !equal {
		n.leaf.mu.Unlock()
		lane.RUnlock()
		var v V
		return v, false
	}
	if n.n > minNodeSize && i > 0 {
		n.advanceSequence()
		old := n.deleteLeaf(i)
		m.count.Add(-1)
		n.advanceSequence()
		n.leaf.mu.Unlock()
		lane.RUnlock()
		return old, true
	}
	n.leaf.mu.Unlock()
	lane.RUnlock()
	m.structure.Lock()
	defer m.structure.Unlock()
	held := m.structuralStart(key)
	defer m.structuralEnd(held)
	m.tree.length = int(m.count.Load())
	old, ok := m.tree.Delete(key)
	if ok {
		m.count.Add(-1)
	}
	return old, ok
}

// Read calls visit under a leaf latch if key exists. Values are copies.
// A panic releases both latches before propagating to the caller.
func (m *mapCore[K, V, O]) Read(key K, visit func(K, V)) bool {
	if words := m.numericMap(); words != nil {
		return words.read(numericKey[O](key), func(k, v uint64) { visit(decodeWordKey[K](k, words.keyKind), unpackWord[V](v, words.valueKind)) })
	}
	lane := m.structure.lane(structuralLaneCode[O](key))
	lane.RLock()
	defer lane.RUnlock()
	if m.tree.root == nil {
		return false
	}
	n := m.tree.root.findLeaf(key)
	n.leaf.mu.RLock()
	defer n.leaf.mu.RUnlock()
	i, equal := n.exact(key)
	if !equal {
		return false
	}
	visit(n.keys[i], n.leaf.values[i])
	return true
}

// Update atomically replaces key's value with change(stored key, value), as
// upstream's update does, and returns the new value. A panic in change leaves
// the stored value unchanged.
func (m *mapCore[K, V, O]) Update(key K, change func(K, V) V) (V, bool) {
	if words := m.numericMap(); words != nil {
		value, found := words.update(numericKey[O](key), func(k, v uint64) uint64 {
			return packWord(change(decodeWordKey[K](k, words.keyKind), unpackWord[V](v, words.valueKind)))
		})
		return mapWordValue[V](value, found, words.valueKind)
	}
	lane := m.structure.lane(structuralLaneCode[O](key))
	lane.RLock()
	defer lane.RUnlock()
	if m.tree.root != nil {
		n := m.tree.root.findLeaf(key)
		n.leaf.mu.Lock()
		defer n.leaf.mu.Unlock()
		i, equal := n.exact(key)
		if equal {
			value := change(n.keys[i], n.leaf.values[i])
			n.setValue(i, value)
			return value, true
		}
	}
	var v V
	return v, false
}

// UpdateOrInsert inserts value if key is absent, and otherwise stores
// merge(stored key, old, value), as upstream's update_or_insert does. It
// returns the old value and whether key was present. A panic in merge leaves
// the stored value unchanged.
func (m *mapCore[K, V, O]) UpdateOrInsert(key K, value V, merge func(key K, old, value V) V) (V, bool) {
	if words := m.numericMap(); words != nil {
		old, found := words.updateOrInsert(numericKey[O](key), packWord(value), func(k, a, b uint64) uint64 {
			return packWord(merge(decodeWordKey[K](k, words.keyKind), unpackWord[V](a, words.valueKind), unpackWord[V](b, words.valueKind)))
		})
		return mapWordValue[V](old, found, words.valueKind)
	}
	if old, found, done := m.upsertLeaf(key, value, merge); done {
		return old, found
	}
	m.structure.Lock()
	defer m.structure.Unlock()
	stored, old, ok := m.tree.GetKeyValue(key)
	if ok {
		value = merge(stored, old, value)
	}
	held := m.structuralStart(key)
	defer m.structuralEnd(held)
	m.tree.length = int(m.count.Load())
	m.tree.Insert(key, value)
	if !ok {
		m.count.Add(1)
	}
	return old, ok
}

// LoadOrStore returns key's value if present, and otherwise stores and
// returns value, as upstream's get_or_insert does. loaded reports that key
// was present.
func (m *mapCore[K, V, O]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	if words := m.numericMap(); words != nil {
		actual, loaded := words.loadOrStore(numericKey[O](key), packWord(value))
		return unpackWord[V](actual, words.valueKind), loaded
	}
	if old, found, done := m.upsertLeaf(key, value, nil); done {
		if found {
			return old, true
		}
		return value, false
	}
	m.structure.Lock()
	defer m.structure.Unlock()
	if old, ok := m.tree.Get(key); ok {
		return old, true
	}
	held := m.structuralStart(key)
	defer m.structuralEnd(held)
	m.tree.length = int(m.count.Load())
	m.tree.Insert(key, value)
	m.count.Add(1)
	return value, false
}

// upsertLeaf finishes an UpdateOrInsert, or with a nil merge a LoadOrStore,
// under key's leaf latch when that needs no structural change. done reports
// that it did; otherwise nothing changed.
func (m *mapCore[K, V, O]) upsertLeaf(key K, value V, merge func(K, V, V) V) (old V, found, done bool) {
	lane := m.structure.lane(structuralLaneCode[O](key))
	lane.RLock()
	defer lane.RUnlock()
	if m.tree.root == nil {
		return old, false, false
	}
	n := m.tree.root.findLeaf(key)
	n.leaf.mu.Lock()
	defer n.leaf.mu.Unlock()
	i, equal := n.exact(key)
	if equal {
		old = n.leaf.values[i]
		if merge != nil {
			n.setValue(i, merge(n.keys[i], old, value))
		}
		return old, true, true
	}
	if n.n == nodeSize {
		return old, false, false
	}
	if i == n.n && n.n > 0 && compareKeys[O](key, n.keys[n.n-1]) < 0 {
		i = n.lower(key)
	}
	// A new first key would change the separators above the leaf.
	if i == 0 {
		return old, false, false
	}
	n.advanceSequence()
	n.insertLeaf(i, m.tree.storedKey(key), value)
	m.count.Add(1)
	n.advanceSequence()
	return old, false, true
}

// DeleteIf removes key if condition accepts its stored key and value, as
// upstream's remove_if does, and returns them. condition may run more than
// once.
func (m *mapCore[K, V, O]) DeleteIf(key K, condition func(K, V) bool) (K, V, bool) {
	if words := m.numericMap(); words != nil {
		old, found := words.deleteIf(numericKey[O](key), func(k, v uint64) bool {
			return condition(decodeWordKey[K](k, words.keyKind), unpackWord[V](v, words.valueKind))
		})
		return mapWordEntry[K, V](numericKey[O](key), old, found, words)
	}
	if k, v, removed, done := m.deleteLeafIf(key, condition); done {
		return k, v, removed
	}
	m.structure.Lock()
	defer m.structure.Unlock()
	if k, v, ok := m.tree.GetKeyValue(key); ok && condition(k, v) {
		held := m.structuralStart(key)
		defer m.structuralEnd(held)
		m.tree.length = int(m.count.Load())
		m.tree.Delete(key)
		m.count.Add(-1)
		return k, v, true
	}
	var k K
	var v V
	return k, v, false
}

// deleteLeafIf finishes a DeleteIf under key's leaf latch when that needs no
// structural change. done reports that it did; otherwise nothing changed.
func (m *mapCore[K, V, O]) deleteLeafIf(key K, condition func(K, V) bool) (k K, v V, removed, done bool) {
	lane := m.structure.lane(structuralLaneCode[O](key))
	lane.RLock()
	defer lane.RUnlock()
	if m.tree.root == nil {
		return k, v, false, true
	}
	n := m.tree.root.findLeaf(key)
	n.leaf.mu.Lock()
	defer n.leaf.mu.Unlock()
	i, equal := n.exact(key)
	if !equal || !condition(n.keys[i], n.leaf.values[i]) {
		return k, v, false, true
	}
	if n.n <= minNodeSize || i == 0 {
		return k, v, false, false
	}
	k = n.keys[i]
	n.advanceSequence()
	v = n.deleteLeaf(i)
	m.count.Add(-1)
	n.advanceSequence()
	return k, v, true, true
}

// Take removes key, returning the stored key and its value, as upstream's
// remove_entry does.
func (m *mapCore[K, V, O]) Take(key K) (K, V, bool) {
	if words := m.numericMap(); words != nil {
		old, found := words.delete(numericKey[O](key))
		return mapWordEntry[K, V](numericKey[O](key), old, found, words)
	}
	m.structure.Lock()
	defer m.structure.Unlock()
	stored, v, ok := m.tree.GetKeyValue(key)
	if ok {
		held := m.structuralStart(key)
		defer m.structuralEnd(held)
		m.tree.length = int(m.count.Load())
		m.tree.Delete(key)
		m.count.Add(-1)
	}
	return stored, v, ok
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
	words := m.words.Load()
	if words == nil && !m.wordDisabled.Load() {
		if m.count.Load() == 0 {
			clear(dst[:len(keys)])
			return
		}
		words = m.initNumericMap()
	}
	if words != nil {
		// Whole groups of eight go through the batched lookup.
		var codes [batchLanes]uint64
		var values [batchLanes]Result[uint64]
		for start := 0; start+batchLanes <= len(keys); start += batchLanes {
			for j := range codes {
				codes[j] = numericKey[O](keys[start+j])
			}
			words.getMany8(&codes, &values)
			for j, r := range values {
				dst[start+j].Value, dst[start+j].OK = mapWordValue[V](r.Value, r.OK, words.valueKind)
			}
		}
		for i := len(keys) &^ (batchLanes - 1); i < len(keys); i++ {
			key := keys[i]
			v, found := words.get(numericKey[O](key))
			if checkedMapWordValues {
				value := v
				if !found {
					value = 0
				}
				if decoded, ok := any(value).(V); ok {
					dst[i].Value, dst[i].OK = decoded, found
				} else {
					dst[i].Value, dst[i].OK = mapWordValue[V](v, found, words.valueKind)
				}
			} else if words.valueKind == 1 {
				value := v
				if !found {
					value = 0
				}
				dst[i].Value, dst[i].OK = any(value).(V), found
			} else {
				dst[i].Value, dst[i].OK = mapWordValue[V](v, found, words.valueKind)
			}
		}
		return
	}
	for start := 0; start < len(keys); start += batchLanes {
		count := min(batchLanes, len(keys)-start)
		batch := keys[start : start+count]
		version := m.version.Load()
		var images [batchLanes]*searchImage[K, V, O]
		var codes [batchLanes]uint64
		valid := version&1 == 0 && descendPublished(m.root.Load(), batch, &images, &codes)
		for j, key := range batch {
			var result Result[V]
			current := valid
			if current && images[j] != nil {
				result.Value, result.OK, current = images[j].readValue(key, codes[j], &m.version, version)
			}
			if !current || m.version.Load() != version {
				result.Value, result.OK = m.Get(key)
			}
			dst[start+j] = result
		}
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
	if words := m.numericMap(); words != nil {
		low, high, open, empty := wordBounds[K, O](b)
		if empty {
			return
		}
		// An unbounded walk starts at low ascending and at the last key
		// descending; a bounded one checks both ends.
		words.walk(low, high, reverse, !open, func(k, v uint64) bool {
			if open && reverse && k < low {
				return false
			}
			return visit(decodeWordKey[K](k, words.keyKind), unpackWord[V](v, words.valueKind))
		})
		return
	}
	m.walk(b, reverse, visit)
}

// scanLeaves bounds the leaves a scan copies per latch acquisition, and so
// how long it can hold back a structural writer.
const scanLeaves = 4

// walk visits leaf copies of the keys in b. It resumes past the last key it
// visited.
func (m *mapCore[K, V, O]) walk(b *Bounds[K], reverse bool, visit func(K, V) bool) {
	var keys [scanLeaves * nodeSize]K
	var values [scanLeaves * nodeSize]V
	var last K
	resumed := false
	// next is the following leaf, valid while the structural epoch stays epoch.
	var next *node[K, V, O]
	var epoch uint64
	for {
		count, done := m.copyLeaf(b, &last, resumed, reverse, &keys, &values, &next, &epoch)
		for i := 0; i < count; i++ {
			if !visit(keys[i], values[i]) {
				return
			}
		}
		if done || count == 0 {
			return
		}
		last, resumed = keys[count-1], true
	}
}

// copyLeaf copies, in visiting order, the entries of up to scanLeaves leaves
// from the first that holds any in range and past last when resumed. done reports that no later
// leaf can hold more.
func (m *mapCore[K, V, O]) copyLeaf(b *Bounds[K], last *K, resumed, reverse bool, keys *[scanLeaves * nodeSize]K, values *[scanLeaves * nodeSize]V, chain **node[K, V, O], epoch *uint64) (count int, done bool) {
	// A scan excludes structural changes only, without enabling lanes.
	guard := m.structure.acquire(0)
	defer guard.unlock()
	if m.tree.root == nil {
		return 0, true
	}
	version := m.version.Load()
	var n *node[K, V, O]
	switch {
	case resumed && *chain != nil && *epoch == version:
		// No split or merge since the last leaf: continue along the chain.
		n = *chain
	case resumed:
		n = m.tree.root.findLeaf(*last)
	case reverse && b.upperKind != unbounded:
		n = m.tree.root.findLeaf(b.upper)
	case reverse:
		n = m.tree.lastLeaf()
	case b.lowerKind != unbounded:
		n = m.tree.root.findLeaf(b.lower)
	default:
		n = m.tree.firstLeaf()
	}
	for n != nil {
		n.leaf.mu.RLock()
		if reverse {
			end := n.n
			if resumed {
				end = n.lower(*last)
			} else if b.upperKind != unbounded {
				end = n.lower(b.upper)
				if b.upperKind == included && end < n.n && compareKeys[O](n.keys[end], b.upper) == 0 {
					end++
				}
			}
			for i := end - 1; i >= 0; i-- {
				if !aboveLower[K, O](b, n.keys[i]) {
					done = true
					break
				}
				keys[count], values[count] = n.keys[i], n.leaf.values[i]
				count++
			}
		} else {
			start := 0
			if resumed {
				start, _ = n.lowerExact(*last)
				if start < n.n && compareKeys[O](n.keys[start], *last) == 0 {
					start++
				}
			} else if b.lowerKind != unbounded {
				start = n.lower(b.lower)
				if b.lowerKind == excluded && start < n.n && compareKeys[O](n.keys[start], b.lower) == 0 {
					start++
				}
			}
			for i := start; i < n.n; i++ {
				if !belowUpper[K, O](b, n.keys[i]) {
					done = true
					break
				}
				keys[count], values[count] = n.keys[i], n.leaf.values[i]
				count++
			}
		}
		next := n.leaf.next
		if reverse {
			next = n.leaf.prev
		}
		n.leaf.mu.RUnlock()
		if done || next == nil || count > len(keys)-nodeSize {
			*chain, *epoch = next, version
			return count, done || next == nil
		}
		n = next
	}
	return 0, true
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
	if words := m.numericMap(); words != nil {
		words.walk(0, 0, true, false, func(k, v uint64) bool {
			key, value, ok = decodeWordKey[K](k, words.keyKind), unpackWord[V](v, words.valueKind), true
			return false
		})
		return key, value, ok
	}
	m.walk(&Bounds[K]{}, true, func(k K, v V) bool {
		key, value, ok = k, v, true
		return false
	})
	return key, value, ok
}
func (m *mapCore[K, V, O]) PopFirst() (K, V, bool) {
	if words := m.numericMap(); words != nil {
		k, v, found := words.pop(false)
		return mapWordEntry[K, V](k, v, found, words)
	}
	m.structure.Lock()
	defer m.structure.Unlock()
	key, _, present := m.tree.First()
	if !present {
		var v V
		return key, v, false
	}
	held := m.structuralStart(key)
	defer m.structuralEnd(held)
	m.tree.length = int(m.count.Load())
	k, v, ok := m.tree.PopFirst()
	if ok {
		m.count.Add(-1)
	}
	return k, v, ok
}
func (m *mapCore[K, V, O]) PopLast() (K, V, bool) {
	if words := m.numericMap(); words != nil {
		k, v, found := words.pop(true)
		return mapWordEntry[K, V](k, v, found, words)
	}
	m.structure.Lock()
	defer m.structure.Unlock()
	key, _, present := m.tree.Last()
	if !present {
		var v V
		return key, v, false
	}
	held := m.structuralStart(key)
	defer m.structuralEnd(held)
	m.tree.length = int(m.count.Load())
	k, v, ok := m.tree.PopLast()
	if ok {
		m.count.Add(-1)
	}
	return k, v, ok
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
