package gobbtree

import (
	"math/bits"
	"runtime"
	"sync/atomic"
	"unsafe"
)

type wordReadView struct {
	root     uint32
	level    uint8
	ready    bool
	blocks   []atomic.Pointer[wordNode]
	routes   wordRoutes
	cache    *wordReadCache
	leaves   *wordLeafCache
	uncached *wordReadView
}
type wordMap struct {
	version            atomic.Uint64
	view               atomic.Pointer[wordReadView]
	keyKind, valueKind uint8
	_                  [46]byte
	structure          *structuralLock
	count              *atomic.Int64
	tree               wordTree
	// held is what lockWindow locked, for the structural writer that took it.
	held [5]*wordNode
}

// Publish the shared arena directories; an optional small-map cache is attached
// after reader activation. Arena reads validate leaf and structural epochs.
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
	// Structural changes retire caches before publication. Reader activation
	// publishes its ready arena view before attaching an optional cache.
	m.view.Store(view)
}
func (m *wordMap) finishStructure() { m.publish(); m.version.Add(1) }

// The first optimistic reader waits for all existing writers. Before that
// barrier, leaf locks suffice and writers can use ordinary bulk copies.
func (m *wordMap) enableReads() {
	m.structure.Lock()
	defer m.structure.Unlock()
	if m.tree.atomicWrites {
		return
	}
	// Invalidate readers that capture the intermediate ready arena view.
	// They must not survive cache attachment: replacements update live and
	// cached values separately. Complete the epoch before unlocking on exit.
	m.version.Add(1)
	defer m.version.Add(1)
	m.tree.atomicWrites = true
	m.publish()
	if current := m.view.Load(); current != nil {
		if m.count.Load() > smallReadCacheLimit {
			view := *current
			view.uncached = current
			view.leaves = newWordLeafCache(view.routes, view.level, m.count.Load())
			m.view.Store(&view)
			return
		}
		view := *current
		view.uncached = current
		nodes := 0
		for i := range m.tree.slabs {
			nodes += wordSlabSize(i)
		}
		view.cache = &wordReadCache{nodes: make([]wordNode, nodes)}
		if root := m.tree.rootNode; root.level > 0 {
			width := bits.Len64(root.keys[0] ^ root.keys[root.n-1])
			radixBits := min(width, bits.Len64(uint64(m.count.Load()-1)/2))
			view.cache.routes = wordRoutes{base: root.keys[0] & (^uint64(0) << width), shift: uint8(width - radixBits), ids: make([]uint32, 1<<radixBits)}
		}
		m.view.Store(&view)
	}
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
		if view.cache != nil {
			id := view.cache.routes.lookupAtomic(key)
			direct := id != 0
			low, high := uint64(0), ^uint64(0)
			if !direct {
				id = view.routes.lookupAtomic(key)
				if id == 0 {
					id = view.root
				} else {
					low = view.routes.base | ((key-view.routes.base)>>view.routes.shift)<<view.routes.shift
					high = low | ((uint64(1) << view.routes.shift) - 1)
				}
			}
			for depth := 0; depth <= int(view.level); depth++ {
				n := view.cache.node(id)
				if n == nil {
					continue retry
				}
				if n.sequence.Load() == 0 {
					n = m.cacheNode(view.cache, id)
					if n == nil {
						continue retry
					}
				}
				if id&wordLeafID != 0 {
					if !direct {
						routes := &view.cache.routes
						bucket := (key - routes.base) >> routes.shift
						if bucket < uint64(len(routes.ids)) {
							first := routes.base | bucket<<routes.shift
							last := first | ((uint64(1) << routes.shift) - 1)
							if first >= low && last <= high {
								atomic.StoreUint32(&routes.ids[bucket], id)
							}
						}
					}
					count := int(n.n)
					i := firstTag(&n.tags, count, key)
					var candidate uint64
					if i < count {
						candidate = n.entries()[i].key
						if candidate < key {
							i = n.lowerEntry(key)
							if i < count {
								candidate = n.entries()[i].key
							}
						}
					}
					found := i < count && candidate == key
					var value uint64
					if found {
						value = atomic.LoadUint64(&n.entries()[i].value)
					}
					if m.version.Load() != version {
						continue retry
					}
					return value, found
				}
				shift := uint(n.level)
				prefix := *n.prefixWord() >> shift
				projection := key >> shift
				var i int
				if projection < prefix {
					i = 0
				} else if projection > prefix|255 {
					i = int(n.n) - 1
				} else {
					i = max(0, upperTag(&n.tags, int(n.n), uint8(projection))-1)
					for i > 0 && n.tags[i] == uint8(projection) && n.keys[i] > key {
						i--
					}
				}
				if i > 0 {
					low = n.keys[i]
				}
				if i+1 < int(n.n) {
					high = n.keys[i+1] - 1
				}
				id = n.inner().children[i]
			}
			continue retry
		}

		if seqlockPlainStores {
			if value, found, ok := m.getOrdered(view, key, version); ok {
				return value, found
			}
		}
		var id uint32
		var fill bool
		if view.leaves != nil {
			id, fill = view.leaves.lookup(key)
		}
		// While a leaf cell is empty, track the key bounds of each node so the
		// leaf parent can fill it whenever that parent covers the whole cell.
		low, high := uint64(0), ^uint64(0)
		if id == 0 {
			id = view.routes.lookupAtomic(key)

			if id == 0 {
				id = view.root
			} else if fill {
				low = view.routes.base | ((key-view.routes.base)>>view.routes.shift)<<view.routes.shift
				high = low | ((uint64(1) << view.routes.shift) - 1)
			}
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
			if block == nil || blockIndex < 8 && int(index&255) >= wordSlabSize(blockIndex) {
				continue retry
			}
			n := wordSlabNode(block, index)
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
			child := atomic.LoadUint32(&n.inner().children[i])
			if fill {
				if child&wordLeafID != 0 {
					// Every key from the parent's first to its last separator
					// also routes here, beyond a cached route's single bucket.
					parentLow := min(low, atomic.LoadUint64(&n.keys[0]))
					parentHigh := max(high, atomic.LoadUint64(&n.keys[count-1]))
					m.cacheLeafParent(view.leaves, n, key, count, version, parentLow, parentHigh)
				}
				if i > 0 {
					low = atomic.LoadUint64(&n.keys[i])
				}
				if i+1 < count {
					high = atomic.LoadUint64(&n.keys[i+1]) - 1
				}
			}
			id = child
		}
	}
}

