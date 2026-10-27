package gobbtree

func firstSharedTagFallback(words *[4]uint64, n int, key uint64) int {
	tags := loadSharedTags(words)
	return firstTagScalar(&tags, n, uint8(key))
}
func upperSharedTagFallback(words *[4]uint64, n int, key uint8) (int, bool) {
	tags := loadSharedTags(words)
	i := upperTagScalar(&tags, n, key)
	return i, i > 0 && tags[(i-1)&(nodeSize-1)] == key
}
