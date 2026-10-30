//go:build goexperiment.simd && arm64

package gobbtree

import (
	"simd"
	"simd/archsimd"
	"unsafe"
)

// NEON is mandatory on Go's arm64 targets. Direct Go intrinsics avoid the
// portable specialization wrapper and stack-based horizontal reduction.
// Four independent accumulators shorten the compare/subtract dependency chain.
// Every load is within the fixed, initialized array, including its padding.
func lowerCode(keys *[nodeSize]uint64, n int, key uint64) int {
	if simd.Emulated() {
		return lowerCodeScalar(keys, n, key)
	}
	target := archsimd.BroadcastUint64x2(key)
	a := countEight((*[8]uint64)(keys[0:8]), target)
	b := countEight((*[8]uint64)(keys[8:16]), target)
	c := countEight((*[8]uint64)(keys[16:24]), target)
	d := countEight((*[8]uint64)(keys[24:32]), target)
	sum := a.Add(b).Add(c.Add(d))
	return min(-int(sum.GetElem(0)+sum.GetElem(1)), n)
}

// SIMD reports whether this build uses hardware SIMD and its vector width.
func SIMD() (hardware bool, bits int) { return !simd.Emulated(), simd.VectorBitSize() }

// Four compares reduce in a balanced tree. Fixed slices let the compiler
// emit constant-offset loads without a loop, bounds checks, or index arithmetic.
func countEight(keys *[8]uint64, target archsimd.Uint64x2) archsimd.Int64x2 {
	a := archsimd.LoadUint64x2(keys[0:2]).Less(target).ToInt64x2()
	b := archsimd.LoadUint64x2(keys[2:4]).Less(target).ToInt64x2()
	c := archsimd.LoadUint64x2(keys[4:6]).Less(target).ToInt64x2()
	d := archsimd.LoadUint64x2(keys[6:8]).Less(target).ToInt64x2()
	return a.Add(b).Add(c.Add(d))
}

// lowerSummary counts a text internal node's 4-byte summaries below t, all
// 32 in one 128-byte line, as lowerCode counts codes.
func lowerSummary(sums *[nodeSize]uint32, n int, t uint32) int {
	if simd.Emulated() {
		return lowerSummaryScalar(sums, n, t)
	}
	target := archsimd.BroadcastUint32x4(t)
	a := countEight32((*[8]uint32)(sums[0:8]), target)
	b := countEight32((*[8]uint32)(sums[8:16]), target)
	c := countEight32((*[8]uint32)(sums[16:24]), target)
	d := countEight32((*[8]uint32)(sums[24:32]), target)
	return min(-int(a.Add(b).Add(c.Add(d)).ReduceSum()), n)
}

// descendText descends an owner's tree from h by text key s, with plain
// loads, and searches s's leaf by its code: the node it stopped at, the
// slot and what it found there, a text constant. The summaries and codes
// are counted inline: a call per level spills the descent's registers.
func descendText(h *gheader, s string) (*gheader, int, int) {
	if simd.Emulated() {
		return descendTextBy(h, s)
	}
	for !h.leaf {
		n := (*innerRoute)(unsafe.Pointer(h))
		count := owned(&n.count)
		sum := summaryOf(s, owned(&n.prefix))
		sums := (*[nodeSize]uint32)(unsafe.Pointer(&n.codes))
		target := archsimd.BroadcastUint32x4(sum)
		a := countEight32((*[8]uint32)(sums[0:8]), target)
		b := countEight32((*[8]uint32)(sums[8:16]), target)
		c := countEight32((*[8]uint32)(sums[16:24]), target)
		d := countEight32((*[8]uint32)(sums[24:32]), target)
		lo := min(-int(a.Add(b).Add(c.Add(d)).ReduceSum()), count)
		if lo < count && sums[lo&(nodeSize-1)] == sum {
			return h, 0, textTie
		}
		h = plainPointer(&n.children[max(lo-1, 0)&(nodeSize-1)])
	}
	n := (*leafRoute)(unsafe.Pointer(h))
	count, p := owned(&n.count), owned(&n.prefix)
	// textFits, written out: as a call it spills what the search needs.
	h0, h1 := *(*uint64)(unsafe.Pointer(&n.head[0])), *(*uint64)(unsafe.Pointer(&n.head[1]))
	if !(p <= 16 && len(s) <= p+16 && len(s) >= p && headDiff(stringCode(s, 0), headWindow(s), p, h0, h1) == 0) {
		return h, 0, textSearch
	}
	code := stringCode(s, p)
	target := archsimd.BroadcastUint64x2(code)
	a := countEight((*[8]uint64)(n.codes[0:8]), target)
	b := countEight((*[8]uint64)(n.codes[8:16]), target)
	c := countEight((*[8]uint64)(n.codes[16:24]), target)
	d := countEight((*[8]uint64)(n.codes[24:32]), target)
	sum := a.Add(b).Add(c.Add(d))
	lo := min(-int(sum.GetElem(0)+sum.GetElem(1)), count)
	if lo < count && n.codes[lo&(nodeSize-1)] == code {
		return h, lo, textCode
	}
	return h, lo, textAbsent
}

