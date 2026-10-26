package gobbtree

import "sync/atomic"

// Stage independent leaf loads before consuming their results. Cached routes
// provide exact leaf IDs; cold routes and invalidated lanes use the ordinary
// optimistic lookup. Results retain GetMany's per-entry consistency.
func (m *wordMap) getMany8(keys *[batchLanes]uint64, dst *[batchLanes]Result[uint64]) {
	version := m.version.Load()
	view := m.view.Load()
	if version&1 != 0 || view == nil || !view.ready || view.leaves == nil {
		for j := 0; j < batchLanes; j++ {
			dst[j].Value, dst[j].OK = m.get(keys[j])
		}
		return
	}
	var nodes [batchLanes]*wordNode
	var sequences [batchLanes]uint64
	var positions [batchLanes]int
	for j := 0; j < batchLanes; j++ {
		id, _ := view.leaves.lookup(keys[j])
		if id == 0 {
			continue
		}
		index := (id &^ wordLeafID) - 1
		blockIndex := int(index >> 8)
		if blockIndex >= len(view.blocks) {
			continue
		}
		block := view.blocks[blockIndex].Load()
		if block == nil || blockIndex < 8 && int(index&255) >= wordSlabSize(blockIndex) {
			continue
		}
		nodes[j] = wordSlabNode(block, index)
	}
	for j := 0; j < batchLanes; j++ {
		if n := nodes[j]; n != nil {
			sequences[j] = n.sequence.Load()
			if sequences[j]&1 != 0 {
				nodes[j] = nil
			}
		}
	}
	for j := 0; j < batchLanes; j++ {
		if n := nodes[j]; n != nil {
			positions[j] = firstSharedTag(n.tagWords(), int(sequences[j]>>1&63), keys[j])
		}
	}
	for j := 0; j < batchLanes; j++ {
		dst[j] = Result[uint64]{}
		n := nodes[j]
		if n == nil {
			continue
		}
		i, size := positions[j], int(sequences[j]>>1&63)
		if i < size {
			key := atomic.LoadUint64(&n.entries()[i].key)
			if key < keys[j] {
				lo, hi := 0, size
				for lo < hi {
					mid := int(uint(lo+hi) >> 1)
					if atomic.LoadUint64(&n.entries()[mid].key) < keys[j] {
						lo = mid + 1
					} else {
						hi = mid
					}
				}
				i = lo
				if i < size {
					key = atomic.LoadUint64(&n.entries()[i].key)
				}
			}
			if i < size && key == keys[j] {
				dst[j] = Result[uint64]{atomic.LoadUint64(&n.entries()[i].value), true}
			}
		}
		if n.sequence.Load() != sequences[j] {
			nodes[j] = nil
		}
	}
	valid := m.version.Load() == version
	for j := 0; j < batchLanes; j++ {
		if !valid || nodes[j] == nil {
			dst[j].Value, dst[j].OK = m.get(keys[j])
		}
	}
}
