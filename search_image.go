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
	prefixHead       [2]uint64
	imageVersion     uint64
	// node is set on a textual leaf's only image. Its keys stay in the node:
	// readers latch the leaf and validate the structural epoch instead of
	// searching a copy, so leaf writes publish nothing.
	node *node[K, V, O]
}

func (n *searchImage[K, V, O]) lower(key K) int {
	i, _ := n.lowerExact(key)
	return i
}
func (n *searchImage[K, V, O]) lowerExact(key K) (int, bool) {
	s := newKeySearch[O](key)
	return n.lowerSearch(&s)
}
func (n *searchImage[K, V, O]) lowerSearch(s *keySearch[K, O]) (int, bool) {
	c, code := s.locate(&n.keys[0], &n.prefixHead, n.prefix)
	if c < 0 {
		return 0, false
	}
	if c > 0 {
		return n.n, false
	}
	return lowerTied[O](n.keys, n.codes, n.n, s, code)
}
func (n *searchImage[K, V, O]) childIndex(key K) int {
	s := newKeySearch[O](key)
	return n.routeSearch(&s)
}
func (n *searchImage[K, V, O]) routeSearch(s *keySearch[K, O]) int {
	i, equal := n.lowerSearch(s)
	if equal {
		return i
	}
	return max(0, i-1)
}

// readValue validates one published leaf. Its caller also validates the
// structural epoch covering the route to this image.
func (n *searchImage[K, V, O]) readValue(key K, code uint64, epoch *atomic.Uint64, version uint64) (value V, found, valid bool) {
	if n.readEdges != nil {
		return value, false, false
	}
	if n.node != nil {
		s := newKeySearch[O](key)
		value, found, valid = n.node.latchedGet(&s, epoch, version)
		return value, found, valid
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
		i, found = n.lowerExact(key)
	} else {
		i = findCode(n.codes, &n.tags, n.n, code)
		found = i < n.n && n.codes[i] == code
	}
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
