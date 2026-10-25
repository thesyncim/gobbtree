package gobbtree

import (
	"cmp"
	"iter"
	"sync"
	"sync/atomic"
)

// Map is a concurrent ordered map. Its zero value is ready to use.
// Get uses immutable search images and version validation. Built-in word
// values use atomic loads; other values use a shared leaf latch. Writers
// latch leaves independently; splits, merges and new minima take a structural
// latch exclusively. Scans and callbacks also hold the structural read latch.
// Do not copy a Map after first use. Callbacks must not reenter the same Map.
type Map[K cmp.Ordered, V any] struct {
	version atomic.Uint64
	root    atomic.Pointer[searchImage[K, V]]
	// Keep writer bookkeeping off the cache line loaded by every lookup.
	_         [48]byte
	structure sync.RWMutex
	tree      Tree[K, V]
	count     atomic.Int64
}

// New creates an empty concurrent ordered map.
func New[K cmp.Ordered, V any]() *Map[K, V] { return new(Map[K, V]) }

// Len returns the number of entries at an instant during the call.
func (m *Map[K, V]) Len() int      { return int(m.count.Load()) }
func (m *Map[K, V]) IsEmpty() bool { return m.count.Load() == 0 }
func (m *Map[K, V]) Clear() {
	m.structure.Lock()
	defer m.structure.Unlock()
	m.version.Add(1)
	m.tree.Clear()
	m.root.Store(nil)
	m.count.Store(0)
	m.version.Add(1)
}
func (m *Map[K, V]) Get(key K) (V, bool) { return m.optimisticGet(key) }

