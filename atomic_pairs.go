package gobbtree

import "sync/atomic"

// Keys and values share a cache line. Every field is numeric, so the GC does
// not scan these blocks. Writers invalidate the old image before moving slots.
type atomicPairs struct {
	sequence atomic.Uint64
	_        [56]byte
	entries  [nodeSize]struct{ key, value atomic.Uint64 }
}

func (p *atomicPairs) lower(n int, key uint64) int {
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if p.entries[mid].key.Load() < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
func (p *atomicPairs) get(tags *[nodeSize]uint8, n int, key uint64) (uint64, bool) {
	i := firstTag(tags, n, key)
	if i >= n {
		return 0, false
	}
	candidate := p.entries[i].key.Load()
	if candidate != key {
		i = p.lower(n, key)
		if i >= n {
			return 0, false
		}
		candidate = p.entries[i].key.Load()
	}
	if candidate != key {
		return 0, false
	}
	return p.entries[i].value.Load(), true
}
