package gobbtree

import (
	"cmp"
	"iter"
	"sync/atomic"
)

// Map is a concurrent ordered map. Its zero value is ready to use.
// Get validates optimistic reads of numeric arenas or immutable search images;
// textual leaves with other values are read under their latch.
// Built-in word values use atomic loads; other values use a leaf latch. Writers
// latch leaves independently; splits, merges and routing changes take a structural
// latch exclusively. Scans copy a leaf at a time and visit without latches.
// Do not copy a Map after first use. Read, Update, UpdateOrInsert and DeleteIf
// callbacks run under the entry's latch and must not reenter the same Map.
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
	count     atomic.Int64
}

// New creates an empty concurrent ordered map.
func New[K cmp.Ordered, V any]() *Map[K, V] { return new(Map[K, V]) }

// Len returns the number of entries at an instant during the call.
func (m *mapCore[K, V, O]) Len() int      { return int(m.count.Load()) }
func (m *mapCore[K, V, O]) IsEmpty() bool { return m.count.Load() == 0 }
func (m *mapCore[K, V, O]) Clear() {
	if m.structure.state.Load() == nil {
		return
	}
	if words := m.numericMap(); words != nil {
		words.clear()
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

// Update atomically replaces an existing value with change(old), returning
// the new value. A panic in change leaves the stored value unchanged.
func (m *mapCore[K, V, O]) Update(key K, change func(V) V) (V, bool) {
	if words := m.numericMap(); words != nil {
		value, found := words.update(numericKey[O](key), func(v uint64) uint64 { return packWord(change(unpackWord[V](v, words.valueKind))) })
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
			value := change(n.leaf.values[i])
			n.setValue(i, value)
			return value, true
		}
	}
	var v V
	return v, false
}

// UpdateOrInsert inserts value if absent, otherwise stores merge(old,value).
// It returns the old value and whether an existing entry was updated.
func (m *mapCore[K, V, O]) UpdateOrInsert(key K, value V, merge func(V, V) V) (V, bool) {
	if words := m.numericMap(); words != nil {
		old, found := words.updateOrInsert(numericKey[O](key), packWord(value), func(a, b uint64) uint64 {
			return packWord(merge(unpackWord[V](a, words.valueKind), unpackWord[V](b, words.valueKind)))
		})
		return mapWordValue[V](old, found, words.valueKind)
	}
	m.structure.Lock()
	defer m.structure.Unlock()
	old, ok := m.tree.Get(key)
	if ok {
		value = merge(old, value)
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

// LoadOrStore returns the existing value if present, or stores and returns value.
// loaded is true exactly when the value was already present.
func (m *mapCore[K, V, O]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	if words := m.numericMap(); words != nil {
		actual, loaded := words.loadOrStore(numericKey[O](key), packWord(value))
		return unpackWord[V](actual, words.valueKind), loaded
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

// DeleteIf removes key only if predicate accepts its current value.
func (m *mapCore[K, V, O]) DeleteIf(key K, predicate func(V) bool) (V, bool) {
	if words := m.numericMap(); words != nil {
		old, found := words.deleteIf(numericKey[O](key), func(v uint64) bool { return predicate(unpackWord[V](v, words.valueKind)) })
		return mapWordValue[V](old, found, words.valueKind)
	}
	m.structure.Lock()
	defer m.structure.Unlock()
	if v, ok := m.tree.Get(key); ok && predicate(v) {
		held := m.structuralStart(key)
		defer m.structuralEnd(held)
		m.tree.length = int(m.count.Load())
		m.tree.Delete(key)
		m.count.Add(-1)
		return v, true
	}
	var v V
	return v, false
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
		if view := words.view.Load(); len(keys) > 1 && view != nil && view.leaves != nil {
			for start := 0; start < len(keys); start += batchLanes {
				count := min(batchLanes, len(keys)-start)
				if count < batchLanes {
					for j := 0; j < count; j++ {
						v, found := words.get(numericKey[O](keys[start+j]))
						if checkedMapWordValues {
							value := v
							if !found {
								value = 0
							}
							if decoded, ok := any(value).(V); ok {
								dst[start+j].Value, dst[start+j].OK = decoded, found
							} else {
								dst[start+j].Value, dst[start+j].OK = mapWordValue[V](v, found, words.valueKind)
							}
						} else if words.valueKind == 1 {
							value := v
							if !found {
								value = 0
							}
							dst[start+j].Value, dst[start+j].OK = any(value).(V), found
						} else {
							dst[start+j].Value, dst[start+j].OK = mapWordValue[V](v, found, words.valueKind)
						}
					}
					continue
				}
				var codes [batchLanes]uint64
				var values [batchLanes]Result[uint64]
				for j := 0; j < count; j++ {
					codes[j] = numericKey[O](keys[start+j])
				}
				words.getMany8(&codes, &values)
				for j := 0; j < count; j++ {
					if checkedMapWordValues {
						value := values[j].Value
						if !values[j].OK {
							value = 0
						}
						if decoded, ok := any(value).(V); ok {
							dst[start+j].Value, dst[start+j].OK = decoded, values[j].OK
						} else {
							dst[start+j].Value, dst[start+j].OK = mapWordValue[V](values[j].Value, values[j].OK, words.valueKind)
						}
					} else if words.valueKind == 1 {
						value := values[j].Value
						if !values[j].OK {
							value = 0
						}
						dst[start+j].Value, dst[start+j].OK = any(value).(V), values[j].OK
					} else {
						dst[start+j].Value, dst[start+j].OK = mapWordValue[V](values[j].Value, values[j].OK, words.valueKind)
					}
				}
			}
			return
		}
		for i, key := range keys {
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
func (m *mapCore[K, V, O]) InsertMany(entries []Entry[K, V], dst []Result[V]) {
	if len(dst) < len(entries) {
		panic("gobbtree: InsertMany destination too short")
	}
	for i, e := range entries {
		dst[i].Value, dst[i].OK = m.Insert(e.Key, e.Value)
	}
}

// Scan visits [lower,upper) in ascending order, a leaf at a time, as
// upstream's scan does. Each leaf is copied under its latches, released before
// visit runs, so visit holds no latch and may use the map. It is not a
// snapshot: an entry present throughout is visited exactly once; entries
// inserted or removed meanwhile may or may not be. Returning false stops.
func (m *mapCore[K, V, O]) Scan(lower, upper K, visit func(K, V) bool) {
	if words := m.numericMap(); words != nil {
		words.walk(numericKey[O](lower), numericKey[O](upper), false, true, func(k, v uint64) bool {
			return visit(decodeWordKey[K](k, words.keyKind), unpackWord[V](v, words.valueKind))
		})
		return
	}
	if compareKeys[O](lower, upper) < 0 {
		m.walk(&lower, &upper, false, visit)
	}
}

// ScanReverse is Scan in descending order.
func (m *mapCore[K, V, O]) ScanReverse(lower, upper K, visit func(K, V) bool) {
	if words := m.numericMap(); words != nil {
		words.walk(numericKey[O](lower), numericKey[O](upper), true, true, func(k, v uint64) bool {
			return visit(decodeWordKey[K](k, words.keyKind), unpackWord[V](v, words.valueKind))
		})
		return
	}
	if compareKeys[O](lower, upper) < 0 {
		m.walk(&lower, &upper, true, visit)
	}
}

// All visits every entry in key order, as Scan does.
func (m *mapCore[K, V, O]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if words := m.numericMap(); words != nil {
			words.walk(0, 0, false, false, func(k, v uint64) bool {
				return yield(decodeWordKey[K](k, words.keyKind), unpackWord[V](v, words.valueKind))
			})
			return
		}
		m.walk(nil, nil, false, yield)
	}
}

// walk visits leaf copies from lower (inclusive) to upper (exclusive); a nil
// bound is open. It resumes past the last key it visited.
func (m *mapCore[K, V, O]) walk(lower, upper *K, reverse bool, visit func(K, V) bool) {
	var keys [scanLeaves * nodeSize]K
	var values [scanLeaves * nodeSize]V
	var last K
	resumed := false
	// next is the following leaf, valid while the structural epoch stays epoch.
	var next *node[K, V, O]
	var epoch uint64
	for {
		count, done := m.copyLeaf(lower, upper, &last, resumed, reverse, &keys, &values, &next, &epoch)
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
func (m *mapCore[K, V, O]) copyLeaf(lower, upper, last *K, resumed, reverse bool, keys *[scanLeaves * nodeSize]K, values *[scanLeaves * nodeSize]V, chain **node[K, V, O], epoch *uint64) (count int, done bool) {
	m.structure.RLock()
	defer m.structure.RUnlock()
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
	case reverse && upper != nil:
		n = m.tree.root.findLeaf(*upper)
	case reverse:
		n = m.tree.lastLeaf()
	case lower != nil:
		n = m.tree.root.findLeaf(*lower)
	default:
		n = m.tree.firstLeaf()
	}
	for n != nil {
		n.leaf.mu.RLock()
		if reverse {
			end := n.n
			if resumed {
				end = n.lower(*last)
			} else if upper != nil {
				end = n.lower(*upper)
			}
			for i := end - 1; i >= 0; i-- {
				if lower != nil && compareKeys[O](n.keys[i], *lower) < 0 {
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
			} else if lower != nil {
				start = n.lower(*lower)
			}
			for i := start; i < n.n; i++ {
				if upper != nil && compareKeys[O](n.keys[i], *upper) >= 0 {
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

// Last returns the largest entry, read like a scan.
func (m *mapCore[K, V, O]) Last() (key K, value V, ok bool) {
	if words := m.numericMap(); words != nil {
		words.walk(0, 0, true, false, func(k, v uint64) bool {
			key, value, ok = decodeWordKey[K](k, words.keyKind), unpackWord[V](v, words.valueKind), true
			return false
		})
		return key, value, ok
	}
	m.walk(nil, nil, true, func(k K, v V) bool {
		key, value, ok = k, v, true
		return false
	})
	return key, value, ok
}
func (m *mapCore[K, V, O]) PopFirst() (K, V, bool) {
	if words := m.numericMap(); words != nil {
		k, v, found := words.edge(false, true)
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
		k, v, found := words.edge(true, true)
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
