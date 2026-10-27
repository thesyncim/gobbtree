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
		index := (key - c.base) >> c.shift
		if index >= uint64(len(c.cells)) {
			return 0, false, false
		}
		cell := &c.cells[index]
		head := *(*uint64)(unsafe.Pointer(&cell.head))
		acc ^= head
		if head <= 2 {
			if head == 0 {
				return 0, false, false
			}
		} else if first, second := head&leafCacheIDMask, head>>24&leafCacheIDMask; first == second {
			id = uint32(first) | wordLeafID
		} else {
			tail := *after((*uint64)(unsafe.Pointer(&cell.tail)), head)
			acc ^= tail
			cut0 := head >> 48 & 1023
			cut1 := head>>58 | ((tail >> 48 & 15) << 6)
			cut2 := tail >> 52 & 1023
			q := c.fraction(key)
			if c.shift > 10 && (q == cut0 || q == cut1 || q == cut2) {
				id = 0
			} else {
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
				id = uint32(leaf) | wordLeafID
			}
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
