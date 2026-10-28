package gobbtree

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

// orderingZero is never written. An address offset by a loaded value masked
// with it carries an address dependency on that value: arm64 performs the
// dependent load after the load it depends on, and amd64 orders all loads.
// The Go memory model does not promise this; it is the hardware's rule, and
// every plain load below is validated by a seqlock that relies on it.
var orderingZero uintptr

func after[T any](p *T, acc uint64) *T {
	return (*T)(unsafe.Add(unsafe.Pointer(p), uintptr(acc)&orderingZero))
}

// getOrdered is get's descent through the uncached large map, with plain
// loads. Acquire loads would hold every later lookup's loads until this
// lookup's cache misses return. Instead the opening epoch is the only acquire
// load; the leaf's opening sequence feeds the addresses of the entries it
// guards, and every loaded word, including those that only steer branches,
// feeds the addresses of the closing sequence and epoch loads. ok is false
// when the read must take the atomic path: a cell to fill, a leaf being
// written, or a failed validation.
func (m *wordMap) getOrdered(view *wordReadView, key, version uint64) (value uint64, found, ok bool) {
	var id uint32
	var acc uint64
	if c := view.leaves; c != nil {
		var fill bool
		id, fill, acc = c.lookupOrdered(key)
		if fill {
			return 0, false, false
		}
	}
	if id == 0 {
		routes := &view.routes
		if index := (key - routes.base) >> routes.shift; index < uint64(len(routes.ids)) {
			id = routes.ids[index]
		}
		if id == 0 {
			id = view.root
		}
	}
	for depth := 0; depth <= int(view.level); depth++ {
		acc ^= uint64(id)
		if id == 0 {
			return 0, false, false
		}
		index := (id &^ wordLeafID) - 1
		blockIndex := int(index >> 8)
		if blockIndex >= len(view.blocks) {
			return 0, false, false
		}
		block := *(**wordNode)(unsafe.Pointer(&view.blocks[blockIndex]))
		if block == nil || blockIndex < 8 && int(index&255) >= wordSlabSize(blockIndex) {
			return 0, false, false
		}
		n := wordSlabNode(block, index)
		sequence := *(*uint64)(unsafe.Pointer(&n.sequence))
		acc ^= sequence
		count := int(sequence >> 1 & 63)
		if id&wordLeafID != 0 {
			if sequence&1 != 0 {
				return 0, false, false
			}
			// The guarded words load after the opening sequence.
			leaf := after(n, sequence)
			entries := leaf.entries()
			tags := leaf.tagWords()
			acc ^= tags[0] ^ tags[1] ^ tags[2] ^ tags[3]
			i, candidate, read, proven := leaf.findOrdered(key, count)
			if !proven {
				return 0, false, false
			}
			acc ^= candidate ^ read ^ uint64(i)
			found = i < count && candidate == key
			if found {
				value = entries[i].value
				acc ^= value
			}
			closing := *after((*uint64)(unsafe.Pointer(&n.sequence)), acc)
			epoch := *after((*uint64)(unsafe.Pointer(&m.version)), acc)
			if closing != sequence || epoch != version {
				return 0, false, false
			}
			return value, found, true
		}
		if count == 0 {
			return 0, false, false
		}
		i, read, proven := routeOrdered(n, key, count)
		if !proven {
			return 0, false, false
		}
		acc ^= read
		id = n.inner().children[i]
	}
	return 0, false, false
}

// routeOrdered routes key through a shared internal node with count
// children, with plain loads, to the last separator at most key. A search's
// result may depend on the words it read only through branches, which order
// nothing on arm64, so the separators that bracket key are loaded again and
// returned in acc: in the validated snapshot they prove the route, whatever
// the search read. proven is false when they do not bracket key.
func routeOrdered(n *wordNode, key uint64, count int) (i int, acc uint64, proven bool) {
	i = routeCode(&n.keys, count, key)
	acc = uint64(i)
	if i > 0 {
		low := n.keys[i&(nodeSize-1)]
		acc ^= low
		if low > key {
			return i, acc, false
		}
	}
	if i+1 < count {
		high := n.keys[(i+1)&(nodeSize-1)]
		acc ^= high
		if high <= key {
			return i, acc, false
		}
	}
	return i, acc, true
}

