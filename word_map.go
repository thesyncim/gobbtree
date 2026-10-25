package gobbtree

import (
	"runtime"
	"sync/atomic"
)

type wordReadView struct {
	root   uint32
	level  uint8
	ready  bool
	blocks []atomic.Pointer[wordReadBlock]
	routes wordRoutes
}
type wordMap struct {
	version            atomic.Uint64
	view               atomic.Pointer[wordReadView]
	keyKind, valueKind uint8
	_                  [46]byte
	structure          *structuralLock
	count              *atomic.Int64
	tree               wordTree
}

// Only directory headers are published. Nodes contain one copy of their data;
// readers access shared numeric fields atomically and validate leaf/structure epochs.
func (m *wordMap) publish() {
	if m.tree.root == 0 {
		m.view.Store(nil)
		return
	}
	previous := m.view.Load()
	blocks := m.tree.readBlocks[:cap(m.tree.readBlocks)]
	routes := m.tree.routes
	sameRoutes := previous != nil && previous.routes.base == routes.base && previous.routes.shift == routes.shift && previous.routes.depth == routes.depth && len(previous.routes.ids) == len(routes.ids)
	if sameRoutes && len(routes.ids) != 0 {
		sameRoutes = &previous.routes.ids[0] == &routes.ids[0]
	}
	if previous != nil && previous.root == m.tree.root && previous.ready == m.tree.atomicWrites && sameRoutes && &previous.blocks[0] == &blocks[0] {
		return
	}
	view := &wordReadView{root: m.tree.root, level: m.tree.rootNode.level, ready: m.tree.atomicWrites, blocks: blocks, routes: routes}

	m.view.Store(view)
}
func (m *wordMap) finishStructure() { m.publish(); m.version.Add(1) }

