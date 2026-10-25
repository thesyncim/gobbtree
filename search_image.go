package gobbtree

import (
	"cmp"
	"sync/atomic"
)

// Integer and float codes fully encode ordering, so their published images
// do not duplicate full keys. Strings retain keys to resolve window ties.
type searchImage[K cmp.Ordered, V any] struct {
	n            int
	prefix       int
	readEdges    *[nodeSize]atomic.Pointer[searchImage[K, V]]
	wordState    *atomicWords
	imageVersion uint64
	leaf         *leaf[K, V]
	strings      bool
	valueKind    uint8
	compact      bool
	valid        atomic.Bool
	directory    *[256]uint8
	highCodes    [nodeSize]uint32
	tags         [nodeSize]uint8
	codes        [nodeSize]uint64
	keys         *[nodeSize]K
}

func (n *searchImage[K, V]) lower(key K) int {
	if n.prefix > 0 {
		s, _ := keyString(key)
		first, _ := keyString(n.keys[0])
		c := cmp.Compare(s[:min(len(s), n.prefix)], first[:n.prefix])
		if c < 0 {
			return 0
		}
		if c > 0 {
			return n.n
		}
	}
	code := keyCode(key, n.prefix)
	i := lowerCode(&n.codes, n.n, code)
	if i == n.n || n.codes[i] != code || cmp.Compare(n.keys[i], key) >= 0 {
		return i
	}
	lo, hi := i+1, n.n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if n.codes[mid] == code && cmp.Compare(n.keys[mid], key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
func (n *searchImage[K, V]) childIndex(key K) int {
	i := n.lower(key)
	if i < n.n && cmp.Compare(n.keys[i], key) == 0 {
		return i
	}
	return max(0, i-1)
}

// readValue validates one published leaf. Its caller also validates the
// structural epoch covering the route to this image.
func (n *searchImage[K, V]) readValue(key K, code uint64) (value V, found, valid bool) {
	if n.readEdges != nil {
		return value, false, false
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
		i = findCode(&n.codes, &n.tags, n.n, code)
	}
	found = i < n.n && ((!n.strings && n.codes[i] == code) || (n.strings && cmp.Compare(n.keys[i], key) == 0))
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