// descendCode descends an owner's tree from h by code, the code of a key
// that codes order exactly, with plain loads, and searches its leaf: the
// leaf, its slot for code, and whether code is there (seekCode). An
// internal node routes to its last child whose separator is at most
// code, counting codes no greater; its first, 0, always is, and slots past
// its count, codePad, are only for the largest code, which the count caps.
func descendCode(h *gheader, code uint64, place bool) (*gheader, int, bool) {
	if simd.Emulated() {
		return descendCodeBy(h, code, place)
	}
	target := archsimd.BroadcastUint64x2(code)
	for !h.leaf {
		n := (*innerRoute)(unsafe.Pointer(h))
		count := owned(&n.count)
		codes := (*[nodeSize]uint64)(unsafe.Pointer(&n.codes))
		a := countEightAtMost((*[8]uint64)(codes[0:8]), target)
		b := countEightAtMost((*[8]uint64)(codes[8:16]), target)
		c := countEightAtMost((*[8]uint64)(codes[16:24]), target)
		d := countEightAtMost((*[8]uint64)(codes[24:32]), target)
		sum := a.Add(b).Add(c.Add(d))
		i := min(-int(sum.GetElem(0)+sum.GetElem(1)), count) - 1
		h = plainPointer(&n.children[i&(nodeSize-1)])
	}
	i, ok := seekCode((*leafRoute)(unsafe.Pointer(h)), owned(&h.count), code, place)
	return h, i, ok
}