func (m *Map[K, V]) Contains(key K) bool { _, ok := m.Get(key); return ok }
func (m *Map[K, V]) Insert(key K, value V) (V, bool) {
	m.structure.RLock()
	if m.tree.root != nil {
		n := m.tree.root.findLeaf(key)
		n.leaf.mu.Lock()
		i := n.lower(key)
		if i < n.n && cmp.Compare(n.keys[i], key) == 0 {
			old := n.leaf.values[i]
			n.setValue(i, value)
			n.leaf.mu.Unlock()
			m.structure.RUnlock()
			return old, true
		}
		if n.n < nodeSize && i > 0 {
			n.leaf.advanceSequence()
			n.insertLeaf(i, key, value)
			m.count.Add(1)
			n.leaf.advanceSequence()
			n.leaf.mu.Unlock()
			m.structure.RUnlock()
			var v V
			return v, false
		}
		n.leaf.mu.Unlock()
	}
	m.structure.RUnlock()
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
func (m *Map[K, V]) Delete(key K) (V, bool) {
	m.structure.RLock()
	if m.tree.root == nil {
		m.structure.RUnlock()
		var v V
		return v, false
	}
	n := m.tree.root.findLeaf(key)
	n.leaf.mu.Lock()
	i := n.lower(key)
	if i == n.n || cmp.Compare(n.keys[i], key) != 0 {
		n.leaf.mu.Unlock()
		m.structure.RUnlock()
		var v V
		return v, false
	}
	if n.n > minNodeSize && i > 0 {
		n.leaf.advanceSequence()
		old := n.deleteLeaf(i)
		m.count.Add(-1)
		n.leaf.advanceSequence()
		n.leaf.mu.Unlock()
		m.structure.RUnlock()
		return old, true
	}
	n.leaf.mu.Unlock()
	m.structure.RUnlock()
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

// Read calls visit under a shared leaf latch if key exists. Values are copies.
// A panic releases both latches before propagating to the caller.
func (m *Map[K, V]) Read(key K, visit func(K, V)) bool {
	m.structure.RLock()
	defer m.structure.RUnlock()
	if m.tree.root == nil {
		return false
	}
	n := m.tree.root.findLeaf(key)
	n.leaf.mu.RLock()
	defer n.leaf.mu.RUnlock()
	i := n.lower(key)
	if i == n.n || cmp.Compare(n.keys[i], key) != 0 {
		return false
	}
	visit(n.keys[i], n.leaf.values[i])
	return true
}

// Update atomically replaces an existing value with change(old), returning
// the new value. A panic in change leaves the stored value unchanged.
func (m *Map[K, V]) Update(key K, change func(V) V) (V, bool) {
	m.structure.RLock()
	defer m.structure.RUnlock()
	if m.tree.root != nil {
		n := m.tree.root.findLeaf(key)
		n.leaf.mu.Lock()
		defer n.leaf.mu.Unlock()
		i := n.lower(key)
		if i < n.n && cmp.Compare(n.keys[i], key) == 0 {
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
func (m *Map[K, V]) UpdateOrInsert(key K, value V, merge func(V, V) V) (V, bool) {
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
func (m *Map[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
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
func (m *Map[K, V]) DeleteIf(key K, predicate func(V) bool) (V, bool) {
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
func (m *Map[K, V]) GetMany(keys []K, dst []Result[V]) {
	if len(dst) < len(keys) {
		panic("gobbtree: GetMany destination too short")
	}
	for start := 0; start < len(keys); start += batchLanes {
		count := min(batchLanes, len(keys)-start)
		batch := keys[start : start+count]
		version := m.version.Load()
		var images [batchLanes]*searchImage[K, V]
		var codes [batchLanes]uint64
		valid := version&1 == 0 && descendPublished(m.root.Load(), batch, &images, &codes)
		for j, key := range batch {
			var result Result[V]
			current := valid
			if current && images[j] != nil {
				result.Value, result.OK, current = images[j].readValue(key, codes[j])
			}
			if !current || m.version.Load() != version {
				result.Value, result.OK = m.Get(key)
			}
			dst[start+j] = result
		}
	}
}

// InsertMany applies entries in order. The batch is not a transaction.
func (m *Map[K, V]) InsertMany(entries []Entry[K, V], dst []Result[V]) {
	if len(dst) < len(entries) {
		panic("gobbtree: InsertMany destination too short")
	}
	for i, e := range entries {
		dst[i].Value, dst[i].OK = m.Insert(e.Key, e.Value)
	}
}

// Scan visits [lower,upper) in ascending order, one leaf at a time. It is
// weakly consistent under concurrent writes, not a snapshot. Callbacks run
// under latches and must not reenter this map. Returning false stops early.
func (m *Map[K, V]) Scan(lower, upper K, visit func(K, V) bool) {
	m.structure.RLock()
	defer m.structure.RUnlock()
	if m.tree.root == nil || cmp.Compare(lower, upper) >= 0 {
		return
	}
	for n := m.tree.root.findLeaf(lower); n != nil; n = n.leaf.next {
		if !n.scanLeaf(lower, upper, false, visit) {
			return
		}
	}
}
func (m *Map[K, V]) ScanReverse(lower, upper K, visit func(K, V) bool) {
	m.structure.RLock()
	defer m.structure.RUnlock()
	if m.tree.root == nil || cmp.Compare(lower, upper) >= 0 {
		return
	}
	for n := m.tree.root.findLeaf(upper); n != nil; n = n.leaf.prev {
		if !n.scanLeaf(lower, upper, true, visit) {
			return
		}
	}
}
func (n *node[K, V]) scanLeaf(lower, upper K, reverse bool, visit func(K, V) bool) bool {
	n.leaf.mu.RLock()
	defer n.leaf.mu.RUnlock()
	if reverse {
		for i := n.lower(upper) - 1; i >= 0; i-- {
			if cmp.Compare(n.keys[i], lower) < 0 || !visit(n.keys[i], n.leaf.values[i]) {
				return false
			}
		}
	} else {
		for i := n.lower(lower); i < n.n; i++ {
			if cmp.Compare(n.keys[i], upper) >= 0 || !visit(n.keys[i], n.leaf.values[i]) {
				return false
			}
		}
	}
	return true
}

// All visits every entry in key order, with the same callback and consistency
// rules as Scan. Use Snapshot to iterate without holding map latches.
func (m *Map[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		m.structure.RLock()
		defer m.structure.RUnlock()
		for n := m.tree.firstLeaf(); n != nil; n = n.leaf.next {
			if !n.visitLeaf(yield) {
				return
			}
		}
	}
}
func (n *node[K, V]) visitLeaf(yield func(K, V) bool) bool {
	n.leaf.mu.RLock()
	defer n.leaf.mu.RUnlock()
	for i := 0; i < n.n; i++ {
		if !yield(n.keys[i], n.leaf.values[i]) {
			return false
		}
	}
	return true
}

// Snapshot returns a consistent ascending copy of the entries, allocating
// one slice. Writers are excluded while the slice is copied.
func (m *Map[K, V]) Snapshot() []Entry[K, V] {
	m.structure.Lock()
	defer m.structure.Unlock()
	out := make([]Entry[K, V], 0, m.Len())
	for k, v := range m.tree.All() {
		out = append(out, Entry[K, V]{k, v})
	}
	return out
}
func (m *Map[K, V]) First() (K, V, bool) {
	m.structure.Lock()
	defer m.structure.Unlock()
	return m.tree.First()
}
func (m *Map[K, V]) Last() (K, V, bool) {
	m.structure.Lock()
	defer m.structure.Unlock()
	return m.tree.Last()
}
func (m *Map[K, V]) PopFirst() (K, V, bool) {
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
func (m *Map[K, V]) PopLast() (K, V, bool) {
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
