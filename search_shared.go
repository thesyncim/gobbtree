package gobbtree

func firstSharedTagFallback(words *[4]uint64, n int, key uint64) int {
	tags := loadSharedTags(words)
	return firstTagScalar(&tags, n, uint8(key))
}