// findOrdered finds key among a shared leaf's count entries with plain loads,
// for a caller that folds the leaf's tag words into its validation. It
// returns key's slot, or the slot a miss stops at, the key loaded there, and
// in acc every other word it decided by. Like routeOrdered it proves a miss
// by loads the caller validates: no tag matches, the first matching slot
// holds a greater key, or the key below the lower bound is less. proven is
// false when those loads disagree.
func (n *wordNode) findOrdered(key uint64, count int) (i int, candidate, acc uint64, proven bool) {
	entries := n.entries()
	i = firstTag(&n.tags, count, key)
	if i >= count {
		return i, 0, 0, true
	}
	candidate = entries[i&(nodeSize-1)].key
	if candidate >= key {
		return i, candidate, 0, true
	}
	i = n.lowerEntry(key)
	if i > 0 {
		acc = entries[(i-1)&(nodeSize-1)].key
		if acc >= key {
			return i, candidate, acc, false
		}
	}
	if i < count {
		candidate = entries[i&(nodeSize-1)].key
		if candidate < key {
			return i, candidate, acc, false
		}
	}
	return i, candidate, acc, true
}

// routeAtomic is routeOrdered with atomic loads.
func routeAtomic(n *wordNode, key uint64, count int) int {
	// Only the count separators load; padding sorts above every key.
	var keys [nodeSize]uint64
	for i := range keys {
		keys[i] = ^uint64(0)
		if i < count {
			keys[i] = atomic.LoadUint64(&n.keys[i])
		}
	}
	return routeCode(&keys, count, key)
}

// lookupOrdered is lookup with plain loads; acc carries the words it read.
// A published cell is immutable, and its tail loads after its head.
func (c *wordLeafCache) lookupOrdered(key uint64) (id uint32, fill bool, acc uint64) {
	index := (key - c.base) >> c.shift
	if index >= uint64(len(c.cells)) {
		return 0, false, 0
	}
	cell := &c.cells[index]
	head := *(*uint64)(unsafe.Pointer(&cell.head))
	if head <= 2 {
		return 0, head == 0, head
	}
	first, second := head&leafCacheIDMask, head>>24&leafCacheIDMask
	if first == second {
		return uint32(first) | wordLeafID, false, head
	}
	tail := *after((*uint64)(unsafe.Pointer(&cell.tail)), head)
	acc = head ^ tail
	cut0 := head >> 48 & 1023
	cut1 := head>>58 | ((tail >> 48 & 15) << 6)
	cut2 := tail >> 52 & 1023
	q := c.fraction(key)
	if c.shift > 10 && (q == cut0 || q == cut1 || q == cut2) {
		return 0, false, acc
	}
	leaf := first
	if q >= cut0 {
		leaf = second
	}
	if q >= cut1 {
		leaf = tail & leafCacheIDMask
	}
	if q >= cut2 {
		leaf = tail >> 24 & leafCacheIDMask
	}
	return uint32(leaf) | wordLeafID, false, acc
}

