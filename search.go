package gobbtree

import "encoding/binary"

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

// Broad numeric nodes index the first key in each top-byte bucket.
// The directory only narrows the search; full codes settle bucket boundaries.
// Each eight-bucket word starts at the count of keys in earlier words; a key
// adds one to the buckets above its own within its word.
func fillDirectory(directory *[256]uint8, codes *[nodeSize]uint64, n int) {
	const ones = 0x0101010101010101
	i := 0
	for word := 0; word < 256/8; word++ {
		counts := uint64(i) * ones
		for i < n && int(codes[i]>>59) == word {
			counts += ones << (8 * (codes[i]>>56&7 + 1))
			i++
		}
		binary.LittleEndian.PutUint64(directory[word*8:], counts)
	}
}

// Refill the buckets that interior codes first..last can affect. Their
// unchanged neighbors bound them, so no other bucket's lower bound moves.
func refillDirectory(directory *[256]uint8, codes *[nodeSize]uint64, first, last int) {
	i := first
	end := int(codes[last+1] >> 56)
	for bucket := int(codes[first-1]>>56) + 1; bucket <= end; bucket++ {
		for i <= last && codes[i] < uint64(bucket)<<56 {
			i++
		}
		directory[bucket] = uint8(i)
	}
}
func directoryRoute(codes *[nodeSize]uint64, directory *[256]uint8, n int, code uint64) int {
	i := int(directory[code>>56])
	for i < n && codes[i] <= code {
		i++
	}
	return max(0, i-1)
}

func routeCompact(keys *[nodeSize]uint64, high *[nodeSize]uint32, n int, code uint64) int {
	if code == ^uint64(0) {
		return n - 1
	}
	return max(0, lowerCompact(keys, high, n, code+1)-1)
}

func firstTagScalar(tags *[nodeSize]uint8, n int, tag uint8) int {
	for i := 0; i < n; i++ {
		if tags[i] == tag {
			return i
		}
	}
	return n
}

func upperTagScalar(tags *[nodeSize]uint8, n int, key uint8) int {
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if tags[mid] <= key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
