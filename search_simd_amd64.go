//go:build goexperiment.simd && amd64

package gobbtree

import (
	"math/bits"
	"os"
	"simd"
	"simd/archsimd"
	"strings"
)

// Direct integer kernels need fewer optional CPU features than portable
// simd's full API. Use available integer widths by default, while honoring
// an explicit portable width request as an upper bound.
var amd64MaxWidth = func() int {
	for option := range strings.SplitSeq(os.Getenv("GODEBUG"), ",") {
		if strings.HasPrefix(option, "simd=") {
			return simd.VectorBitSize()
		}
	}
	return 512
}()

// Every load stays within the padded 32-slot array. Sorted comparison masks
// form a prefix of ones, so its first zero is the lower bound.
func lowerCode(keys *[nodeSize]uint64, n int, key uint64) int {
	if simd.Emulated() {
		return lowerCodeScalar(keys, n, key)
	}
	if archsimd.X86.AVX512() && amd64MaxWidth >= 512 {
		target := archsimd.BroadcastUint64x8(key)
		a := uint32(archsimd.LoadUint64x8(keys[0:8]).Less(target).ToBits())
		b := uint32(archsimd.LoadUint64x8(keys[8:16]).Less(target).ToBits())
		c := uint32(archsimd.LoadUint64x8(keys[16:24]).Less(target).ToBits())
		d := uint32(archsimd.LoadUint64x8(keys[24:32]).Less(target).ToBits())
		mask := a | b<<8 | c<<16 | d<<24
		archsimd.ClearAVXUpperBits()
		return min(bits.TrailingZeros32(^mask), n)
	}
	if archsimd.X86.AVX2() && amd64MaxWidth >= 256 {
		// Bias unsigned order once; AVX2 has signed packed qword comparisons.
		target := archsimd.BroadcastInt64x4(int64(key ^ (1 << 63)))
		sign := archsimd.BroadcastInt64x4(-1 << 63)
		a := uint32(target.Greater(archsimd.LoadUint64x4(keys[0:4]).AsInt64x4().Xor(sign)).ToBits())
		b := uint32(target.Greater(archsimd.LoadUint64x4(keys[4:8]).AsInt64x4().Xor(sign)).ToBits())
		c := uint32(target.Greater(archsimd.LoadUint64x4(keys[8:12]).AsInt64x4().Xor(sign)).ToBits())
		d := uint32(target.Greater(archsimd.LoadUint64x4(keys[12:16]).AsInt64x4().Xor(sign)).ToBits())
		lo := a | b<<4 | c<<8 | d<<12
		e := uint32(target.Greater(archsimd.LoadUint64x4(keys[16:20]).AsInt64x4().Xor(sign)).ToBits())
		f := uint32(target.Greater(archsimd.LoadUint64x4(keys[20:24]).AsInt64x4().Xor(sign)).ToBits())
		g := uint32(target.Greater(archsimd.LoadUint64x4(keys[24:28]).AsInt64x4().Xor(sign)).ToBits())
		h := uint32(target.Greater(archsimd.LoadUint64x4(keys[28:32]).AsInt64x4().Xor(sign)).ToBits())
		mask := lo | e<<16 | f<<20 | g<<24 | h<<28
		archsimd.ClearAVXUpperBits()
		return min(bits.TrailingZeros32(^mask), n)
	}
	return lowerCodePortable(keys, n, key)
}

// lowerSummary counts a text internal node's 4-byte summaries below t.
func lowerSummary(sums *[nodeSize]uint32, n int, t uint32) int {
	if simd.Emulated() {
		return lowerSummaryScalar(sums, n, t)
	}
	var mask uint32
	if archsimd.X86.AVX512() && amd64MaxWidth >= 512 {
		target := archsimd.BroadcastUint32x16(t)
		a := uint32(archsimd.LoadUint32x16(sums[:16]).Less(target).ToBits())
		b := uint32(archsimd.LoadUint32x16(sums[16:]).Less(target).ToBits())
		mask = a | b<<16
	} else if archsimd.X86.AVX2() && amd64MaxWidth >= 256 {
		target := archsimd.BroadcastInt32x8(int32(t ^ (1 << 31)))
		sign := archsimd.BroadcastInt32x8(-1 << 31)
		a := uint32(target.Greater(archsimd.LoadUint32x8(sums[0:8]).AsInt32x8().Xor(sign)).ToBits())
		b := uint32(target.Greater(archsimd.LoadUint32x8(sums[8:16]).AsInt32x8().Xor(sign)).ToBits())
		c := uint32(target.Greater(archsimd.LoadUint32x8(sums[16:24]).AsInt32x8().Xor(sign)).ToBits())
		d := uint32(target.Greater(archsimd.LoadUint32x8(sums[24:32]).AsInt32x8().Xor(sign)).ToBits())
		mask = a | b<<8 | c<<16 | d<<24
	} else {
		return lowerSummaryPortable(sums, n, t)
	}
	archsimd.ClearAVXUpperBits()
	return min(bits.TrailingZeros32(^mask), n)
}

// SIMD reports the selected maximum hardware width; leaf tag filters use
// their natural 256-bit tile on AVX2 and AVX-512 machines.
func SIMD() (bool, int) {
	if simd.Emulated() {
		return false, 0
	}
	if archsimd.X86.AVX512() && amd64MaxWidth >= 512 {
		return true, 512
	}
	if archsimd.X86.AVX2() && amd64MaxWidth >= 256 {
		return true, 256
	}
	return simdPortable()
}

func firstTag(tags *[nodeSize]uint8, n int, key uint64) int {
	if simd.Emulated() {
		return firstTagScalar(tags, n, uint8(key))
	}
	var mask uint32
	if archsimd.X86.AVX2() && amd64MaxWidth >= 256 {
		// A leaf has exactly 32 byte tags: one YMM load is the natural tile even
		// on AVX-512 CPUs, without reading padding beyond the allocation.
		target := archsimd.BroadcastUint8x32(uint8(key))
		mask = archsimd.LoadUint8x32(tags[:]).Equal(target).ToBits()
	} else if archsimd.X86.AVX() {
		target := archsimd.BroadcastUint8x16(uint8(key))
		a := uint32(archsimd.LoadUint8x16(tags[:16]).Equal(target).ToBits())
		b := uint32(archsimd.LoadUint8x16(tags[16:]).Equal(target).ToBits())
		mask = a | b<<16
	} else {
		return firstTagScalar(tags, n, uint8(key))
	}
	archsimd.ClearAVXUpperBits()
	i := bits.TrailingZeros32(mask)
	return min(i, n)
}

// mask16Count256 is how many of mask's sixteen lanes are set.
func mask16Count256(mask archsimd.Mask16x16) int {
	bytes := mask.ToInt16x16().AsInt8x32()
	return bits.OnesCount32(bytes.Less(archsimd.Int8x32{}).ToBits()) / 2
}