// getMany8Ordered is getMany8 with plain loads, ordered as in getOrdered:
// each lane's guarded words load after its opening sequence, and its closing
// sequence and the closing epoch load after everything the lanes read.
// Lanes it cannot validate report false in ok.
func (m *wordMap) getMany8Ordered(view *wordReadView, keys *[batchLanes]uint64, dst *[batchLanes]Result[uint64], ok *[batchLanes]bool) (acc uint64) {
	var nodes [batchLanes]*wordNode
	var lanes [batchLanes]uint64
	for j := 0; j < batchLanes; j++ {
		id, _, read := view.leaves.lookupOrdered(keys[j])
		lanes[j] = read ^ uint64(id)
		if id == 0 {
			continue
		}
		index := (id &^ wordLeafID) - 1
		blockIndex := int(index >> 8)
		if blockIndex >= len(view.blocks) {
			continue
		}
		block := *(**wordNode)(unsafe.Pointer(&view.blocks[blockIndex]))
		if block == nil || blockIndex < 8 && int(index&255) >= wordSlabSize(blockIndex) {
			continue
		}
		nodes[j] = wordSlabNode(block, index)
	}
	var sequences [batchLanes]uint64
	for j := 0; j < batchLanes; j++ {
		if n := nodes[j]; n != nil {
			sequences[j] = *(*uint64)(unsafe.Pointer(&n.sequence))
			if sequences[j]&1 != 0 {
				nodes[j] = nil
			} else {
				// The guarded words load after the opening sequence.
				nodes[j] = after(n, sequences[j])
			}
		}
	}
	var positions [batchLanes]int
	for j := 0; j < batchLanes; j++ {
		if n := nodes[j]; n != nil {
			positions[j] = firstTag(&n.tags, int(sequences[j]>>1&63), keys[j])
		}
	}
	for j := 0; j < batchLanes; j++ {
		dst[j] = Result[uint64]{}
		n := nodes[j]
		if n == nil {
			continue
		}
		i, size := positions[j], int(sequences[j]>>1&63)
		entries := n.entries()
		// Misses are proven as in findOrdered.
		tags := n.tagWords()
		read := lanes[j] ^ sequences[j] ^ tags[0] ^ tags[1] ^ tags[2] ^ tags[3]
		proven := true
		var key, value uint64
		if i < size {
			key = entries[i&(nodeSize-1)].key
			if key < keys[j] {
				i = n.lowerEntry(keys[j])
				if i > 0 {
					below := entries[(i-1)&(nodeSize-1)].key
					read ^= below
					proven = below < keys[j]
				}
				if i < size {
					key = entries[i&(nodeSize-1)].key
					proven = proven && key >= keys[j]
				}
			}
			if proven && i < size && key == keys[j] {
				value = entries[i&(nodeSize-1)].value
				dst[j] = Result[uint64]{value, true}
			}
		}
		read ^= key ^ value ^ uint64(i)
		closing := *after((*uint64)(unsafe.Pointer(&n.sequence)), read)
		ok[j] = proven && closing == sequences[j]
		acc ^= read
	}
	return acc
}

// slabNode resolves a node ID through a published view, or nil.
func (view *wordReadView) slabNode(id uint32) *wordNode {
	index := (id &^ wordLeafID) - 1
	blockIndex := int(index >> 8)
	if id == 0 || blockIndex >= len(view.blocks) {
		return nil
	}
	block := *(**wordNode)(unsafe.Pointer(&view.blocks[blockIndex]))
	if block == nil || blockIndex < 8 && int(index&255) >= wordSlabSize(blockIndex) {
		return nil
	}
	return wordSlabNode(block, index)
}

// leafOrdered routes key to its leaf with plain loads, as getOrdered does;
// acc carries every word read, for the caller's closing validation.
func (m *wordMap) leafOrdered(view *wordReadView, key uint64) (id uint32, acc uint64, ok bool) {
	if c := view.leaves; c != nil {
		var fill bool
		id, fill, acc = c.lookupOrdered(key)
		if fill {
			id = 0
		}
	}
	if id == 0 {
		routes := &view.routes
		if index := (key - routes.base) >> routes.shift; index < uint64(len(routes.ids)) {
			id = routes.ids[index]
		}
		if id == 0 {
			id = view.root
		}
	}
	for depth := 0; depth <= int(view.level); depth++ {
		acc ^= uint64(id)
		if id&wordLeafID != 0 {
			return id, acc, true
		}
		n := view.slabNode(id)
		if n == nil {
			return 0, 0, false
		}
		sequence := *(*uint64)(unsafe.Pointer(&n.sequence))
		acc ^= sequence
		count := int(sequence >> 1 & 63)
		if count == 0 {
			return 0, 0, false
		}
		i, read, proven := routeOrdered(n, key, count)
		if !proven {
			return 0, 0, false
		}
		acc ^= read
		id = n.inner().children[i]
	}
	return 0, 0, false
}

