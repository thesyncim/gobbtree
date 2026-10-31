//go:build goexperiment.simd && arm64

package gobbtree

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"simd"
	"simd/archsimd"
	"unsafe"
)

// findWord is findWordBy with each page's windows counted inline, as
// descendText counts: a call per level spills the descent's registers. A
// page's windows are counted over its capacity (diskSlotRange), in tiles
// of eight, two at a time into two counters.
func (m *FileMap[K, V]) findWord(data []byte, root int, word uint64) (keySlot, valueSlot int, ok bool) {
	in, lf := &m.layout.internal, &m.layout.leaf
	if simd.Emulated() || in.capacity < 8 || lf.capacity < 8 || uintptr(unsafe.Pointer(unsafe.SliceData(data)))&1 != 0 {
		return m.findWordBy(data, root, word)
	}
	start := (root &^ 1) + m.layout.area
	if n := int(data[start]); n != 0 {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], word)
		if !bytes.Equal(encoded[:min(n, 8)], data[start+1:start+1+n]) {
			return
		}
	}
	signed := m.layout.key.kind == reflect.Int64
	stored := func(slot int) uint64 {
		w := binary.NativeEndian.Uint64(data[slot:])
		if signed {
			w ^= 1 << 63
		}
		return w
	}
	tagged, left, leftIndex := root, -1, 0
	for depth := 0; ; depth++ {
		if depth > fileMaxDepth {
			panic("gobbtree: corrupt file map: tree too deep")
		}
		node, leaf := tagged&^1, tagged&1 != 0
		n := int(binary.NativeEndian.Uint16(data[node:]))
		capacity := in.capacity
		if leaf {
			capacity = lf.capacity
			if data[node+2] != 0 {
				headSlot := m.headSlot(data, node, left, leftIndex)
				if h := stored(headSlot); h >= word {
					if h != word {
						return headSlot, 0, false
					}
					return headSlot, node + lf.trailing, true
				}
			}
		}
		prefix := int(binary.NativeEndian.Uint16(data[node+4:]))
		var window uint64
		if prefix < 8 {
			window = word << (prefix * 8) >> 48
		}
		// The page's windows below window and equal to it, over its
		// capacity, as diskSlotCount16 counts them. Internal pages and
		// leaves keep them at the same offset.
		slots := data[node+in.inline : node+in.inline+capacity*2]
		s := unsafe.Slice((*uint16)(unsafe.Pointer(&slots[0])), len(slots)/2)
		target := archsimd.BroadcastUint16x8(uint16(window))
		var lo0, lo1, eq0, eq1 archsimd.Int16x8
		whole := len(s) &^ 7
		i := 0
		for ; i+16 <= whole; i += 16 {
			v0 := archsimd.LoadUint16x8(s[i : i+8])
			v1 := archsimd.LoadUint16x8(s[i+8 : i+16])
			lo0 = lo0.Add(v0.Less(target).ToInt16x8())
			lo1 = lo1.Add(v1.Less(target).ToInt16x8())
			eq0 = eq0.Add(v0.Equal(target).ToInt16x8())
			eq1 = eq1.Add(v1.Equal(target).ToInt16x8())
		}
		if i < whole {
			v := archsimd.LoadUint16x8(s[i : i+8])
			lo0 = lo0.Add(v.Less(target).ToInt16x8())
			eq0 = eq0.Add(v.Equal(target).ToInt16x8())
		}
		if whole < len(s) {
			first := len(s) - 8
			v := archsimd.LoadUint16x8(s[first:])
			positions := archsimd.LoadUint16x8(laneIndexes16[:]).Add(archsimd.BroadcastUint16x8(uint16(first)))
			fresh := positions.Greater(archsimd.BroadcastUint16x8(uint16(whole - 1)))
			lo1 = lo1.Add(v.Less(target).And(fresh).ToInt16x8())
			eq1 = eq1.Add(v.Equal(target).And(fresh).ToInt16x8())
		}
		below, equal := -int(lo0.Add(lo1).ReduceSum()), -int(eq0.Add(eq1).ReduceSum())
		lo, hi := min(below, n), min(below+equal, n)
		if !leaf {
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
			continue
		}
		// The keys past the head: index i is entry i+head.
		for lo < hi {
			mid := int(uint(lo+hi) >> 1)
			if stored(node+lf.keys+mid*lf.keyStride) < word {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		if lo >= n {
			return
		}
		keySlot = node + lf.keys + lo*lf.keyStride
		if stored(keySlot) != word {
			return keySlot, 0, false
		}
		return keySlot, node + lf.trailing + (lo+1)*lf.stride, true
	}
}