// hintedCode settles code through r's hint in an owner's tree, with plain
// loads: the leaf the hint names, code's slot there and whether code is
// there, settled where the leaf holds it or it lies between the leaf's
// first key and its last. Where place is true, an absent code's slot is
// the one it would take. The cell, the leaf's block and its tags are read
// written out, as hintedCodeBy's calls would: a lookup's calls cost it the
// lookups the core would otherwise overlap with it.
func hintedCode(r *codeHints, d *leafDir, code uint64, place bool) (h *gheader, i int, found, settled bool) {
	if simd.Emulated() {
		return hintedCodeBy(r, d, code, place)
	}
	if r == nil || len(r.cells) == 0 {
		return nil, 0, false, false
	}
	region := (code - r.base) >> r.shift
	var c *hintCell
	switch {
	case r.hashed:
		c = &r.cells[(region*0x9e3779b97f4a7c15>>(64-r.bits))&uint64(len(r.cells)-1)]
	case region < uint64(len(r.cells)):
		c = &r.cells[region]
	default:
		return nil, 0, false, false
	}
	head, tail := c.head, c.tail
	if head <= hintDescend {
		return nil, 0, false, false
	}
	q := (code & (uint64(1)<<r.shift - 1)) << 10 >> r.shift
	if r.shift >= 10 {
		q = code >> (r.shift - 10) & 1023
	}
	id := head & hintIDMask
	if q >= head>>48&1023 {
		id = head >> 24 & hintIDMask
	}
	if q >= head>>58|(tail>>48&15)<<6 {
		id = tail & hintIDMask
	}
	if q >= tail>>52&1023 {
		id = tail >> 24 & hintIDMask
	}
	blocks := plainPointer(&d.blocks)
	if blocks == nil || id/leafBlock >= uint64(len(*blocks)) {
		return nil, 0, false, false
	}
	h = (*gheader)(unsafe.Add((*blocks)[id/leafBlock], uintptr(id%leafBlock)*d.stride))
	n := (*leafRoute)(unsafe.Pointer(h))
	count := owned(&n.count)
	target := archsimd.BroadcastUint8x16(uint8(code))
	ta := archsimd.LoadUint8x16(n.tags[:16]).Equal(target).ToInt8x16().ToBits()
	tb := archsimd.LoadUint8x16(n.tags[16:]).Equal(target).ToInt8x16().ToBits()
	pa := archsimd.LoadUint8x16(tagPositions[:16]).OrNot(ta)
	pb := archsimd.LoadUint8x16(tagPositions[16:]).OrNot(tb)
	i = min(int(pa.Min(pb).ReduceMin()), count)
	if i < count && n.codes[i&(nodeSize-1)] == code {
		return h, i, true, true
	}
	if first, high := bounds(h); !owns(count, first, high, code) {
		return nil, 0, false, false
	}
	if i < count || place {
		// Another code has its tag, or its slot is asked for.
		i = lowerCode(&n.codes, count, code)
		return h, i, i < count && n.codes[i&(nodeSize-1)] == code, true
	}
	return h, i, false, true
}

// descendTextOrdered descends a racing reader's text tree by s, as
// textLeafOrdered does, and searches the leaf by its code, a key that fits
// its windows and lacks its prefix being absent: the leaf, its latch word, the slot, what it found there, and the
// words read of the leaf, for the caller's closing check; false where it
// can't settle. Summaries and codes are counted inline, as descendText
// counts them: a call per level spills the descent's registers.
func descendTextOrdered(root **gheader, s string) (h *gheader, word uint64, lo, state int, acc uint64, ok bool) {
	if simd.Emulated() {
		return descendTextOrderedBy(root, s)
	}
	h = *root
	if h == nil {
		return nil, 0, 0, 0, 0, false
	}
	word = h.latch.Load()
	if *after(root, word) != h {
		return nil, 0, 0, 0, 0, false
	}
	for depth := 0; ; depth++ {
		if word&latchExclusive != 0 || depth == maxDepth {
			return nil, 0, 0, 0, 0, false
		}
		if h.leaf {
			break
		}
		n := (*innerRoute)(unsafe.Pointer(h))
		count := int(*(*int32)(unsafe.Pointer(&n.count)))
		if count < 1 || count > nodeSize {
			return nil, 0, 0, 0, 0, false
		}
		p := int(*(*int32)(unsafe.Pointer(&n.prefix)))
		sum := summaryOf(s, p)
		sums := (*[nodeSize]uint32)(unsafe.Pointer(&n.codes))
		target := archsimd.BroadcastUint32x4(sum)
		ca := countEight32((*[8]uint32)(sums[0:8]), target)
		cb := countEight32((*[8]uint32)(sums[8:16]), target)
		cc := countEight32((*[8]uint32)(sums[16:24]), target)
		cd := countEight32((*[8]uint32)(sums[24:32]), target)
		i := max(min(-int(ca.Add(cb).Add(cc.Add(cd)).ReduceSum()), count)-1, 0)
		// The summaries that bracket the route prove it in the snapshot
		// the closing check vouches for; one equal to key's is a tie.
		below, next := sums[i&(nodeSize-1)], sums[(i+1)&(nodeSize-1)]
		if i > 0 && below >= sum || i+1 < count && next <= sum {
			return nil, 0, 0, 0, 0, false
		}
		a := uint64(i) ^ uint64(count) ^ uint64(p)<<8 ^ uint64(below)<<16 ^ uint64(next)<<32
		child := *after((**gheader)(unsafe.Pointer(&n.children[i&(nodeSize-1)])), a)
		if child == nil {
			return nil, 0, 0, 0, 0, false
		}
		childWord := child.latch.Load()
		if *after((*uint64)(unsafe.Pointer(&h.latch)), a^childWord^uint64(uintptr(unsafe.Pointer(child))))&^latchSharedMask != word&^latchSharedMask {
			return nil, 0, 0, 0, 0, false
		}
		h, word = child, childWord
	}
	n := (*leafRoute)(unsafe.Pointer(h))
	count := int(*(*int32)(unsafe.Pointer(&n.count)))
	p := int(*(*int32)(unsafe.Pointer(&n.prefix)))
	if count < 0 || count > nodeSize || p > 16 {
		return nil, 0, 0, 0, 0, false
	}
	h0, h1 := *(*uint64)(unsafe.Pointer(&n.head[0])), *(*uint64)(unsafe.Pointer(&n.head[1]))
	acc = uint64(count) ^ uint64(p)<<8 ^ h0 ^ h1<<1
	// Key has the leaf's prefix where its first p bytes are the head's.
	if len(s) < p || headDiff(stringCode(s, 0), headWindow(s), p, h0, h1) != 0 {
		return h, word, 0, orderedOutside, acc, true
	}
	if len(s) > p+16 {
		return nil, 0, 0, 0, 0, false
	}
	code := stringCode(s, p)
	target := archsimd.BroadcastUint64x2(code)
	ca := countEight((*[8]uint64)(n.codes[0:8]), target)
	cb := countEight((*[8]uint64)(n.codes[8:16]), target)
	cc := countEight((*[8]uint64)(n.codes[16:24]), target)
	cd := countEight((*[8]uint64)(n.codes[24:32]), target)
	sum := ca.Add(cb).Add(cc.Add(cd))
	lo = min(-int(sum.GetElem(0)+sum.GetElem(1)), count)
	c := n.codes[lo&(nodeSize-1)]
	acc ^= uint64(lo)<<40 ^ c
	if lo < count && c == code {
		return h, word, lo, orderedCode, acc, true
	}
	return h, word, lo, orderedAbsent, acc, true
}