// addCount adjusts the count. An exclusive writer, serial or structural,
// stores it where stores stay ordered: an atomic add is a full barrier. Once
// writes are lane-free, leaf writers add concurrently, so nothing is.
func (m *wordMap) addCount(delta int64, exclusive bool) {
	if exclusive && orderedStores && !m.laneFree() {
		*(*int64)(unsafe.Pointer(m.count)) = m.count.Load() + delta
		return
	}
	m.count.Add(delta)
}
func (m *wordMap) leafFor(key uint64) uint32 {
	id, _, _ := m.locate(key)
	return id
}

// locate also returns the leaf's parent and child index when it descends to
// the leaf; a cached leaf route returns no parent.
func (m *wordMap) locate(key uint64) (uint32, *wordNode, int) {
	if view := m.view.Load(); view != nil {
		if view.cache != nil {
			if id := view.cache.routes.lookupAtomic(key); id != 0 {
				return id, nil, 0
			}
		}
		if view.leaves != nil {
			if id, _ := view.leaves.lookup(key); id != 0 {
				return id, nil, 0
			}
		}
	}
	if m.tree.root == 0 {
		return 0, nil, 0
	}
	id := m.tree.routes.lookup(key)
	if id == 0 {
		id = m.tree.root
	}
	var parent *wordNode
	index := 0
	for {
		n := m.tree.node(id)
		if n.leaf {
			return id, parent, index
		}
		parent, index = n, m.tree.route(n, key)
		id = n.inner().children[index]
	}
}
func (m *wordMap) insert(key, value uint64) (uint64, bool) {
	if !m.laneFree() {
		// Before readers are active, writers take lanes.
	} else if n, id, view, ok := m.lockLeafIfLaneFree(key); ok {
		old, replaced, done := m.insertInLeaf(n, id, view, key, value)
		n.mu.Unlock()
		if done {
			return old, replaced
		}
		m.structural(key, func() {
			if !m.tree.insertSharedAtRoute(key, value) {
				old, replaced = m.tree.insert(key, value)
			}
			if !replaced {
				m.addCount(1, true)
			}
		})
		return old, replaced
	}
	guard := m.structure.acquire(key)
	// Once readers are active, lane-free writers may share any leaf, so this
	// writer holds its leaf too.
	serial := guard.lane == nil && !m.laneFree()
	// Before readers, a serial writer appends past the largest key straight
	// to the rightmost leaf, as Tree does.
	plain := serial && !m.tree.atomicWrites
	if n := m.tree.tail; plain && n != nil && n.next == 0 && n.n > 0 && key > n.entries()[n.n-1].key {
		if n.n < nodeSize {
			m.tree.insertedShared.Store(key)
			n.appendLeafPlain(key, value)
			n.sequence.Store(uint64(n.n) << 1)
		} else if !m.tree.appendFull(n, key, value) {
			n = nil
		}
		if n != nil {
			m.addCount(1, true)
			guard.unlock()
			return 0, false
		}
	}
	id, parent, index := m.locate(key)
	var leaf *wordNode
	if id != 0 {
		n := m.tree.node(id)
		leaf = n
		if !serial {
			n.mu.Lock()
		}
		i := n.exactEntry(key)
		if i < int(n.n) && n.entries()[i].key == key {
			old := n.entries()[i].value
			m.storeValue(id, n, i, value)
			if !serial {
				n.mu.Unlock()
			}
			guard.unlock()
			return old, true
		}
		if n.n < nodeSize {
			if i == int(n.n) && n.n > 0 && key < n.entries()[n.n-1].key {
				i = n.lowerEntry(key)
			}
			if i > 0 {
				if m.tree.atomicWrites {
					m.disableReadCache()
					n.beginLeafWrite()
				} else if plain && n.next == 0 {
					m.tree.tail = n
				}
				m.tree.insertLeaf(n, i, key, value)
				m.addCount(1, serial)
				if m.tree.atomicWrites {
					n.endLeafWrite()
				}
				if !serial {
					n.mu.Unlock()
				}
				guard.unlock()
				return 0, false
			}
		}
		if !serial {
			n.mu.Unlock()
		}
	}
	guard = m.lockStructure(guard)
	m.disableReadCache()
	m.version.Add(1)
	if m.laneFree() {
		// Only lane-free writers can hold leaves beside the structure lock.
		m.lockWindow(key)
	}
	defer m.endStructure(guard)
	var old uint64
	var replaced bool
	// A serial writer never released the writer lock, so the located parent
	// still holds the absent key's leaf at index.
	if serial && parent != nil && m.tree.insertAtParent(parent, index, leaf, key, value) {
		m.addCount(1, true)
		return 0, false
	}
	if !m.tree.insertSharedAtRoute(key, value) {
		old, replaced = m.tree.insert(key, value)
	}
	if !replaced {
		m.addCount(1, true)
	}
	return old, replaced
}

