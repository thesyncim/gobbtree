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

// The counts accumulate each tile's masks, all ones where they hold, and
// reduce once, after the last tile.

func diskCount16(values []uint16, key uint16) (int, int) {
	var lo, eq archsimd.Int16x8
	i, n := 0, len(values)
	target := archsimd.BroadcastUint16x8(key)
	for ; i+8 <= n; i += 8 {
		v := archsimd.LoadUint16x8(values[i : i+8])
		lo = lo.Add(v.Less(target).ToInt16x8())
		eq = eq.Add(v.Equal(target).ToInt16x8())
	}
	below, equal := -int(lo.ReduceSum()), -int(eq.ReduceSum())
	for ; i < n; i++ {
		if values[i] < key {
			below++
		}
		if values[i] == key {
			equal++
		}
	}
	return below, below + equal
}

func diskCount32(values []uint32, key uint32) (int, int) {
	var lo, eq archsimd.Int32x4
	i, n := 0, len(values)
	target := archsimd.BroadcastUint32x4(key)
	for ; i+4 <= n; i += 4 {
		v := archsimd.LoadUint32x4(values[i : i+4])
		lo = lo.Add(v.Less(target).ToInt32x4())
		eq = eq.Add(v.Equal(target).ToInt32x4())
	}
	below, equal := -int(lo.ReduceSum()), -int(eq.ReduceSum())
	for ; i < n; i++ {
		if values[i] < key {
			below++
		}
		if values[i] == key {
			equal++
		}
	}
	return below, below + equal
}

func diskCount64(values []uint64, key uint64) (int, int) {
	var lo, eq archsimd.Int64x2
	i, n := 0, len(values)
	target := archsimd.BroadcastUint64x2(key)
	for ; i+2 <= n; i += 2 {
		v := archsimd.LoadUint64x2(values[i : i+2])
		lo = lo.Add(v.Less(target).ToInt64x2())
		eq = eq.Add(v.Equal(target).ToInt64x2())
	}
	below := -int(lo.GetElem(0) + lo.GetElem(1))
	equal := -int(eq.GetElem(0) + eq.GetElem(1))
	for ; i < n; i++ {
		if values[i] < key {
			below++
		}
		if values[i] == key {
			equal++
		}
	}
	return below, below + equal
}

// diskSlotRange is diskInlineRange over a page's slots, n of them live: it
// counts every slot, as upstream's NeonCount does (D56), and clamps the
// counts to n. Slots past n hold the greatest window (upstream's invariant
// 4), which no target is below, so the counts are exact; on any bytes they
// stay within 0..n. Each kind of page has one count of slots, so the loop
// runs the same trips for every page of a kind, where a loop to n ends at a
// different trip for each.
func diskSlotRange(slots []byte, width, n int, target uint64) (int, int) {
	if simd.Emulated() || len(slots) < 16 || uintptr(unsafe.Pointer(&slots[0]))%uintptr(width) != 0 {
		return diskInlineRange(slots[:n*width], width, target)
	}
	var below, equal int
	switch width {
	case 2:
		below, equal = diskSlotCount16(unsafe.Slice((*uint16)(unsafe.Pointer(&slots[0])), len(slots)/2), uint16(target))
	case 4:
		below, equal = diskSlotCount32(unsafe.Slice((*uint32)(unsafe.Pointer(&slots[0])), len(slots)/4), uint32(target))
	case 8:
		below, equal = diskSlotCount64(unsafe.Slice((*uint64)(unsafe.Pointer(&slots[0])), len(slots)/8), uint64(target))
	default:
		return diskInlineRange(slots[:n*width], width, target)
	}
	below = min(below, n)
	return below, min(below+equal, n)
}

// Each count's last tile, where the slots end short of a whole one, is the
// last whole tile of slots, which overlaps the one before: lanes the tiles
// before counted are masked off, by their positions, which depend on the
// slots' count alone.
var (
	laneIndexes16 = [8]uint16{0, 1, 2, 3, 4, 5, 6, 7}
	laneIndexes32 = [4]uint32{0, 1, 2, 3}
)

func diskSlotCount16(slots []uint16, key uint16) (int, int) {
	// Two counters each, added together once, so that half the tiles'
	// adds don't wait on the other half's: the count is on each level's
	// critical path, between the page's loads and its child's.
	var lo0, lo1, eq0, eq1 archsimd.Int16x8
	target := archsimd.BroadcastUint16x8(key)
	whole := len(slots) &^ 7
	i := 0
	for ; i+16 <= whole; i += 16 {
		v0 := archsimd.LoadUint16x8(slots[i : i+8])
		v1 := archsimd.LoadUint16x8(slots[i+8 : i+16])
		lo0 = lo0.Add(v0.Less(target).ToInt16x8())
		lo1 = lo1.Add(v1.Less(target).ToInt16x8())
		eq0 = eq0.Add(v0.Equal(target).ToInt16x8())
		eq1 = eq1.Add(v1.Equal(target).ToInt16x8())
	}
	if i < whole {
		v := archsimd.LoadUint16x8(slots[i : i+8])
		lo0 = lo0.Add(v.Less(target).ToInt16x8())
		eq0 = eq0.Add(v.Equal(target).ToInt16x8())
	}
	if whole < len(slots) {
		first := len(slots) - 8
		v := archsimd.LoadUint16x8(slots[first:])
		positions := archsimd.LoadUint16x8(laneIndexes16[:]).Add(archsimd.BroadcastUint16x8(uint16(first)))
		fresh := positions.Greater(archsimd.BroadcastUint16x8(uint16(whole - 1)))
		lo1 = lo1.Add(v.Less(target).And(fresh).ToInt16x8())
		eq1 = eq1.Add(v.Equal(target).And(fresh).ToInt16x8())
	}
	return -int(lo0.Add(lo1).ReduceSum()), -int(eq0.Add(eq1).ReduceSum())
}

