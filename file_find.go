package gobbtree

import (
	"bytes"
	"encoding/binary"
	"math/bits"
	"reflect"
	"unsafe"
)

// What findWordFast settled.
const (
	wordAbsent = iota
	wordFound
	wordUnsettled
)

// findWord is find for keys of 8 bytes with inline windows, int64 and
// uint64, by word, the big-endian word their encoding is: findWordFast's
// descent where it settles the lookup, findWordBy's otherwise.
func (m *FileMap[K, V]) findWord(img *fileImage, word uint64) (keySlot, valueSlot int, ok bool) {
	keySlot, valueSlot, state := m.findWordFast(img, word)
	if state != wordUnsettled {
		return keySlot, valueSlot, state == wordFound
	}
	return m.findWordBy(img.data, img.root, word)
}

// findWordBy is findWord's every lookup: one loop down the levels, whose
// windows it takes from word, and whose separators and keys it compares as
// words.
//
// It checks each page it reaches lies in the map, and clamps the page's
// count to its capacity: every read the layout places within the page is
// then in bounds, and reads it by its address, as upstream's node code
// reads a page, without a check for each. A corrupt map may give wrong
// answers, or panic, but is never read out of bounds.
func (m *FileMap[K, V]) findWordBy(data []byte, root int, word uint64) (keySlot, valueSlot int, ok bool) {
	l := &m.layout
	in, lf := &l.internal, &l.leaf
	// The shared prefix: every key begins with its bytes.
	start := (root &^ 1) + l.area
	if n := int(data[start]); n != 0 {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], word)
		if !bytes.Equal(encoded[:min(n, 8)], data[start+1:start+1+n]) {
			return
		}
	}
	// A stored key's word: its bits, a signed key's sign bit flipped.
	flip := uint64(0)
	if l.key.kind == reflect.Int64 {
		flip = 1 << 63
	}
	base := unsafe.Pointer(unsafe.SliceData(data))
	tagged, size := root, l.rootSize
	for depth := 0; tagged&1 == 0; depth++ {
		node := tagged
		if depth == fileMaxDepth || node < 0 || node > len(data)-size {
			panic("gobbtree: corrupt file map: page out of bounds")
		}
		p := unsafe.Add(base, node)
		n := min(int(*(*uint16)(p)), in.capacity)
		below, equal := diskCountWindows16(unsafe.Add(p, in.inline), in.capacity, &l.internalTail, windowOf(word, *(*uint16)(unsafe.Add(p, 4))))
		lo, hi := min(below, n), min(below+equal, n)
		for lo < hi {
			mid := int(uint(lo+hi) >> 1)
			if binary.NativeEndian.Uint64(data[m.separatorSlot(data, node, mid):])^flip > word {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		tagged = node + int(*(*uint64)(unsafe.Add(p, in.trailing+lo*in.stride)))
		size = in.size
	}
	leaf := tagged &^ 1
	if size != l.rootSize {
		size = lf.size
	}
	if leaf < 0 || leaf > len(data)-size {
		panic("gobbtree: corrupt file map: page out of bounds")
	}
	p := unsafe.Add(base, leaf)
	n := min(int(*(*uint16)(p)), lf.capacity)
	// The head entry, the leaf's first, keeps its key in the leaf, as every
	// leaf with inline windows does (D49).
	if *(*uint8)(unsafe.Add(p, 2)) != 0 {
		if head := *(*uint64)(unsafe.Add(p, lf.headKey)) ^ flip; head >= word {
			if head != word {
				return leaf + lf.headKey, 0, false
			}
			return leaf + lf.headKey, leaf + lf.trailing, true
		}
	}
	below, equal := diskCountWindows16(unsafe.Add(p, lf.inline), lf.capacity, &l.leafTail, windowOf(word, *(*uint16)(unsafe.Add(p, 4))))
	// The keys past the head: index i is entry i+head.
	lo, hi := min(below, n), min(below+equal, n)
	keys := unsafe.Add(p, lf.keys)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if *(*uint64)(unsafe.Add(keys, mid*lf.keyStride))^flip < word {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= n {
		return
	}
	keySlot = leaf + lf.keys + lo*lf.keyStride
	if *(*uint64)(unsafe.Add(keys, lo*lf.keyStride))^flip != word {
		return keySlot, 0, false
	}
	return keySlot, leaf + lf.trailing + (lo+1)*lf.stride, true
}

// windowOf is word's 2-byte window past a page's prefix: zero for a prefix
// that takes all its bytes.
func windowOf(word uint64, prefix uint16) uint64 {
	if prefix >= 8 {
		return 0
	}
	return word << (prefix * 8) >> 48
}

// diskCountWindowsBy is diskCountWindows16 one slot at a time.
func diskCountWindowsBy(p unsafe.Pointer, slots int, key uint64) (below, equal int) {
	target := uint16(key)
	for i := range slots {
		v := *(*uint16)(unsafe.Add(p, 2*i))
		if v < target {
			below++
		}
		if v == target {
			equal++
		}
	}
	return below, equal
}

// diskTailMasks are the masks of a count's last two tiles for slots slots,
// the last 8 slots, then the 8 before: all ones in the lanes past the whole
// tiles of 8 that the count runs before them, which are slots&^7 of them
// where it runs a loop, and 48 where it runs its default pages' sequence of
// six (diskCountWindows16, findWord).
func diskTailMasks(slots int) (mask [16]int16) {
	whole := slots &^ 7
	if slots >= 48 && slots <= 64 {
		whole = 48
	}
	for lane := range 8 {
		if slots-8+lane >= whole {
			mask[lane] = -1
		}
		// The tile before the last only where it isn't the last one.
		if at := slots - 16 + lane; at >= whole && at < slots-8 {
			mask[8+lane] = -1
		}
	}
	return mask
}

// findTextBy is findTextFast's search where the target has no vectors to
// count windows with in the loop, as descendTextBy is descendText's: the
// same descent, each page checked once and read by address, its windows
// counted by diskSlotRange's kernels. It settles every lookup, and panics
// on a corrupt map as findKey does.
func (m *FileMap[K, V]) findTextBy(img *fileImage, s string) (keySlot, valueSlot int, ok bool) {
	l := &m.layout
	if p := img.sharedText; p != "" {
		if len(s) < len(p) {
			return m.findKey(img, *(*K)(unsafe.Pointer(&s)))
		}
		if s[:len(p)] != p {
			return
		}
	}
	data := img.data
	base := unsafe.Pointer(unsafe.SliceData(data))
	in, lf := &l.internal, &l.leaf
	tagged, size, atSeparator := img.root, l.rootSize, false
	for depth := 0; ; depth++ {
		node, leaf := tagged&^1, tagged&1 != 0
		if leaf && size != l.rootSize {
			size = lf.size
		}
		if depth > fileMaxDepth || node < 0 || node > len(data)-size {
			panic("gobbtree: corrupt file map: page out of bounds")
		}
		p := unsafe.Add(base, node)
		prefix := int(*(*uint16)(unsafe.Add(p, 4)))
		code := stringCode(s, prefix)
		if !leaf {
			n := min(int(*(*uint16)(p)), in.capacity)
			slots := unsafe.Slice((*byte)(unsafe.Add(p, in.inline)), in.capacity*4)
			lo, hi := diskSlotRange(slots, 4, n, code>>32)
			equal := false
			for lo < hi {
				mid := int(uint(lo+hi) >> 1)
				// The keys under the page share its prefix, and those that
				// tie its window, past it: they compare from there.
				order, ok := diskCompareSpan(data, m.separatorSlot(data, node, mid), s, prefix+4)
				if !ok {
					panic("gobbtree: corrupt file map: key out of bounds")
				}
				if order > 0 {
					hi = mid
				} else {
					lo, equal = mid+1, order == 0
				}
			}
			if lo > 0 {
				atSeparator = equal
			}
			tagged = node + int(*(*uint64)(unsafe.Add(p, in.trailing+lo*in.stride)))
			size = in.size
			continue
		}
		n := min(int(*(*uint16)(p)), lf.capacity)
		if atSeparator && *(*uint8)(unsafe.Add(p, 2)) != 0 {
			return node + lf.headKey, node + lf.trailing, true
		}
		slots := unsafe.Slice((*byte)(unsafe.Add(p, lf.inline)), lf.capacity*8)
		lo, hi := diskSlotRange(slots, 8, n, code)
		for ; lo < hi; lo++ {
			order, ok := diskCompareSpan(data, node+lf.keys+lo*lf.keyStride, s, prefix+8)
			if !ok {
				panic("gobbtree: corrupt file map: key out of bounds")
			}
			if order == 0 {
				return node + lf.keys + lo*lf.keyStride, node + lf.trailing + (lo+1)*lf.stride, true
			}
			if order > 0 {
				break
			}
		}
		return node + lf.keys + lo*lf.keyStride, 0, false
	}
}

// diskCompareSpan compares the bytes that the span in slot points to with
// s, as strings.Compare does, eight at a time, from byte from on: those
// before it the caller knows equal, as upstream's byte strings compare from
// where they can differ (cmp_from). false where the span, or the slot,
// leaves the map.
func diskCompareSpan(data []byte, slot int, s string, from int) (int, bool) {
	if slot < 0 || slot > len(data)-8 {
		return 0, false
	}
	base := unsafe.Pointer(unsafe.SliceData(data))
	n := int(*(*uint32)(unsafe.Add(base, slot)))
	start := slot + int(*(*uint32)(unsafe.Add(base, slot+4)))
	if start < 0 || start > len(data)-n {
		return 0, false
	}
	p := unsafe.Add(base, start)
	// Whole words from the one from lies in: a byte at a time costs more
	// than a word's bytes the caller knew equal.
	k := min(n, len(s))
	i := min(from, k) &^ 7
	for ; i+8 <= k; i += 8 {
		a, b := bits.ReverseBytes64(*(*uint64)(unsafe.Add(p, i))), loadWord(s, i)
		if a != b {
			if a < b {
				return -1, true
			}
			return 1, true
		}
	}
	for ; i < k; i++ {
		if a, b := *(*byte)(unsafe.Add(p, i)), s[i]; a != b {
			if a < b {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case n < len(s):
		return -1, true
	case n > len(s):
		return 1, true
	}
	return 0, true
}