// A serial guard already holds the structural lock; a lane guard is
// exchanged for it. endStructure publishes, then releases what was taken.
func (m *wordMap) lockStructure(guard structuralGuard) structuralGuard {
	if guard.lane == nil {
		return guard
	}
	guard.unlock()
	m.structure.Lock()
	return structuralGuard{}
}
func (m *wordMap) endStructure(guard structuralGuard) {
	m.finishStructure()
	m.unlockWindow()
	if guard.g != nil {
		guard.g.writer.Unlock()
	} else {
		m.structure.Unlock()
	}
}
func (m *wordMap) delete(key uint64) (uint64, bool) {
	if !m.laneFree() {
		// Before readers are active, writers take lanes.
	} else if n, _, _, ok := m.lockLeafIfLaneFree(key); ok {
		value, found, done := m.deleteInLeaf(n, key)
		n.mu.Unlock()
		if done {
			return value, found
		}
		m.structural(key, func() {
			if value, found = m.tree.delete(key); found {
				m.addCount(-1, true)
			}
		})
		return value, found
	}
	guard := m.structure.acquire(key)
	// Once readers are active, lane-free writers may share any leaf, so this
	// writer holds its leaf too.
	serial := guard.lane == nil && !m.laneFree()
	id := m.leafFor(key)
	if id == 0 {
		guard.unlock()
		return 0, false
	}
	n := m.tree.node(id)
	if !serial {
		n.mu.Lock()
	}
	i := n.exactEntry(key)
	if i == int(n.n) || n.entries()[i].key != key {
		if !serial {
			n.mu.Unlock()
		}
		guard.unlock()
		return 0, false
	}
	if n.n > minNodeSize && i > 0 {
		if m.tree.atomicWrites {
			m.disableReadCache()
			n.beginLeafWrite()
		}
		value := m.tree.removeLeaf(n, i)
		m.addCount(-1, serial)
		if m.tree.atomicWrites {
			n.endLeafWrite()
		}
		if !serial {
			n.mu.Unlock()
		}
		guard.unlock()
		return value, true
	}
	if !serial {
		n.mu.Unlock()
	}
	guard = m.lockStructure(guard)
	m.disableReadCache()
	m.version.Add(1)
	if m.laneFree() {
		// Only lane-free writers can hold leaves beside the structure lock.
		m.lockWindow(key)
	}
	defer m.endStructure(guard)
	value, found := m.tree.delete(key)
	if found {
		m.addCount(-1, true)
	}
	return value, found
}
func (m *wordMap) clear() {
	m.structure.Lock()
	defer m.structure.Unlock()
	m.disableReadCache()
	m.version.Add(1)
	defer m.finishStructure()
	// Lane-free writers may hold leaves of the tree being dropped.
	for _, n := range m.lockAllLeaves() {
		defer n.mu.Unlock()
	}
	m.tree = wordTree{shared: true, keyKind: m.keyKind, valueKind: m.valueKind}
	m.count.Store(0)
}
func (m *wordMap) read(key uint64, visit func(uint64, uint64)) bool {
	n, _, _, ok := m.lockLeafIfLaneFree(key)
	if !ok {
		guard := m.structure.acquire(key)
		defer guard.unlock()
		id := m.leafFor(key)
		if id == 0 {
			return false
		}
		n = m.tree.node(id)
		n.mu.Lock()
	}
	defer n.mu.Unlock()
	i := n.exactEntry(key)
	if i == int(n.n) || n.entries()[i].key != key {
		return false
	}
	visit(n.entries()[i].key, n.entries()[i].value)
	return true
}
func (m *wordMap) update(key uint64, change func(uint64) uint64) (uint64, bool) {
	n, id, _, ok := m.lockLeafIfLaneFree(key)
	if !ok {
		guard := m.structure.acquire(key)
		defer guard.unlock()
		id = m.leafFor(key)
		if id == 0 {
			return 0, false
		}
		n = m.tree.node(id)
		n.mu.Lock()
	}
	defer n.mu.Unlock()
	i := n.exactEntry(key)
	if i == int(n.n) || n.entries()[i].key != key {
		return 0, false
	}
	value := change(n.entries()[i].value)
	m.storeValue(id, n, i, value)
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
	// Lane-free writers stay out of the leaves this may read or change.
	m.lockWindow(key)
	defer m.unlockWindow()
	if id := m.leafFor(key); id != 0 {
		n := m.tree.node(id)
		i := n.exactEntry(key)
		if i < int(n.n) && n.entries()[i].key == key {
			old := n.entries()[i].value
			value = merge(old, value)
			m.storeValue(id, n, i, value)
			return old, true
		}
	}
	m.disableReadCache()
	m.version.Add(1)
	defer m.finishStructure()
	m.tree.insert(key, value)
	m.addCount(1, true)
	return 0, false
}
func (m *wordMap) loadOrStore(key, value uint64) (uint64, bool) {
	m.structure.Lock()
	defer m.structure.Unlock()
	// Lane-free writers stay out of the leaves this may read or change.
	m.lockWindow(key)
	defer m.unlockWindow()
	if old, found := m.lookupLocked(key); found {
		return old, true
	}
	m.disableReadCache()
	m.version.Add(1)
	defer m.finishStructure()
	m.tree.insert(key, value)
	m.addCount(1, true)
	return value, false
}
func (m *wordMap) deleteIf(key uint64, predicate func(uint64) bool) (uint64, bool) {
	m.structure.Lock()
	defer m.structure.Unlock()
	// Lane-free writers stay out of the leaves this may read or change.
	m.lockWindow(key)
	defer m.unlockWindow()
	old, found := m.lookupLocked(key)
	if !found || !predicate(old) {
		return 0, false
	}
	m.disableReadCache()
	m.version.Add(1)
	defer m.finishStructure()
	m.tree.delete(key)
	m.addCount(-1, true)
	return old, true
}

