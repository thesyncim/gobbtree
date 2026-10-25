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