// copyOrdered is copyLeaf without latches: leaves are copied with plain
// loads and validated by their sequences and the structural epoch, as
// lookups are. One loadFence orders a batch's copies before its closing
// checks. ok is false when a writer interfered; the caller retries or latches.
func (m *wordMap) copyOrdered(low, high uint64, limited, reverse, bounded bool, buf *[scanLeaves * nodeSize]wordEntry, next *uint32, epoch *uint64) (count int, done, ok bool) {
	version := m.version.Load()
	if version&1 != 0 {
		return 0, false, false
	}
	view := m.view.Load()
	if view == nil {
		return 0, true, m.version.Load() == version
	}
	if !view.ready {
		// As in get, the first optimistic reader activates the arena.
		m.enableReads()
		return 0, false, false
	}
	var id uint32
	// Only a leaf reached by descent holds keys outside the range still to
	// visit; a chained leaf starts past everything already visited.
	searched := false
	routed := true
	switch {
	case *next != 0 && *epoch == version:
		id = *next
	case !reverse:
		id, _, routed = m.leafOrdered(view, low)
	case limited:
		id, _, routed = m.leafOrdered(view, high)
	default:
		id, _, routed = m.leafOrdered(view, ^uint64(0))
	}
	if !routed {
		return 0, false, false
	}
	searched = *next == 0 || *epoch != version
	var leaves [scanLeaves]*wordNode
	var sequences [scanLeaves]uint64
	copied := 0
	for id != 0 {
		n := view.slabNode(id)
		if n == nil {
			return 0, false, false
		}
		sequence := *(*uint64)(unsafe.Pointer(&n.sequence))
		if sequence&1 != 0 {
			return 0, false, false
		}
		leaves[copied], sequences[copied] = n, sequence
		copied++
		// The guarded words load after the opening sequence.
		n = after(n, sequence)
		entries := n.entries()
		size := min(int(sequence>>1&63), nodeSize)
		if reverse {
			end := size
			if limited && searched {
				end = n.lowerEntry(high)
			}
			for i := min(end, size) - 1; i >= 0; i-- {
				entry := entries[i]
				if bounded && entry.key < low {
					done = true
					break
				}
				buf[count] = entry
				count++
			}
			id = n.prev
		} else {
			start := 0
			if searched {
				start = n.lowerEntry(low)
			}
			for i := start; i < size; i++ {
				entry := entries[i]
				if bounded && entry.key >= high {
					done = true
					break
				}
				buf[count] = entry
				count++
			}
			id = n.next
		}
		searched = false
		if done || copied == scanLeaves {
			break
		}
	}
	loadFence()
	for i := 0; i < copied; i++ {
		if leaves[i].sequence.Load() != sequences[i] {
			return 0, false, false
		}
	}
	if m.version.Load() != version {
		return 0, false, false
	}
	*next, *epoch = id, version
	return count, done || id == 0, true
}

// Lane-free writers. Once readers are active, a leaf write routes to its leaf
// as a lookup does, locks only that leaf, and checks that the structural
// epoch has not moved since it routed. Structural writers bump the epoch and
// then lock every leaf they can change (lockWindow), so a leaf writer either
// sees the new epoch and routes again, or finishes before the structural
// writer takes its leaf. Race builds and maps without readers use lanes.

// laneFree reports whether writes skip lanes: readers are active, so every
// leaf write holds its leaf.
func (m *wordMap) laneFree() bool {
	if !seqlockPlainStores {
		return false
	}
	view := m.view.Load()
	return view != nil && view.ready
}

// contended is called with a lane guard: writers contend. It makes writes
// lane-free, as a first reader does, and reports true for the caller to
// retry, having released the guard: in lanes, every split or merge stops all
// writers. Otherwise it returns the guard to use; a map with no tree to
// publish stays in lanes.
func (m *wordMap) contended(guard structuralGuard, key uint64) (structuralGuard, bool) {
	if !seqlockPlainStores || m.laneFree() {
		return guard, false
	}
	guard.unlock()
	m.enableReads()
	if m.laneFree() {
		return structuralGuard{}, true
	}
	return m.structure.acquire(key), false
}

// lockLeafIfLaneFree routes key to its leaf and locks it when writes are
// lane-free, with the epoch unchanged since routing. ok is false when the map
// is not ready for lane-free writes, or routing keeps meeting structure.
func (m *wordMap) lockLeafIfLaneFree(key uint64) (n *wordNode, id uint32, view *wordReadView, ok bool) {
	if !seqlockPlainStores {
		return nil, 0, nil, false
	}
	for attempt := 0; attempt < 64; attempt++ {
		version := m.version.Load()
		view = m.view.Load()
		if view == nil || !view.ready {
			return nil, 0, nil, false
		}
		if version&1 != 0 {
			runtime.Gosched()
			continue
		}
		id = 0
		if view.cache != nil {
			// A small map's cache routes reach the leaf directly.
			id = view.cache.routes.lookupAtomic(key)
		}
		if id == 0 {
			var routed bool
			if id, _, routed = m.leafOrdered(view, key); !routed {
				continue
			}
		}
		if n = view.slabNode(id); n == nil || id&wordLeafID == 0 {
			continue
		}
		n.mu.Lock()
		if m.version.Load() == version {
			return n, id, view, true
		}
		n.mu.Unlock()
	}
	return nil, 0, nil, false
}

