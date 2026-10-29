//go:build !goexperiment.simd

package gobbtree

func lowerCode(keys *[nodeSize]uint64, n int, key uint64) int {
	return lowerCodeScalar(keys, n, key)
}

// SIMD reports whether this build uses hardware SIMD and its vector width.
func SIMD() (hardware bool, bits int) { return false, 0 }

func lowerCompact(keys *[nodeSize]uint64, high *[nodeSize]uint32, n int, key uint64) int {
	return lowerCodeScalar(keys, n, key)
}

func lowerSummary(sums *[nodeSize]uint32, n int, t uint32) int { return lowerSummaryScalar(sums, n, t) }

func findCode(keys *[nodeSize]uint64, tags *[nodeSize]uint8, n int, key uint64) int {
	return lowerCode(keys, n, key)
}

func firstTag(tags *[nodeSize]uint8, n int, key uint64) int {
	return firstTagScalar(tags, n, uint8(key))
}

func firstSharedTag(words *[4]uint64, n int, key uint64) int {
	return firstSharedTagFallback(words, n, key)
}
func lowerEntries(entries *[nodeSize]wordEntry, key uint64) int {
	return lowerEntriesScalar(entries, key)
}
