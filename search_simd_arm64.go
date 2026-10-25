//go:build goexperiment.simd && arm64

package gobbtree

import (
	"simd"
	"simd/archsimd"
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

// A 32-bit summary doubles SIMD density. Full codes resolve summary ties.
func lowerCompact(keys *[nodeSize]uint64, high *[nodeSize]uint32, n int, key uint64) int {
	if simd.Emulated() {
		return lowerCodeScalar(keys, n, key)
	}
	target := archsimd.BroadcastUint32x4(uint32(key >> 32))
	a := countEight32((*[8]uint32)(high[0:8]), target)
	b := countEight32((*[8]uint32)(high[8:16]), target)
	c := countEight32((*[8]uint32)(high[16:24]), target)
	d := countEight32((*[8]uint32)(high[24:32]), target)
	i := min(-int(a.Add(b).Add(c.Add(d)).ReduceSum()), n)
	if i < n && high[i] == uint32(key>>32) && keys[i] < key {
		return lowerCodeScalar(keys, n, key)
	}
	return i
}
func countEight32(keys *[8]uint32, target archsimd.Uint32x4) archsimd.Int32x4 {
	a := archsimd.LoadUint32x4(keys[0:4]).Less(target).ToInt32x4()
	b := archsimd.LoadUint32x4(keys[4:8]).Less(target).ToInt32x4()
	return a.Add(b)
}

var tagPositions = [nodeSize]uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31}

// Byte tags reject most misses in two vectors. Verify the full key and
// use the ordered search when tags collide. Padding never returns a live slot.
func findCode(keys *[nodeSize]uint64, tags *[nodeSize]uint8, n int, key uint64) int {
	if simd.Emulated() {
		return lowerCodeScalar(keys, n, key)
	}
	target := archsimd.BroadcastUint8x16(uint8(key))
	a := archsimd.LoadUint8x16(tags[:16]).Equal(target).ToInt8x16().ToBits()
	b := archsimd.LoadUint8x16(tags[16:]).Equal(target).ToInt8x16().ToBits()
	pa := archsimd.LoadUint8x16(tagPositions[:16]).OrNot(a)
	pb := archsimd.LoadUint8x16(tagPositions[16:]).OrNot(b)
	i := int(pa.Min(pb).ReduceMin())
	if i >= n {
		return n
	}
	if keys[i] == key {
		return i
	}
	return lowerCode(keys, n, key)
}
