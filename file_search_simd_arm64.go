//go:build goexperiment.simd && arm64

package gobbtree

import (
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
	target := archsimd.BroadcastUint16x8(key)
	for ; i+8 <= len(values); i += 8 {
		v := archsimd.LoadUint16x8(values[i : i+8])
		lo -= int(v.Less(target).ToInt16x8().ReduceSum())
		eq -= int(v.Equal(target).ToInt16x8().ReduceSum())
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

func diskCount32(values []uint32, key uint32) (int, int) {
	lo, eq, i := 0, 0, 0
	target := archsimd.BroadcastUint32x4(key)
	for ; i+4 <= len(values); i += 4 {
		v := archsimd.LoadUint32x4(values[i : i+4])
		lo -= int(v.Less(target).ToInt32x4().ReduceSum())
		eq -= int(v.Equal(target).ToInt32x4().ReduceSum())
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
	target := archsimd.BroadcastUint64x2(key)
	for ; i+2 <= len(values); i += 2 {
		v := archsimd.LoadUint64x2(values[i : i+2])
		a := v.Less(target).ToInt64x2()
		b := v.Equal(target).ToInt64x2()
		lo -= int(a.GetElem(0) + a.GetElem(1))
		eq -= int(b.GetElem(0) + b.GetElem(1))
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