// lockWindow locks the leaves a structural change around key can touch: its
// leaf and two neighbours on each side, which cover a pair split, borrowing
// from the outer leaf, and a merge. The caller holds the structure lock.
func (m *wordMap) lockWindow(key uint64) { m.lockWindowAt(m.leafFor(key)) }
func (m *wordMap) lockWindowAt(id uint32) {
	if id == 0 {
		return
	}
	first, last := id, id
	for range 2 {
		if prev := m.tree.node(first).prev; prev != 0 {
			first = prev
		}
		if next := m.tree.node(last).next; next != 0 {
			last = next
		}
	}
	// Left to right; a leaf writer holds one leaf and waits for nothing.
	for i, id := 0, first; ; i, id = i+1, m.tree.node(id).next {
		n := m.tree.node(id)
		n.mu.Lock()
		m.held[i] = n
		if id == last {
			return
		}
	}
}

// unlockWindow releases what lockWindow took.
func (m *wordMap) unlockWindow() {
	for i, n := range m.held {
		if n != nil {
			n.mu.Unlock()
			m.held[i] = nil
		}
	}
}

// lockAllLeaves locks every leaf, for a change to the whole map.
func (m *wordMap) lockAllLeaves() []*wordNode {
	var held []*wordNode
	for id := m.tree.end(false); id != 0; id = m.tree.node(id).next {
		n := m.tree.node(id)
		n.mu.Lock()
		held = append(held, n)
	}
	return held
}

// insertInLeaf inserts or replaces key within its locked leaf when that
// changes no other node; done is false when the insert needs structure.
func (m *wordMap) insertInLeaf(n *wordNode, id uint32, view *wordReadView, key, value uint64) (old uint64, replaced, done bool) {
	i := n.exactEntry(key)
	if i < int(n.n) && n.entries()[i].key == key {
		old = n.entries()[i].value
		storeValueIn(view, id, n, i, value)
		return old, true, true
	}
	if n.n == nodeSize {
		return 0, false, false
	}
	if i == int(n.n) && n.n > 0 && key < n.entries()[n.n-1].key {
		i = n.lowerEntry(key)
	}
	if i == 0 && n.prev == 0 {
		// Separators bound their subtrees' keys from below, so a new
		// minimum changes none, except below the leftmost leaf.
		return 0, false, false
	}
	m.disableReadCache()
	n.beginLeafWrite()
	m.tree.insertLeaf(n, i, key, value)
	m.addCount(1, false)
	n.endLeafWrite()
	return 0, false, true
}

// deleteInLeaf removes key within its locked leaf when that changes no other
// node; done is false when the removal needs structure.
func (m *wordMap) deleteInLeaf(n *wordNode, key uint64) (value uint64, found, done bool) {
	i := n.exactEntry(key)
	if i == int(n.n) || n.entries()[i].key != key {
		return 0, false, true
	}
	if n.n <= minNodeSize || i == 0 && n.prev == 0 {
		// Removing a leaf's minimum leaves its separator a lower bound;
		// the tree's minimum stays exact.
		return 0, false, false
	}
	m.disableReadCache()
	n.beginLeafWrite()
	value = m.tree.removeLeaf(n, i)
	m.addCount(-1, false)
	n.endLeafWrite()
	return value, true, true
}

// structural runs change under the structure lock, with the epoch odd and
// the leaves around key locked, so that lane-free writers stay out.
func (m *wordMap) structural(key uint64, change func()) {
	m.structure.Lock()
	defer m.structure.Unlock()
	m.disableReadCache()
	m.version.Add(1)
	m.lockWindow(key)
	defer m.unlockWindow()
	defer m.finishStructure()
	change()
}
