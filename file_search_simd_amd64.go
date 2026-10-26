//go:build goexperiment.simd && amd64

package gobbtree

import (
	"math/bits"
	"simd"
	"simd/archsimd"
	"unsafe"
)

// Vector loads cover complete live tiles only; the short tail stays scalar.
func diskInlineRange(data []byte, width int, target uint64) (int, int) {
	if len(data) == 0 {
		return 0, 0
	}
	if simd.Emulated() || uintptr(unsafe.Pointer(&data[0]))%uintptr(width) != 0 {
		return diskInlineRangeScalar(data, width, target)
	}
	switch width {
	case 2:
		return diskCount16(unsafe.Slice((*uint16)(unsafe.Pointer(&data[0])), len(data)/2), uint16(target))
	case 4:
		return diskCount32(unsafe.Slice((*uint32)(unsafe.Pointer(&data[0])), len(data)/4), uint32(target))
	case 8:
		return diskCount64(unsafe.Slice((*uint64)(unsafe.Pointer(&data[0])), len(data)/8), uint64(target))
	}
	return diskInlineRangeScalar(data, width, target)
}

func diskCount16(values []uint16, key uint16) (int, int) {
	lo, eq, i := 0, 0, 0
	if archsimd.X86.AVX512() && amd64MaxWidth >= 512 {
		target := archsimd.BroadcastUint16x32(key)
		for ; i+32 <= len(values); i += 32 {
			v := archsimd.LoadUint16x32(values[i : i+32])
			lo += bits.OnesCount64(uint64(v.Less(target).ToBits()))
			eq += bits.OnesCount64(uint64(v.Equal(target).ToBits()))
		}
	}
	if archsimd.X86.AVX2() && amd64MaxWidth >= 256 {
		target := archsimd.BroadcastUint16x16(key)
		for ; i+16 <= len(values); i += 16 {
			v := archsimd.LoadUint16x16(values[i : i+16])
			// Mask16x16.ToBits requires AVX-512. Count the two byte
			// sign bits per word instead, using AVX2's VPMOVMSKB.
			lo += diskMask16Count(v.Less(target))
			eq += diskMask16Count(v.Equal(target))
		}
	}
	if archsimd.X86.AVX2() || archsimd.X86.AVX512() {
		archsimd.ClearAVXUpperBits()
	}
	for ; i < len(values); i++ {
		if values[i] < key {
			lo++
		}
		if values[i] == key {
			eq++
		}
	}
	return lo, lo + eq
}

func diskMask16Count(mask archsimd.Mask16x16) int {
	bytes := mask.ToInt16x16().AsInt8x32()
	return bits.OnesCount32(bytes.Less(archsimd.Int8x32{}).ToBits()) / 2
}

func diskCount32(values []uint32, key uint32) (int, int) {
	lo, eq, i := 0, 0, 0
	if archsimd.X86.AVX512() && amd64MaxWidth >= 512 {
		target := archsimd.BroadcastUint32x16(key)
		for ; i+16 <= len(values); i += 16 {
			v := archsimd.LoadUint32x16(values[i : i+16])
			lo += bits.OnesCount64(uint64(v.Less(target).ToBits()))
			eq += bits.OnesCount64(uint64(v.Equal(target).ToBits()))
		}
	}
	if archsimd.X86.AVX2() && amd64MaxWidth >= 256 {
		target := archsimd.BroadcastUint32x8(key)
		for ; i+8 <= len(values); i += 8 {
			v := archsimd.LoadUint32x8(values[i : i+8])
			lo += bits.OnesCount64(uint64(v.Less(target).ToBits()))
			eq += bits.OnesCount64(uint64(v.Equal(target).ToBits()))
		}
	}
	if archsimd.X86.AVX2() || archsimd.X86.AVX512() {
		archsimd.ClearAVXUpperBits()
	}
	for ; i < len(values); i++ {
		if values[i] < key {
			lo++
		}
		if values[i] == key {
			eq++
		}
	}
	return lo, lo + eq
}

func diskCount64(values []uint64, key uint64) (int, int) {
	lo, eq, i := 0, 0, 0
	if archsimd.X86.AVX512() && amd64MaxWidth >= 512 {
		target := archsimd.BroadcastUint64x8(key)
		for ; i+8 <= len(values); i += 8 {
			v := archsimd.LoadUint64x8(values[i : i+8])
			lo += bits.OnesCount64(uint64(v.Less(target).ToBits()))
			eq += bits.OnesCount64(uint64(v.Equal(target).ToBits()))
		}
	}
	if archsimd.X86.AVX2() && amd64MaxWidth >= 256 {
		target := archsimd.BroadcastUint64x4(key)
		for ; i+4 <= len(values); i += 4 {
			v := archsimd.LoadUint64x4(values[i : i+4])
			lo += bits.OnesCount64(uint64(v.Less(target).ToBits()))
			eq += bits.OnesCount64(uint64(v.Equal(target).ToBits()))
		}
	}
	if archsimd.X86.AVX2() || archsimd.X86.AVX512() {
		archsimd.ClearAVXUpperBits()
	}
	for ; i < len(values); i++ {
		if values[i] < key {
			lo++
		}
		if values[i] == key {
			eq++
		}
	}
	return lo, lo + eq
}