// The first optimistic reader waits for all existing writers. Before that
// barrier, leaf locks suffice and writers can use ordinary bulk copies.
func (m *wordMap) enableReads() {
	m.structure.Lock()
	defer m.structure.Unlock()
	m.tree.atomicWrites = true
	m.publish()
}
func (m *wordMap) get(key uint64) (uint64, bool) {
retry:
	for {
		version := m.version.Load()
		if version&1 != 0 {
			runtime.Gosched()
			continue
		}
		view := m.view.Load()
		if view == nil {
			if m.version.Load() == version {
				return 0, false
			}
			continue
		}
		if !view.ready {
			m.enableReads()
			continue
		}
		id := view.routes.lookupAtomic(key)
		if id == 0 {
			id = view.root
		}
		for depth := 0; depth <= int(view.level); depth++ {
			if id == 0 {
				continue retry
			}
			index := (id &^ wordLeafID) - 1
			blockIndex := int(index >> 8)
			if blockIndex >= len(view.blocks) {
				continue retry
			}
			block := view.blocks[blockIndex].Load()
			if block == nil {
				continue retry
			}
			n := &block.nodes[index&255]
			sequence := n.sequence.Load()
			count := int(sequence >> 1 & 63)
			if id&wordLeafID != 0 {
				if sequence&1 != 0 {
					runtime.Gosched()
					continue retry
				}
				i := firstSharedTag(n.tagWords(), count, key)
				var value, candidate uint64
				if i < count {
					candidate = atomic.LoadUint64(&n.entries()[i].key)
					if candidate < key {
						lo, hi := 0, count
						for lo < hi {
							mid := int(uint(lo+hi) >> 1)
							if atomic.LoadUint64(&n.entries()[mid].key) < key {
								lo = mid + 1
							} else {
								hi = mid
							}
						}
						i = lo
						if i < count {
							candidate = atomic.LoadUint64(&n.entries()[i].key)
						}
					}
				}
				found := i < count && candidate == key
				if found {
					value = atomic.LoadUint64(&n.entries()[i].value)
				}
				if n.sequence.Load() != sequence || m.version.Load() != version {
					continue retry
				}
				return value, found
			}
			if count == 0 {
				continue retry
			}
			shift := uint(sequence >> 7 & 63)
			prefix := atomic.LoadUint64(n.prefixWord()) >> shift
			projection := key >> shift
			var i int
			if projection < prefix {
				i = 0
			} else if projection > prefix|255 {
				i = count - 1
			} else {
				upper, mayEqual := upperSharedTag(n.tagWords(), count, uint8(projection))
				i = max(0, upper-1)
				for mayEqual && i > 0 && atomic.LoadUint64(&n.keys[i]) > key {
					i--
				}
			}
			id = atomic.LoadUint32(&n.inner().children[i])
		}
	}
}
func (m *wordMap) leafFor(key uint64) uint32 {
	if m.tree.root == 0 {
		return 0
	}
	id := m.tree.routes.lookup(key)
	if id == 0 {
		id = m.tree.root
	}
	for {
		n := m.tree.node(id)
		if n.leaf {
			return id
		}
		id = n.inner().children[m.tree.route(n, key)]
	}
}
func (m *wordMap) insert(key, value uint64) (uint64, bool) {
	lane := m.structure.lane(key)
	lane.RLock()
	if id := m.leafFor(key); id != 0 {
		n := m.tree.node(id)
		n.mu.Lock()
		i := n.exactEntry(key)
		if i < int(n.n) && n.entries()[i].key == key {
			old := n.entries()[i].value
			atomic.StoreUint64(&n.entries()[i].value, value)
			n.mu.Unlock()
			lane.RUnlock()
			return old, true
		}
		if n.n < nodeSize {
			if i == int(n.n) && n.n > 0 && key < n.entries()[n.n-1].key {
				i = n.lowerEntry(key)
			}
			if i > 0 {
				n.beginLeafWrite()
				m.tree.insertLeaf(n, i, key, value)
				m.count.Add(1)
				n.endLeafWrite()
				n.mu.Unlock()
				lane.RUnlock()
				return 0, false
			}
		}
		n.mu.Unlock()
	}
	lane.RUnlock()
	m.structure.Lock()
	defer m.structure.Unlock()
	m.version.Add(1)
	defer m.finishStructure()
	old, replaced := m.tree.insert(key, value)
	if !replaced {
		m.count.Add(1)
	}
	return old, replaced
}
func (m *wordMap) delete(key uint64) (uint64, bool) {
	lane := m.structure.lane(key)
	lane.RLock()
	if id := m.leafFor(key); id != 0 {
		n := m.tree.node(id)
		n.mu.Lock()
		i := n.exactEntry(key)
		if i == int(n.n) || n.entries()[i].key != key {
			n.mu.Unlock()
			lane.RUnlock()
			return 0, false
		}
		if n.n > minNodeSize && i > 0 {
			n.beginLeafWrite()
			value, found := m.tree.remove(id, key)
			m.count.Add(-1)
			n.endLeafWrite()
			n.mu.Unlock()
			lane.RUnlock()
			return value, found
		}
		n.mu.Unlock()
	} else {
		lane.RUnlock()
		return 0, false
	}
	lane.RUnlock()
	m.structure.Lock()
	defer m.structure.Unlock()
	m.version.Add(1)
	defer m.finishStructure()
	value, found := m.tree.delete(key)
	if found {
		m.count.Add(-1)
	}
	return value, found
}
func (m *wordMap) clear() {
	m.structure.Lock()
	defer m.structure.Unlock()
	m.version.Add(1)
	defer m.finishStructure()
	m.tree = wordTree{shared: true, keyKind: m.keyKind, valueKind: m.valueKind}
	m.count.Store(0)
}
func (m *wordMap) read(key uint64, visit func(uint64, uint64)) bool {
	lane := m.structure.lane(key)
	lane.RLock()
	defer lane.RUnlock()
	id := m.leafFor(key)
	if id == 0 {
		return false
	}
	n := m.tree.node(id)
	n.mu.Lock()
	defer n.mu.Unlock()
	i := n.exactEntry(key)
	if i == int(n.n) || n.entries()[i].key != key {
		return false
	}
	visit(n.entries()[i].key, n.entries()[i].value)
	return true
}
func (m *wordMap) update(key uint64, change func(uint64) uint64) (uint64, bool) {
	lane := m.structure.lane(key)
	lane.RLock()
	defer lane.RUnlock()
	id := m.leafFor(key)
	if id == 0 {
		return 0, false
	}
	n := m.tree.node(id)
	n.mu.Lock()
	defer n.mu.Unlock()
	i := n.exactEntry(key)
	if i == int(n.n) || n.entries()[i].key != key {
		return 0, false
	}
	value := change(n.entries()[i].value)
	atomic.StoreUint64(&n.entries()[i].value, value)
	return value, true
}
func (m *wordMap) lookupLocked(key uint64) (uint64, bool) {
	id := m.leafFor(key)
	if id == 0 {
		return 0, false
	}
	n := m.tree.node(id)
	i := n.exactEntry(key)
	if i == int(n.n) || n.entries()[i].key != key {
		return 0, false
	}
	return n.entries()[i].value, true
}
func (m *wordMap) updateOrInsert(key, value uint64, merge func(uint64, uint64) uint64) (uint64, bool) {
	m.structure.Lock()
	defer m.structure.Unlock()
	old, found := m.lookupLocked(key)
	if found {
		value = merge(old, value)
	}
	m.version.Add(1)
	defer m.finishStructure()
	m.tree.insert(key, value)
	if !found {
		m.count.Add(1)
	}
	return old, found
}
func (m *wordMap) loadOrStore(key, value uint64) (uint64, bool) {
	m.structure.Lock()
	defer m.structure.Unlock()
	if old, found := m.lookupLocked(key); found {
		return old, true
	}
	m.version.Add(1)
	defer m.finishStructure()
	m.tree.insert(key, value)
	m.count.Add(1)
	return value, false
}
func (m *wordMap) deleteIf(key uint64, predicate func(uint64) bool) (uint64, bool) {
	m.structure.Lock()
	defer m.structure.Unlock()
	old, found := m.lookupLocked(key)
	if !found || !predicate(old) {
		return 0, false
	}
	m.version.Add(1)
	defer m.finishStructure()
	m.tree.delete(key)
	m.count.Add(-1)
	return old, true
}
func (m *wordMap) walk(lower, upper uint64, reverse, bounded bool, visit func(uint64, uint64) bool) {
	m.structure.RLock()
	defer m.structure.RUnlock()
	if m.tree.root == 0 || (bounded && lower >= upper) {
		return
	}
	id := m.tree.end(reverse)
	if bounded {
		key := lower
		if reverse {
			key = upper
		}
		id = m.leafFor(key)
	}
	for id != 0 {
		n := m.tree.node(id)
		if !n.visitNumeric(lower, upper, reverse, bounded, visit) {
			return
		}
		if reverse {
			id = n.prev
		} else {
			id = n.next
		}
	}
}
func (n *wordNode) visitNumeric(lower, upper uint64, reverse, bounded bool, visit func(uint64, uint64) bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if reverse {
		start := int(n.n) - 1
		if bounded {
			start = n.lowerEntry(upper) - 1
		}
		for i := start; i >= 0; i-- {
			entry := n.entries()[i]
			if bounded && entry.key < lower || !visit(entry.key, entry.value) {
				return false
			}
		}
	} else {
		start := 0
		if bounded {
			start = n.lowerEntry(lower)
		}
		for i := start; i < int(n.n); i++ {
			entry := n.entries()[i]
			if bounded && entry.key >= upper || !visit(entry.key, entry.value) {
				return false
			}
		}
	}
	return true
}
func (m *wordMap) edge(last, pop bool) (uint64, uint64, bool) {
	m.structure.Lock()
	defer m.structure.Unlock()
	id := m.tree.end(last)
	if id == 0 {
		return 0, 0, false
	}
	n := m.tree.node(id)
	i := 0
	if last {
		i = int(n.n) - 1
	}
	entry := n.entries()[i]
	if pop {
		m.version.Add(1)
		defer m.finishStructure()
		m.tree.delete(entry.key)
		m.count.Add(-1)
	}
	return entry.key, entry.value, true
}
