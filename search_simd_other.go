//go:build goexperiment.simd && !arm64 && !amd64

package gobbtree

func lowerCode(keys *[nodeSize]uint64, n int, key uint64) int { return lowerCodePortable(keys, n, key) }
func lowerCompact(keys *[nodeSize]uint64, high *[nodeSize]uint32, n int, key uint64) int {
	return lowerCompactPortable(keys, high, n, key)
}
func findCode(keys *[nodeSize]uint64, tags *[nodeSize]uint8, n int, key uint64) int {
	return findCodePortable(keys, tags, n, key)
}
func SIMD() (bool, int) { return simdPortable() }

func firstTag(tags *[nodeSize]uint8, n int, key uint64) int {
	return firstTagScalar(tags, n, uint8(key))
}

func upperTag(tags *[nodeSize]uint8, n int, key uint8) int { return upperTagScalar(tags, n, key) }

func firstSharedTag(words *[4]uint64, n int, key uint64) int {
	return firstSharedTagFallback(words, n, key)
}
func upperSharedTag(words *[4]uint64, n int, key uint8) (int, bool) {
	return upperSharedTagFallback(words, n, key)
}

func lowerEntries(entries *[nodeSize]wordEntry, key uint64) int {
	return lowerEntriesScalar(entries, key)
}
