//go:build goexperiment.simd && arm64

package gobbtree

import (
	"simd"
	"simd/archsimd"
)

func upperWordInline(values *[nodeSize]uint16, n int, key uint16) int {
	if simd.Emulated() {
		return upperWordInlineScalar(values, n, key)
	}
	target := archsimd.BroadcastUint16x8(key)
	a := target.Less(archsimd.LoadUint16x8(values[0:8])).ToInt16x8()
	b := target.Less(archsimd.LoadUint16x8(values[8:16])).ToInt16x8()
	c := target.Less(archsimd.LoadUint16x8(values[16:24])).ToInt16x8()
	d := target.Less(archsimd.LoadUint16x8(values[24:32])).ToInt16x8()
	return min(nodeSize+int(a.Add(b).Add(c.Add(d)).ReduceSum()), n)
}
