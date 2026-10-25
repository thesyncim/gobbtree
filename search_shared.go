package gobbtree

func firstSharedTagFallback(words *[4]uint64, n int, key uint64) int {
	tags := loadSharedTags(words)
	return firstTag(&tags, n, key)
}
func upperSharedTagFallback(words *[4]uint64, n int, key uint8) (int, bool) {
	tags := loadSharedTags(words)
	i := upperTag(&tags, n, key)
	return i, i > 0 && tags[i-1] == key
}
