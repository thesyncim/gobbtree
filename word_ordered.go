package gobbtree

import "unsafe"

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
			i := firstTag(&leaf.tags, count, key)
			var candidate uint64
			if i < count {
				candidate = entries[i].key
				if candidate < key {
					i = leaf.lowerEntry(key)
					if i < count {
						candidate = entries[i].key
					}
				}
			}
			acc ^= candidate ^ uint64(i)
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
		shift := uint(sequence >> 7 & 63)
		prefix := *n.prefixWord() >> shift
		acc ^= prefix
		projection := key >> shift
		var i int
		if projection < prefix {
			i = 0
		} else if projection > prefix|255 {
			i = count - 1
		} else {
			upper := upperTag(&n.tags, count, uint8(projection))
			i = max(0, upper-1)
			if upper > 0 && n.tags[upper-1] == uint8(projection) {
				for i > 0 {
					separator := n.keys[i]
					acc ^= separator
					if separator <= key {
						break
					}
					i--
				}
			}
		}
		acc ^= uint64(i)
		id = n.inner().children[i]
	}
	return 0, false, false
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
		var key, value uint64
		if i < size {
			key = entries[i].key
			if key < keys[j] {
				i = n.lowerEntry(keys[j])
				if i < size {
					key = entries[i].key
				}
			}
			if i < size && key == keys[j] {
				value = entries[i].value
				dst[j] = Result[uint64]{value, true}
			}
		}
		read := lanes[j] ^ sequences[j] ^ key ^ value ^ uint64(i)
		closing := *after((*uint64)(unsafe.Pointer(&n.sequence)), read)
		ok[j] = closing == sequences[j]
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
		shift := uint(sequence >> 7 & 63)
		prefix := *n.prefixWord() >> shift
		acc ^= prefix
		projection := key >> shift
		var i int
		if projection < prefix {
			i = 0
		} else if projection > prefix|255 {
			i = count - 1
		} else {
			upper := upperTag(&n.tags, count, uint8(projection))
			i = max(0, upper-1)
			if upper > 0 && n.tags[upper-1] == uint8(projection) {
				for i > 0 {
					separator := n.keys[i]
					acc ^= separator
					if separator <= key {
						break
					}
					i--
				}
			}
		}
		acc ^= uint64(i)
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
	switch {
	case *next != 0 && *epoch == version:
		id = *next
	case !reverse:
		id, _, _ = m.leafOrdered(view, low)
	case limited:
		id, _, _ = m.leafOrdered(view, high)
	default:
		id, _, _ = m.leafOrdered(view, ^uint64(0))
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
