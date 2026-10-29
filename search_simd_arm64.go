//go:build goexperiment.simd && arm64

package gobbtree

import (
	"simd"
	"simd/archsimd"
	"sync/atomic"
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

// Unused leaf slots hold MaxUint64, so counting keys below key is the lower
// bound. Two probes select an aligned run of eight entries; a cold leaf then
// loads about three cache lines instead of all eight. Each vector holds one
// key and its value; only the key lanes are summed.
func lowerEntries(entries *[nodeSize]wordEntry, key uint64) int {
	if simd.Emulated() {
		return lowerEntriesScalar(entries, key)
	}
	base := 0
	if entries[nodeSize/2-1].key < key {
		base = nodeSize / 2
	}
	if entries[base+7].key < key {
		base += 8
	}
	words := (*[16]uint64)(unsafe.Pointer(&entries[base&(nodeSize-8)]))
	target := archsimd.BroadcastUint64x2(key)
	a := countEight((*[8]uint64)(words[0:8]), target)
	b := countEight((*[8]uint64)(words[8:16]), target)
	return base - int(a.Add(b).GetElem(0))
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
// loads, to its leaf; or it stops at the internal node where s's summary
// ties a separator's, for the separators to settle. The summaries are
// counted inline: a call per level spills the descent's registers.
func descendText(h *gheader, s string) (leaf, tie *gheader) {
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
			return nil, h
		}
		h = plainPointer(&n.children[max(lo-1, 0)&(nodeSize-1)])
	}
	return h, nil
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

// Each source word is atomic. Constructing vectors in registers avoids a
// stack snapshot and does not issue vector loads against mutable shared data.
func sharedTagVectors(words *[4]uint64) (archsimd.Uint8x16, archsimd.Uint8x16) {
	var a, b archsimd.Uint64x2
	a = a.SetElem(0, atomic.LoadUint64(&words[0])).SetElem(1, atomic.LoadUint64(&words[1]))
	b = b.SetElem(0, atomic.LoadUint64(&words[2])).SetElem(1, atomic.LoadUint64(&words[3]))
	return a.ReshapeToUint8s(), b.ReshapeToUint8s()
}
func firstSharedTag(words *[4]uint64, n int, key uint64) int {
	if simd.Emulated() {
		return firstSharedTagFallback(words, n, key)
	}
	a, b := sharedTagVectors(words)
	target := archsimd.BroadcastUint8x16(uint8(key))
	am := a.Equal(target).ToInt8x16().ToBits()
	bm := b.Equal(target).ToInt8x16().ToBits()
	pa := archsimd.LoadUint8x16(tagPositions[:16]).OrNot(am)
	pb := archsimd.LoadUint8x16(tagPositions[16:]).OrNot(bm)
	return min(int(pa.Min(pb).ReduceMin()), n)
}