// walk visits entries in key order, a leaf at a time. Each leaf is copied
// under its latches, which are released before visit runs: as in upstream's
// scan, visit holds no latch, writers wait at most for one leaf's copy, and
// the walk is not a snapshot. An entry present throughout is visited once;
// a walk resumes just past the last key it visited.
func (m *wordMap) walk(lower, upper uint64, reverse, bounded bool, visit func(uint64, uint64) bool) {
	if bounded && lower >= upper {
		return
	}
	var buf [scanLeaves * nodeSize]wordEntry
	// The open end of the range still to visit: inclusive below, exclusive
	// above. started reports that an unbounded reverse walk has an end. next
	// is the following leaf, valid while the structural epoch stays epoch.
	low, high, started := lower, upper, bounded
	var next uint32
	var epoch uint64
	for {
		count, done, ok := 0, false, false
		if seqlockPlainStores {
			for attempt := 0; attempt < 4 && !ok; attempt++ {
				count, done, ok = m.copyOrdered(low, high, started, reverse, bounded, &buf, &next, &epoch)
				if !ok {
					next = 0
				}
			}
		}
		if !ok {
			count, done = m.copyLeaf(low, high, started, reverse, bounded, &buf, &next, &epoch)
		}
		for i := 0; i < count; i++ {
			if !visit(buf[i].key, buf[i].value) {
				return
			}
		}
		if done || count == 0 {
			return
		}
		last := buf[count-1].key
		if reverse {
			high, started = last, true
		} else {
			if last == ^uint64(0) {
				return
			}
			low = last + 1
		}
	}
}

