//go:build goexperiment.simd && !arm64

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

func lowerCompact(keys *[nodeSize]uint64, high *[nodeSize]uint32, n int, key uint64) int {
	if simd.Emulated() {
		return lowerCodeScalar(keys, n, key)
	}
	target := simd.BroadcastUint32s(uint32(key >> 32))
	sum := simd.BroadcastInt32s(0)
	for i := 0; i < nodeSize; i += target.Len() {
		sum = sum.Sub(simd.LoadUint32s(high[i:]).Less(target).ToInt32s())
	}
	var lanes [16]int32
	sum.Store(lanes[:])
	i := 0
	for j := 0; j < sum.Len(); j++ {
		i += int(lanes[j])
	}
	i = min(i, n)
	if i < n && high[i] == uint32(key>>32) && keys[i] < key {
		return lowerCodeScalar(keys, n, key)
	}
	return i
}

func findCode(keys *[nodeSize]uint64, tags *[nodeSize]uint8, n int, key uint64) int {
	return lowerCode(keys, n, key)
}
