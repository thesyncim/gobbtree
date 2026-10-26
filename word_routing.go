package gobbtree

func upperWordInlineScalar(values *[nodeSize]uint16, n int, key uint16) int {
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if values[mid] <= key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