// scanLeaves bounds the leaves a scan copies per latch acquisition, and so
// how long it can hold back a structural writer.
const scanLeaves = 4

// copyLeaf copies, in visiting order, the entries of up to scanLeaves leaves
// from the first that holds any in range: at least low ascending, below high descending (when
// limited), and inside [lower, upper) when bounded. done reports that no
// later leaf can hold more.
func (m *wordMap) copyLeaf(low, high uint64, limited, reverse, bounded bool, buf *[scanLeaves * nodeSize]wordEntry, next *uint32, epoch *uint64) (count int, done bool) {
	m.structure.RLock()
	defer m.structure.RUnlock()
	if m.tree.root == 0 {
		return 0, true
	}
	version := m.version.Load()
	id := m.tree.end(reverse)
	if *next != 0 && *epoch == version {
		// No split or merge since the last leaf: continue along the chain.
		id = *next
	} else if !reverse {
		id = m.leafFor(low)
	} else if limited {
		id = m.leafFor(high)
	}
	for id != 0 {
		n := m.tree.node(id)
		n.mu.Lock()
		if reverse {
			end := int(n.n)
			if limited {
				end = n.lowerEntry(high)
			}
			for i := end - 1; i >= 0; i-- {
				entry := n.entries()[i]
				if bounded && entry.key < low {
					done = true
					break
				}
				buf[count] = entry
				count++
			}
			id = n.prev
		} else {
			for i := n.lowerEntry(low); i < int(n.n); i++ {
				entry := n.entries()[i]
				if bounded && entry.key >= high {
					done = true
					break
				}
				buf[count] = entry
				count++
			}
			id = n.next
		}
		n.mu.Unlock()
		if done || count > len(buf)-nodeSize {
			break
		}
	}
	*next, *epoch = id, version
	return count, done || id == 0
}

func (m *wordMap) edge(last, pop bool) (uint64, uint64, bool) {
	m.structure.Lock()
	defer m.structure.Unlock()
	id := m.tree.end(last)
	if id == 0 {
		return 0, 0, false
	}
	m.lockWindowAt(id)
	defer m.unlockWindow()
	n := m.tree.node(id)
	i := 0
	if last {
		i = int(n.n) - 1
	}
	entry := n.entries()[i]
	if pop {
		m.disableReadCache()
		m.version.Add(1)
		defer m.finishStructure()
		m.tree.delete(entry.key)
		m.addCount(-1, true)
	}
	return entry.key, entry.value, true
}