// countEightAtMost is countEight counting keys no greater than target.
func countEightAtMost(keys *[8]uint64, target archsimd.Uint64x2) archsimd.Int64x2 {
	a := archsimd.LoadUint64x2(keys[0:2]).LessEqual(target).ToInt64x2()
	b := archsimd.LoadUint64x2(keys[2:4]).LessEqual(target).ToInt64x2()
	c := archsimd.LoadUint64x2(keys[4:6]).LessEqual(target).ToInt64x2()
	d := archsimd.LoadUint64x2(keys[6:8]).LessEqual(target).ToInt64x2()
	return a.Add(b).Add(c.Add(d))
}

func countEight32(keys *[8]uint32, target archsimd.Uint32x4) archsimd.Int32x4 {
	a := archsimd.LoadUint32x4(keys[0:4]).Less(target).ToInt32x4()
	b := archsimd.LoadUint32x4(keys[4:8]).Less(target).ToInt32x4()
	return a.Add(b)
}

var tagPositions = [nodeSize]uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31}

func firstTag(tags *[nodeSize]uint8, n int, key uint64) int {
	if simd.Emulated() {
		return firstTagScalar(tags, n, uint8(key))
	}
	target := archsimd.BroadcastUint8x16(uint8(key))
	a := archsimd.LoadUint8x16(tags[:16]).Equal(target).ToInt8x16().ToBits()
	b := archsimd.LoadUint8x16(tags[16:]).Equal(target).ToInt8x16().ToBits()
	pa := archsimd.LoadUint8x16(tagPositions[:16]).OrNot(a)
	pb := archsimd.LoadUint8x16(tagPositions[16:]).OrNot(b)
	i := int(pa.Min(pb).ReduceMin())
	return min(i, n)
}