func diskSlotCount32(slots []uint32, key uint32) (int, int) {
	var lo, eq archsimd.Int32x4
	target := archsimd.BroadcastUint32x4(key)
	whole := len(slots) &^ 3
	for i := 0; i < whole; i += 4 {
		v := archsimd.LoadUint32x4(slots[i : i+4])
		lo = lo.Add(v.Less(target).ToInt32x4())
		eq = eq.Add(v.Equal(target).ToInt32x4())
	}
	if whole < len(slots) {
		first := len(slots) - 4
		v := archsimd.LoadUint32x4(slots[first:])
		positions := archsimd.LoadUint32x4(laneIndexes32[:]).Add(archsimd.BroadcastUint32x4(uint32(first)))
		fresh := positions.Greater(archsimd.BroadcastUint32x4(uint32(whole - 1)))
		lo = lo.Add(v.Less(target).And(fresh).ToInt32x4())
		eq = eq.Add(v.Equal(target).And(fresh).ToInt32x4())
	}
	return -int(lo.ReduceSum()), -int(eq.ReduceSum())
}

func diskSlotCount64(slots []uint64, key uint64) (int, int) {
	var lo, eq archsimd.Int64x2
	target := archsimd.BroadcastUint64x2(key)
	whole := len(slots) &^ 1
	for i := 0; i < whole; i += 2 {
		v := archsimd.LoadUint64x2(slots[i : i+2])
		lo = lo.Add(v.Less(target).ToInt64x2())
		eq = eq.Add(v.Equal(target).ToInt64x2())
	}
	below := -int(lo.GetElem(0) + lo.GetElem(1))
	equal := -int(eq.GetElem(0) + eq.GetElem(1))
	if whole < len(slots) {
		last := slots[len(slots)-1]
		if last < key {
			below++
		} else if last == key {
			equal++
		}
	}
	return below, equal
}

// diskCountWindows16 counts the 2-byte windows of a page's slots, slots
// of them from p, below key and equal to it, by loads from the page's
// address: its caller checked the page lies in the map. The windows below
// key are counted in vectors, over every slot, as upstream's NeonCount
// does; those equal to it follow them, and are counted one by one: windows
// are sorted, and few are equal. tail masks the last tile's lanes past the
// whole tiles, for slots short of a multiple of 8 (diskTailMask).
func diskCountWindows16(p unsafe.Pointer, slots int, tail *[16]int16, key uint64) (int, int) {
	if simd.Emulated() || slots < 8 {
		return diskCountWindowsBy(p, slots, key)
	}
	at := func(i int) *[8]uint16 { return (*[8]uint16)(unsafe.Add(p, 2*i)) }
	var lo0, lo1 archsimd.Int16x8
	target := archsimd.BroadcastUint16x8(uint16(key))
	if slots >= 48 && slots <= 64 {
		// The default pages' counts, 49 slots in an internal page and 55
		// in a uint64 leaf, in one run without a loop: six whole tiles,
		// then the last 8 slots and the 8 before, masked to what the whole
		// tiles left (diskTailMasks).
		a := archsimd.LoadUint16x8Array(at(0)).Less(target).ToInt16x8()
		b := archsimd.LoadUint16x8Array(at(8)).Less(target).ToInt16x8()
		c := archsimd.LoadUint16x8Array(at(16)).Less(target).ToInt16x8()
		d := archsimd.LoadUint16x8Array(at(24)).Less(target).ToInt16x8()
		e := archsimd.LoadUint16x8Array(at(32)).Less(target).ToInt16x8()
		f := archsimd.LoadUint16x8Array(at(40)).Less(target).ToInt16x8()
		g := archsimd.LoadUint16x8Array(at(slots - 16)).Less(target).ToInt16x8().And(archsimd.LoadInt16x8Array((*[8]int16)(tail[8:16])))
		h := archsimd.LoadUint16x8Array(at(slots - 8)).Less(target).ToInt16x8().And(archsimd.LoadInt16x8Array((*[8]int16)(tail[0:8])))
		below := -int(a.Add(b).Add(c.Add(d)).Add(e.Add(f).Add(g.Add(h))).ReduceSum())
		return below, countEqual16(p, below, slots, uint16(key))
	}
	whole := slots &^ 7
	i := 0
	for ; i+16 <= whole; i += 16 {
		lo0 = lo0.Add(archsimd.LoadUint16x8Array(at(i)).Less(target).ToInt16x8())
		lo1 = lo1.Add(archsimd.LoadUint16x8Array(at(i + 8)).Less(target).ToInt16x8())
	}
	if i < whole {
		lo0 = lo0.Add(archsimd.LoadUint16x8Array(at(i)).Less(target).ToInt16x8())
	}
	if whole < slots {
		below := archsimd.LoadUint16x8Array(at(slots - 8)).Less(target).ToInt16x8()
		lo1 = lo1.Add(below.And(archsimd.LoadInt16x8Array((*[8]int16)(tail[0:8]))))
	}
	below := -int(lo0.Add(lo1).ReduceSum())
	return below, countEqual16(p, below, slots, uint16(key))
}

// countEqual16 counts the slots from below that hold key, before slots.
func countEqual16(p unsafe.Pointer, below, slots int, key uint16) int {
	equal := 0
	for below+equal < slots && *(*uint16)(unsafe.Add(p, 2*(below+equal))) == key {
		equal++
	}
	return equal
}
