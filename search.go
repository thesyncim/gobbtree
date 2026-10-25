package gobbtree

// Routing uses the upper bound: each internal slot is its child's minimum.
// Keeping this independent of the generic node lets it inline into descent.
func routeCode(keys *[nodeSize]uint64, n int, code uint64) int {
	if code == ^uint64(0) {
		return n - 1
	}
	return max(0, lowerCode(keys, n, code+1)-1)
}

func lowerCodeScalar(keys *[nodeSize]uint64, n int, key uint64) int {
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if keys[mid] < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
