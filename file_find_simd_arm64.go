//go:build goexperiment.simd && arm64

package gobbtree

import (
	"reflect"
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

// fastTextPages tells layouts findTextFast searches: string keys, with
// 8-byte windows in leaves of at least 8 slots and 4-byte ones in internal
// pages of at least 8, the leaves keeping a head key, where the vectors
// are hardware's.
func fastTextPages(l *diskLayout) bool {
	return l.key.kind == reflect.String && l.leaf.width == 8 && l.internal.width == 4 &&
		l.leaf.capacity >= 8 && l.internal.capacity >= 8 && l.leaf.headKey >= 0 && !simd.Emulated()
}

// findTextFast is findKey's search for string keys, settled as
// findWordFast settles integer ones: each page checked once to lie in the
// map, its count clamped to its capacity, and read by address within it;
// its windows past its prefix counted in the loop, 4-byte ones in internal
// pages, 8-byte ones in leaves; ties settled by the keys' bytes, which a
// span in the page points to, checked to lie in the map too
// (diskCompareSpan). What it can't settle, it leaves to findKey.
func (m *FileMap[K, V]) findTextFast(img *fileImage, s string) (keySlot, valueSlot, state int) {
	l := &m.layout
	if !l.textFast {
		return 0, 0, wordUnsettled
	}
	// The shared prefix, compared as upstream's zero-padded keys compare:
	// a key that lacks it sorts before or after every key.
	if p := img.sharedText; p != "" {
		if len(s) < len(p) {
			return 0, 0, wordUnsettled
		}
		i := 0
		for ; i+8 <= len(p); i += 8 {
			if loadWord(s, i) != loadWord(p, i) {
				return 0, 0, wordAbsent
			}
		}
		for ; i < len(p); i++ {
			if s[i] != p[i] {
				return 0, 0, wordAbsent
			}
		}
	}
	data := img.data
	base := unsafe.Pointer(unsafe.SliceData(data))
	inline, trailing, stride := l.internal.inline, l.internal.trailing, l.internal.stride
	// atSeparator tells a key equal to the separator left of the edge the
	// path took last that wasn't a first: the head of the leaf it reaches,
	// as upstream's descent tells it (D49). Elsewhere the key is past the
	// head, which the leaf then needn't compare.
	tagged, size, atSeparator := img.root, l.rootSize, false
	for depth := 0; ; depth++ {
		node, leaf := tagged&^1, tagged&1 != 0
		if leaf && size != l.rootSize {
			size = l.leaf.size
		}
		if depth > fileMaxDepth || node < 0 || node > len(data)-size {
			return 0, 0, wordUnsettled
		}
		p := unsafe.Add(base, node)
		prefix := int(*(*uint16)(unsafe.Add(p, 4)))
		code := stringCode(s, prefix)
		if !leaf {
			n := min(int(*(*uint16)(p)), l.internal.capacity)
			// 4-byte windows: 4 lanes a tile.
			slots := unsafe.Add(p, inline)
			at := func(i int) *[4]uint32 { return (*[4]uint32)(unsafe.Add(slots, 4*i)) }
			key := uint32(code >> 32)
			target := archsimd.BroadcastUint32x4(key)
			// Whole tiles over the slots up to n, as far as the page's
			// slots go: those past n hold the greatest window. The rest,
			// fewer than a tile, one by one.
			var lo0, lo1 archsimd.Int32x4
			whole := min((n+3)&^3, l.internal.capacity&^3)
			i := 0
			for ; i+8 <= whole; i += 8 {
				lo0 = lo0.Add(archsimd.LoadUint32x4Array(at(i)).Less(target).ToInt32x4())
				lo1 = lo1.Add(archsimd.LoadUint32x4Array(at(i + 4)).Less(target).ToInt32x4())
			}
			if i < whole {
				lo0 = lo0.Add(archsimd.LoadUint32x4Array(at(i)).Less(target).ToInt32x4())
			}
			lo := -int(lo0.Add(lo1).ReduceSum())
			for j := whole; j < n; j++ {
				if *(*uint32)(unsafe.Add(slots, 4*j)) < key {
					lo++
				}
			}
			lo = min(lo, n)
			equal := false
			if lo < n && *(*uint32)(unsafe.Add(slots, 4*lo)) == key {
				// Separators whose windows tie are settled by their keys,
				// each the head of the leftmost leaf right of it (D49).
				hi := lo + 1
				for hi < n && *(*uint32)(unsafe.Add(slots, 4*hi)) == key {
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
					order, ok := diskCompareSpan(data, down+l.leaf.headKey, s)
					if !ok {
						return 0, 0, wordUnsettled
					}
					if order > 0 {
						hi = mid
					} else {
						// Equal, it is the last at most the key: lo ends
						// past it.
						lo, equal = mid+1, order == 0
					}
				}
			}
			if lo > 0 {
				atSeparator = equal
			}
			tagged = node + int(*(*uint64)(unsafe.Add(p, trailing+lo*stride)))
			size = l.internal.size
			continue
		}
		lf := &l.leaf
		n := min(int(*(*uint16)(p)), lf.capacity)
		if atSeparator && *(*uint8)(unsafe.Add(p, 2)) != 0 {
			return node + lf.headKey, node + lf.trailing, wordFound
		}
		// 8-byte windows: 2 lanes a tile.
		slots := unsafe.Add(p, lf.inline)
		at := func(i int) *[2]uint64 { return (*[2]uint64)(unsafe.Add(slots, 8*i)) }
		target := archsimd.BroadcastUint64x2(code)
		var lo0, lo1 archsimd.Int64x2
		whole := min((n+1)&^1, lf.capacity&^1)
		i := 0
		for ; i+4 <= whole; i += 4 {
			lo0 = lo0.Add(archsimd.LoadUint64x2Array(at(i)).Less(target).ToInt64x2())
			lo1 = lo1.Add(archsimd.LoadUint64x2Array(at(i + 2)).Less(target).ToInt64x2())
		}
		if i < whole {
			lo0 = lo0.Add(archsimd.LoadUint64x2Array(at(i)).Less(target).ToInt64x2())
		}
		sum := lo0.Add(lo1)
		lo := -int(sum.GetElem(0) + sum.GetElem(1))
		if whole < n && *(*uint64)(unsafe.Add(slots, 8*whole)) < code {
			lo++
		}
		lo = min(lo, n)
		// The keys whose windows tie, settled by their bytes in turn.
		for lo < n && *(*uint64)(unsafe.Add(slots, 8*lo)) == code {
			order, ok := diskCompareSpan(data, node+lf.keys+lo*lf.keyStride, s)
			if !ok {
				return 0, 0, wordUnsettled
			}
			if order == 0 {
				return node + lf.keys + lo*lf.keyStride, node + lf.trailing + (lo+1)*lf.stride, wordFound
			}
			if order > 0 {
				break
			}
			lo++
		}
		return node + lf.keys + lo*lf.keyStride, 0, wordAbsent
	}
}
