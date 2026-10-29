package gobbtree

// Every node holds up to nodeSize entries or children.
const nodeSize = 32

// Routing uses the upper bound: each internal slot is its child's minimum.
// Keeping this independent of the generic node lets it inline into descent.
func routeCode(keys *[nodeSize]uint64, n int, code uint64) int {
	if code == ^uint64(0) {
		return n - 1
	}
	return max(0, lowerCode(keys, n, code+1)-1)
}

// Scalar searches mask indexes that are already below n <= nodeSize. Without
// a bounds-check panic, the SIMD kernels that inline them as their fallback
// call nothing and need no stack frame or stack check.
func lowerCodeScalar(keys *[nodeSize]uint64, n int, key uint64) int {
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi)>>1) & (nodeSize - 1)
		if keys[mid] < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// lowerSummaryScalar is how many of n sorted summaries, padded past n with
// MaxUint32, are below t: a text internal node's search (upstream's D43).
func lowerSummaryScalar(sums *[nodeSize]uint32, n int, t uint32) int {
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi)>>1) & (nodeSize - 1)
		if sums[mid] < t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func firstTagScalar(tags *[nodeSize]uint8, n int, tag uint8) int {
	for i := range min(n, nodeSize) {
		if tags[i] == tag {
			return i
		}
	}
	return n
}
