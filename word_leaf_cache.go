package gobbtree

import (
	"math/bits"
	"sync/atomic"
)

// Four leaf IDs and three quantized separators fit in one 16-byte cell.
// Queries in a separator's quantization interval use ordinary tree descent.
// Cells are immutable after publication; any key mutation retires the cache.
// The 24-bit IDs cover up to 16 million arena slots. Larger IDs fall back.
const leafCacheIDMask uint64 = 1<<24 - 1

type wordLeafCell struct {
	head atomic.Uint64 // 0: empty, 1: being filled, 2: ordinary descent
	tail atomic.Uint64
}
type wordLeafCache struct {
	base  uint64
	shift uint8
	cells []wordLeafCell
}

func newWordLeafCache(routes wordRoutes, count int64) *wordLeafCache {
	if len(routes.ids) == 0 {
		return nil
	}
	parentBits := bits.Len(uint(len(routes.ids) - 1))
	radixBits := min(20, max(parentBits, bits.Len64(uint64(count-1)/32)))
	width := int(routes.shift) + parentBits
	return &wordLeafCache{base: routes.base, shift: uint8(width - radixBits), cells: make([]wordLeafCell, 1<<radixBits)}
}
func (c *wordLeafCache) fraction(key uint64) uint64 {
	if c.shift >= 10 {
		return key >> (c.shift - 10) & 1023
	}
	return (key & ((uint64(1) << c.shift) - 1)) << (10 - c.shift)
}
func (c *wordLeafCache) lookup(key uint64) (uint32, bool) {
	index := (key - c.base) >> c.shift
	if index >= uint64(len(c.cells)) {
		return 0, false
	}
	cell := &c.cells[index]
	head := cell.head.Load()
	if head <= 2 {
		return 0, head == 0
	}
	first, second := head&leafCacheIDMask, head>>24&leafCacheIDMask
	if first == second {
		return uint32(first) | wordLeafID, false
	}
	tail := cell.tail.Load()
	cut0 := head >> 48 & 1023
	cut1 := head>>58 | ((tail >> 48 & 15) << 6)
	cut2 := tail >> 52 & 1023
	q := c.fraction(key)
	if c.shift > 10 && (q == cut0 || q == cut1 || q == cut2) {
		return 0, false
	}
	id := first
	if q >= cut0 {
		id = second
	}
	if q >= cut1 {
		id = tail & leafCacheIDMask
	}
	if q >= cut2 {
		id = tail >> 24 & leafCacheIDMask
	}
	return uint32(id) | wordLeafID, false
}

// parent holds every key in [parentLow, parentHigh]. A cell outside that range
// spans several parents and uses ordinary descent. All source reads
// participate in the map epoch.
func (m *wordMap) cacheLeafParent(c *wordLeafCache, parent *wordNode, key uint64, position, count int, version, parentLow, parentHigh uint64) {
	index := (key - c.base) >> c.shift
	if index >= uint64(len(c.cells)) {
		return
	}
	cell := &c.cells[index]
	if cell.head.Load() != 0 {
		return
	}
	low := c.base | index<<c.shift
	high := low | ((uint64(1) << c.shift) - 1)
	if low < parentLow || high > parentHigh {
		if m.version.Load() == version {
			cell.head.CompareAndSwap(0, 2)
		}
		return
	}
	first, last := position, position
	for first > 0 && atomic.LoadUint64(&parent.keys[first]) > low {
		first--
		if last-first >= 4 {
			break
		}
	}
	for last+1 < count && atomic.LoadUint64(&parent.keys[last+1]) <= high {
		last++
		if last-first >= 4 {
			break
		}
	}
	if last-first >= 4 {
		if m.version.Load() == version {
			cell.head.CompareAndSwap(0, 2)
		}
		return
	}
	var ids [4]uint64
	cuts := [3]uint64{1023, 1023, 1023}
	for i := range ids {
		child := atomic.LoadUint32(&parent.inner().children[first+min(i, last-first)])
		id := uint64(child &^ wordLeafID)
		if child&wordLeafID == 0 || id == 0 || id > leafCacheIDMask {
			if m.version.Load() == version {
				cell.head.CompareAndSwap(0, 2)
			}
			return
		}
		ids[i] = id
		if i < last-first {
			cuts[i] = c.fraction(atomic.LoadUint64(&parent.keys[first+i+1]))
		}
	}
	if m.version.Load() != version || !cell.head.CompareAndSwap(0, 1) {
		return
	}
	tail := ids[2] | ids[3]<<24 | (cuts[1]>>6)<<48 | cuts[2]<<52
	head := ids[0] | ids[1]<<24 | cuts[0]<<48 | (cuts[1]&63)<<58
	cell.tail.Store(tail)
	cell.head.Store(head)
}
