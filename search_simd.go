//go:build goexperiment.simd

package gobbtree

import "simd"

// All slots are initialized; unused slots contain MaxUint64. A strict-less
// count excludes padding even when the query is MaxUint64. Fixed capacity
// is a multiple of every Go 1.27 hardware vector width (128/256/512 bits).
func lowerCode(keys *[nodeSize]uint64, n int, key uint64) int {
	if simd.Emulated() {
		return lowerCodeScalar(keys, n, key)
	}
	target := simd.BroadcastUint64s(key)
	sum := simd.BroadcastInt64s(0)
	for i := 0; i < nodeSize; i += target.Len() {
		sum = sum.Sub(simd.LoadUint64s(keys[i:]).Less(target).ToInt64s())
	}
	var lanes [8]int64
	sum.Store(lanes[:])
	count := 0
	for i := 0; i < sum.Len(); i++ {
		count += int(lanes[i])
	}
	return min(count, n)
}

// SIMD reports whether this build uses hardware SIMD and its vector width.
func SIMD() (hardware bool, bits int) { return !simd.Emulated(), simd.VectorBitSize() }
