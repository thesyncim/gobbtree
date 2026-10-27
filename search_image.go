package gobbtree

import (
	"sync/atomic"
)

// Integer and float codes fully encode ordering, so their published images
// do not duplicate full keys. Strings retain keys to resolve window ties.
type searchImage[K any, V any, O keyOrder[K]] struct {
	readEdges        *[nodeSize]atomic.Pointer[searchImage[K, V, O]]
	pairs            *atomicPairs
	n                int
	valid            atomic.Bool
	valueKind        uint8
	strings, compact bool
	_                byte
	tags             [nodeSize]uint8
	wordState        *atomicWords
	leaf             *leaf[K, V, O]
	keys             *[nodeSize]K
	directory        *[256]uint8
	highCodes        *[nodeSize]uint32
	codes            *[nodeSize]uint64
	prefix           int
	imageVersion     uint64
}

func (n *searchImage[K, V, O]) lower(key K) int {
	if n.prefix > 0 {
		c := comparePrefix[O](key, n.keys[0], n.prefix)
		if c < 0 {
			return 0
		}
		if c > 0 {
			return n.n
		}
	}
	code := windowKey[O](key, n.prefix)
	i := lowerCode(n.codes, n.n, code)
	if i == n.n || n.codes[i] != code || compareKeys[O](n.keys[i], key) >= 0 {
		return i
	}
	lo, hi := i+1, n.n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if n.codes[mid] == code && compareKeys[O](n.keys[mid], key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
func (n *searchImage[K, V, O]) childIndex(key K) int {
	i := n.lower(key)
	if i < n.n && compareKeys[O](n.keys[i], key) == 0 {
		return i
	}
	return max(0, i-1)
}

// readValue validates one published leaf. Its caller also validates the
// structural epoch covering the route to this image.
func (n *searchImage[K, V, O]) readValue(key K, code uint64) (value V, found, valid bool) {
	if n.readEdges != nil {
		return value, false, false
	}
	if n.pairs != nil {
		word, ok := n.pairs.get(&n.tags, n.n, code)
		if !n.valid.Load() {
			return value, false, false
		}
		if ok {
			if n.valueKind == 1 {
				value = any(word).(V)
			} else {
				value = unpackWord[V](word, n.valueKind)
			}
		}
		return value, ok, true
	}

	if n.wordState == nil {
		n.leaf.mu.RLock()
		defer n.leaf.mu.RUnlock()
		if n.leaf.sequence.Load() != n.imageVersion {
			return value, false, false
		}
	}
	i := 0
	if n.strings {
		i = n.lower(key)
	} else {
		i = findCode(n.codes, &n.tags, n.n, code)
	}
	found = i < n.n && ((!n.strings && n.codes[i] == code) || (n.strings && compareKeys[O](n.keys[i], key) == 0))
	if state := n.wordState; state != nil {
		var word uint64
		if found {
			word = state.values[i].Load()
		}
		if !n.valid.Load() {
			return value, false, false
		}
		if found {
			if n.valueKind == 1 {
				value = any(word).(V)
			} else {
				value = unpackWord[V](word, n.valueKind)
			}
		}
	} else if found {
		value = n.leaf.values[i]
	}
	return value, found, true
}
