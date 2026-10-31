//go:build goexperiment.simd && arm64

package gobbtree

import (
	"simd"
	"simd/archsimd"
	"unsafe"
)

// fastWordPages tells layouts findWordFast searches: pages of at least 16
// slots, where the vectors are hardware's.
func fastWordPages(l *diskLayout) bool {
	return l.wordPages && l.internal.capacity >= 16 && l.leaf.capacity >= 16 && !simd.Emulated()
}

// findWordFast is findWordBy's descent, settled without a call, for the
// lookups that need none: each page's windows are counted in the loop, as
// diskCountWindows16 counts them, the shared prefix is compared as a word,
// and every read is by address within a page checked to lie in the map.
// What it can't settle that way, a page out of bounds or a tree too deep,
// it leaves to findWordBy (wordUnsettled), which panics on them. With no call,
// it takes no frame and spills nothing. Internal pages and leaves keep
// their windows at the same offset, so one count serves both.
func (m *FileMap[K, V]) findWordFast(img *fileImage, word uint64) (keySlot, valueSlot, state int) {
	l := &m.layout
	if !l.wordFast {
		return 0, 0, wordUnsettled
	}
	if (word^img.shared)>>img.sharedShift != 0 {
		return 0, 0, wordAbsent
	}
	// What every level reads, once; the leaf's offsets are read at the
	// leaf, which needs them once: kept live through the loop, they would
	// be spilled.
	inline, trailing, stride, inCapacity := l.internal.inline, l.internal.trailing, l.internal.stride, l.internal.capacity
	data := img.data
	base := unsafe.Pointer(unsafe.SliceData(data))
	tagged, size := img.root, l.rootSize
	for depth := 0; ; depth++ {
		node, leaf := tagged&^1, tagged&1 != 0
		capacity, tail := inCapacity, &l.internalTail
		if leaf {
			capacity, tail = l.leaf.capacity, &l.leafTail
			if size != l.rootSize {
				size = l.leaf.size
			}
		}
		if depth > fileMaxDepth || node < 0 || node > len(data)-size {
			return 0, 0, wordUnsettled
		}
		p := unsafe.Add(base, node)
		n := min(int(*(*uint16)(p)), capacity)
		if leaf && *(*uint8)(unsafe.Add(p, 2)) != 0 {
			headAt := l.leaf.headKey
			if head := *(*uint64)(unsafe.Add(p, headAt)) ^ l.flip; head >= word {
				if head != word {
					return node + headAt, 0, wordAbsent
				}
				return node + headAt, node + l.leaf.trailing, wordFound
			}
		}
		key := uint16(windowOf(word, *(*uint16)(unsafe.Add(p, 4))))
		slots := unsafe.Add(p, inline)
		at := func(i int) *[8]uint16 { return (*[8]uint16)(unsafe.Add(slots, 2*i)) }
		target := archsimd.BroadcastUint16x8(key)
		var sum archsimd.Int16x8
		switch {
		case n <= 8:
			// A page of few slots, as a root often is: its slots past n
			// hold the greatest window, which no target is below, so its
			// first tile counts them all.
			sum = archsimd.LoadUint16x8Array(at(0)).Less(target).ToInt16x8()
		case n <= 16:
			sum = archsimd.LoadUint16x8Array(at(0)).Less(target).ToInt16x8().Add(archsimd.LoadUint16x8Array(at(8)).Less(target).ToInt16x8())
		case capacity > 48 && capacity <= 56:
			// Six whole tiles, then the last 8 slots, masked to the
			// lanes past them (diskTailMasks): 49 slots in an internal
			// page, 55 in a uint64 leaf.
			a := archsimd.LoadUint16x8Array(at(0)).Less(target).ToInt16x8()
			b := archsimd.LoadUint16x8Array(at(8)).Less(target).ToInt16x8()
			c := archsimd.LoadUint16x8Array(at(16)).Less(target).ToInt16x8()
			d := archsimd.LoadUint16x8Array(at(24)).Less(target).ToInt16x8()
			e := archsimd.LoadUint16x8Array(at(32)).Less(target).ToInt16x8()
			f := archsimd.LoadUint16x8Array(at(40)).Less(target).ToInt16x8()
			h := archsimd.LoadUint16x8Array(at(capacity - 8)).Less(target).ToInt16x8().And(archsimd.LoadInt16x8Array((*[8]int16)(tail[0:8])))
			sum = a.Add(b).Add(c.Add(d)).Add(e.Add(f).Add(h))
		case capacity >= 48 && capacity <= 64:
			a := archsimd.LoadUint16x8Array(at(0)).Less(target).ToInt16x8()
			b := archsimd.LoadUint16x8Array(at(8)).Less(target).ToInt16x8()
			c := archsimd.LoadUint16x8Array(at(16)).Less(target).ToInt16x8()
			d := archsimd.LoadUint16x8Array(at(24)).Less(target).ToInt16x8()
			e := archsimd.LoadUint16x8Array(at(32)).Less(target).ToInt16x8()
			f := archsimd.LoadUint16x8Array(at(40)).Less(target).ToInt16x8()
			g := archsimd.LoadUint16x8Array(at(capacity - 16)).Less(target).ToInt16x8().And(archsimd.LoadInt16x8Array((*[8]int16)(tail[8:16])))
			h := archsimd.LoadUint16x8Array(at(capacity - 8)).Less(target).ToInt16x8().And(archsimd.LoadInt16x8Array((*[8]int16)(tail[0:8])))
			sum = a.Add(b).Add(c.Add(d)).Add(e.Add(f).Add(g.Add(h)))
		default:
			var lo0, lo1 archsimd.Int16x8
			whole := capacity &^ 7
			i := 0
			for ; i+16 <= whole; i += 16 {
				lo0 = lo0.Add(archsimd.LoadUint16x8Array(at(i)).Less(target).ToInt16x8())
				lo1 = lo1.Add(archsimd.LoadUint16x8Array(at(i + 8)).Less(target).ToInt16x8())
			}
			if i < whole {
				lo0 = lo0.Add(archsimd.LoadUint16x8Array(at(i)).Less(target).ToInt16x8())
			}
			if whole < capacity {
				below := archsimd.LoadUint16x8Array(at(capacity - 8)).Less(target).ToInt16x8()
				lo1 = lo1.Add(below.And(archsimd.LoadInt16x8Array((*[8]int16)(tail[0:8]))))
			}
			sum = lo0.Add(lo1)
		}
		lo := min(-int(sum.ReduceSum()), n)
		tie := lo < n && *(*uint16)(unsafe.Add(slots, 2*lo)) == key
		if !leaf {
			if tie {
				// Separators whose windows tie are settled by their keys,
				// each the head of the leftmost leaf right of it (D49),
				// found down first edges, as separatorSlot finds it. At
				// prefix 0, a uint64 tree's windows are its keys' top 16
				// bits, which the separators just above its leaves share
				// often.
				hi := lo + 1
				for hi < n && *(*uint16)(unsafe.Add(slots, 2*hi)) == key {
					hi++
				}
				for lo < hi {
					mid := int(uint(lo+hi) >> 1)
					down := node + int(*(*uint64)(unsafe.Add(p, trailing+(mid+1)*stride)))
					for d := depth + 1; down&1 == 0; d++ {
						if d > fileMaxDepth || down < 0 || down > len(data)-l.internal.size {
							return 0, 0, wordUnsettled
						}
						down += int(*(*uint64)(unsafe.Add(base, down+trailing)))
					}
					if down &^= 1; down < 0 || down > len(data)-l.leaf.size {
						return 0, 0, wordUnsettled
					}
					if *(*uint64)(unsafe.Add(base, down+l.leaf.headKey))^l.flip > word {
						hi = mid
					} else {
						lo = mid + 1
					}
				}
			}
			tagged = node + int(*(*uint64)(unsafe.Add(p, trailing+lo*stride)))
			size = l.internal.size
			continue
		}
		// The keys past the head: index i is entry i+head. Equal windows
		// are settled by the keys, one after another: a window rarely
		// holds more than one.
		keysAt, keyStride, flip := l.leaf.keys, l.leaf.keyStride, l.flip
		keys := unsafe.Add(p, keysAt)
		for ; tie; tie = lo < n && *(*uint16)(unsafe.Add(slots, 2*lo)) == key {
			k := *(*uint64)(unsafe.Add(keys, lo*keyStride)) ^ flip
			if k == word {
				return node + keysAt + lo*keyStride, node + l.leaf.trailing + (lo+1)*l.leaf.stride, wordFound
			}
			if k > word {
				break
			}
			lo++
		}
		return node + keysAt + lo*keyStride, 0, wordAbsent
	}
}
