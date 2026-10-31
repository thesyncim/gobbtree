package gobbtree

import (
	"bytes"
	"encoding/binary"
	"reflect"
)

// findWordBy is find for keys of 8 bytes with inline windows, int64 and
// uint64, by word, the big-endian word their encoding is: one loop down the
// levels, whose windows it takes from word, and whose separators and keys
// it compares as words. findWord is it, with the counts inlined where the
// target has vectors.
func (m *FileMap[K, V]) findWordBy(data []byte, root int, word uint64) (keySlot, valueSlot int, ok bool) {
	in, lf := &m.layout.internal, &m.layout.leaf
	// The shared prefix: every key begins with its bytes.
	start := (root &^ 1) + m.layout.area
	if n := int(data[start]); n != 0 {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], word)
		if !bytes.Equal(encoded[:min(n, 8)], data[start+1:start+1+n]) {
			return
		}
	}
	signed := m.layout.key.kind == reflect.Int64
	// stored is the word of the key in slot.
	stored := func(slot int) uint64 {
		w := binary.NativeEndian.Uint64(data[slot:])
		if signed {
			w ^= 1 << 63
		}
		return w
	}
	tagged, left, leftIndex := root, -1, 0
	for depth := 0; tagged&1 == 0; depth++ {
		if depth == fileMaxDepth {
			panic("gobbtree: corrupt file map: tree too deep")
		}
		node := tagged
		count := int(binary.NativeEndian.Uint16(data[node:]))
		prefix := int(binary.NativeEndian.Uint16(data[node+4:]))
		var window uint64
		if prefix < 8 {
			window = word << (prefix * 8) >> 48
		}
		lo, hi := diskSlotRange(data[node+in.inline:node+in.inline+in.capacity*2], 2, count, window)
		for lo < hi {
			mid := int(uint(lo+hi) >> 1)
			if stored(m.separatorSlot(data, node, mid)) > word {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		if lo > 0 {
			left, leftIndex = node, lo-1
		}
		tagged = node + int(binary.NativeEndian.Uint64(data[node+in.trailing+lo*in.stride:]))
	}
	leaf := tagged &^ 1
	head := int(data[leaf+2])
	n := int(binary.NativeEndian.Uint16(data[leaf:])) + head
	headSlot := -1
	if head != 0 {
		headSlot = m.headSlot(data, leaf, left, leftIndex)
		if h := stored(headSlot); h >= word {
			if h != word {
				return headSlot, 0, false
			}
			return headSlot, leaf + lf.trailing, true
		}
	}
	prefix := int(binary.NativeEndian.Uint16(data[leaf+4:]))
	var window uint64
	if prefix < 8 {
		window = word << (prefix * 8) >> 48
	}
	lo, hi := diskSlotRange(data[leaf+lf.inline:leaf+lf.inline+lf.capacity*2], 2, n-head, window)
	// The keys past the head: index i is entry i+head.
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if stored(leaf+lf.keys+mid*lf.keyStride) < word {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= n-head {
		return
	}
	keySlot = leaf + lf.keys + lo*lf.keyStride
	if stored(keySlot) != word {
		return keySlot, 0, false
	}
	return keySlot, leaf + lf.trailing + (lo+1)*lf.stride, true
}
