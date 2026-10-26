//go:build goexperiment.simd && amd64

package gobbtree

import (
	"math/bits"
	"simd"
	"simd/archsimd"
)

func upperWordInline(values *[nodeSize]uint16, n int, key uint16) int {
	if simd.Emulated() {
		return upperWordInlineScalar(values, n, key)
	}
	if archsimd.X86.AVX512() && amd64MaxWidth >= 512 {
		target := archsimd.BroadcastUint16x32(key)
		count := bits.OnesCount32(target.Less(archsimd.LoadUint16x32(values[:])).ToBits())
		archsimd.ClearAVXUpperBits()
		return min(nodeSize-count, n)
	}
	if archsimd.X86.AVX2() && amd64MaxWidth >= 256 {
		target := archsimd.BroadcastUint16x16(key)
		a := mask16Count256(target.Less(archsimd.LoadUint16x16(values[:16])))
		b := mask16Count256(target.Less(archsimd.LoadUint16x16(values[16:])))
		archsimd.ClearAVXUpperBits()
		return min(nodeSize-a-b, n)
	}
	if archsimd.X86.AVX() {
		target := archsimd.BroadcastUint16x8(key)
		a := mask16Count128(target.Less(archsimd.LoadUint16x8(values[:8])))
		b := mask16Count128(target.Less(archsimd.LoadUint16x8(values[8:16])))
		c := mask16Count128(target.Less(archsimd.LoadUint16x8(values[16:24])))
		d := mask16Count128(target.Less(archsimd.LoadUint16x8(values[24:])))
		archsimd.ClearAVXUpperBits()
		return min(nodeSize-a-b-c-d, n)
	}
	return upperWordInlineScalar(values, n, key)
}
