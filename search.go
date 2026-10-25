package gobbtree

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
