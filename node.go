package gobbtree

import (
	"cmp"
	"sync"
	"sync/atomic"
)

const nodeSize = 32
const minNodeSize = nodeSize / 2

// Internal nodes store one minimum key per child. Leaves alone own values.
// Separate columns keep SIMD searches from loading values or child pointers.
// These are GC-visible Go objects, with no pointer tagging or unsafe reads.
type node[K cmp.Ordered, V any] struct {
	n          int
	prefix     int
	children   *[nodeSize]*node[K, V]
	leaf       *leaf[K, V]
	strings    bool
	shared     bool
	compact    bool
	directory  *[256]uint8
	highCodes  [nodeSize]uint32
	tags       [nodeSize]uint8
	codes      [nodeSize]uint64
	keys       [nodeSize]K
	image      atomic.Pointer[searchImage[K, V]]
	parentSlot *atomic.Pointer[searchImage[K, V]]
	readEdges  *[nodeSize]atomic.Pointer[searchImage[K, V]]
}

type leaf[K cmp.Ordered, V any] struct {
	sequence   atomic.Uint64
	words      *atomicWords
	wordKind   uint8
	mu         sync.RWMutex
	next, prev *node[K, V]
	values     [nodeSize]V
}

func newLeaf[K cmp.Ordered, V any](shared bool) *node[K, V] {
	n := &node[K, V]{leaf: new(leaf[K, V]), shared: shared}
	initWordValues(n)
	var key K
	_, n.strings = keyString(key)
	n.pad()
	return n
}
func newInner[K cmp.Ordered, V any](shared bool) *node[K, V] {
	n := &node[K, V]{children: new([nodeSize]*node[K, V]), shared: shared}
	if shared {
		n.readEdges = new([nodeSize]atomic.Pointer[searchImage[K, V]])
	}
	var key K
	_, n.strings = keyString(key)
	n.pad()
	return n
}
func (n *node[K, V]) pad() {
	for i := n.n; i < nodeSize; i++ {
		n.codes[i] = ^uint64(0)
		n.highCodes[i] = ^uint32(0)
		n.tags[i] = 0
	}
}

// refit derives a prefix from the live keys, instead of upstream's ancestor
// fences. Searches check that prefix before skipping it. This makes prefix
// changes local and avoids reading mutable ancestors in the concurrent map.
func (n *node[K, V]) refit() {
	n.prefix = 0
	if n.strings && n.n > 1 {
		if a, ok := keyString(n.keys[0]); ok {
			b, _ := keyString(n.keys[n.n-1])
			end := min(len(a), len(b), 65535)
			for n.prefix < end && a[n.prefix] == b[n.prefix] {
				n.prefix++
			}
		}
	}
	for i := 0; i < n.n; i++ {
		n.codes[i] = keyCode(n.keys[i], n.prefix)
		n.highCodes[i] = uint32(n.codes[i] >> 32)
		n.tags[i] = uint8(n.codes[i])
	}
	n.pad()
	n.refreshNumeric()
	n.publish()
}

func (n *node[K, V]) lower(key K) int {
	if !n.strings {
		return n.lowerNumeric(numericCode(key))
	}
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
	if i == n.n || n.codes[i] != code {
		return i
	}
	// Usually one candidate. Full ordering settles ties in truncated windows,
	// and makes zero-padding and total-order floating point semantics exact.
	if cmp.Compare(n.keys[i], key) >= 0 {
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
func (n *node[K, V]) childIndex(key K) int {
	if !n.strings {
		return n.routeNumeric(numericCode(key))
	}
	i := n.lower(key)
	if i < n.n && cmp.Compare(n.keys[i], key) == 0 {
		return i
	}
	return max(0, i-1)
}
func (n *node[K, V]) findLeaf(key K) *node[K, V] {
	if !n.strings {
		return n.findLeafCode(numericCode(key))
	}
	for n.children != nil {
		n = n.children[n.childIndex(key)]
	}
	return n
}

// Numeric codes encode the complete ordering. Routing needs an upper bound
// on those codes, with no full-key load or re-encoding at each tree level.
func (n *node[K, V]) findLeafCode(code uint64) *node[K, V] {
	for n.children != nil {
		index := 0
		if n.directory != nil {
			index = directoryRoute(&n.codes, n.directory, n.n, code)
		} else if n.compact {
			if code == ^uint64(0) {
				index = n.n - 1
			} else {
				index = max(0, lowerCompact(&n.codes, &n.highCodes, n.n, code+1)-1)
			}
		} else {
			index = routeCode(&n.codes, n.n, code)
		}
		n = n.children[index]
	}
	return n
}
func (n *node[K, V]) insertLeaf(i int, key K, value V) {
	copy(n.keys[i+1:n.n+1], n.keys[i:n.n])
	copy(n.codes[i+1:n.n+1], n.codes[i:n.n])
	copy(n.highCodes[i+1:n.n+1], n.highCodes[i:n.n])
	copy(n.tags[i+1:n.n+1], n.tags[i:n.n])
	copy(n.leaf.values[i+1:n.n+1], n.leaf.values[i:n.n])
	n.keys[i], n.leaf.values[i] = key, value
	n.n++
	if n.strings && (i == 0 || i == n.n-1) {
		n.refit()
	} else {
		n.codes[i] = keyCode(key, n.prefix)
		n.highCodes[i] = uint32(n.codes[i] >> 32)
		n.tags[i] = uint8(n.codes[i])
		n.refreshNumeric()
		n.publishFrom(i)
	}
}
func (n *node[K, V]) deleteLeaf(i int) V {
	old := n.leaf.values[i]
	copy(n.keys[i:n.n-1], n.keys[i+1:n.n])
	copy(n.codes[i:n.n-1], n.codes[i+1:n.n])
	copy(n.highCodes[i:n.n-1], n.highCodes[i+1:n.n])
	copy(n.tags[i:n.n-1], n.tags[i+1:n.n])
	copy(n.leaf.values[i:n.n-1], n.leaf.values[i+1:n.n])
	n.n--
	var k K
	var v V
	n.keys[n.n], n.leaf.values[n.n] = k, v
	n.codes[n.n] = ^uint64(0)
	n.highCodes[n.n] = ^uint32(0)
	n.tags[n.n] = 0
	if n.strings && (i == 0 || i == n.n) {
		n.refit()
	} else {
		n.refreshNumeric()
		n.publishFrom(i)
	}
	return old
}

// split divides a full node. The caller installs its right half in a parent.
func (n *node[K, V]) split() *node[K, V] {
	var right *node[K, V]
	if n.leaf != nil {
		right = newLeaf[K, V](n.shared)
		copy(right.leaf.values[:], n.leaf.values[minNodeSize:])
		clear(n.leaf.values[minNodeSize:])
		right.leaf.next = n.leaf.next
		if right.leaf.next != nil {
			right.leaf.next.leaf.prev = right
		}
		right.leaf.prev = n
		n.leaf.next = right
	} else {
		right = newInner[K, V](n.shared)
		copy(right.children[:], n.children[minNodeSize:])
		clear(n.children[minNodeSize:])
	}
	copy(right.keys[:], n.keys[minNodeSize:])
	clear(n.keys[minNodeSize:])
	n.n = minNodeSize
	right.n = nodeSize - minNodeSize
	n.refit()
	right.refit()
	return right
}
func (n *node[K, V]) insertChild(i int, child *node[K, V]) {
	copy(n.keys[i+1:n.n+1], n.keys[i:n.n])
	copy(n.children[i+1:n.n+1], n.children[i:n.n])
	n.keys[i], n.children[i] = child.keys[0], child
	n.n++
	n.refit()
}
func (n *node[K, V]) removeChild(i int) {
	copy(n.keys[i:n.n-1], n.keys[i+1:n.n])
	copy(n.children[i:n.n-1], n.children[i+1:n.n])
	n.n--
	var k K
	n.keys[n.n] = k
	n.children[n.n] = nil
	n.refit()
}

func (n *node[K, V]) refreshNumeric() {
	n.compact = !n.strings && n.n > 0 && n.codes[0]>>32 != n.codes[n.n-1]>>32
	if !n.strings && n.children != nil && n.n >= 4 && n.codes[n.n-1]>>56-n.codes[0]>>56 >= uint64(2*n.n) {
		if n.directory == nil {
			n.directory = new([256]uint8)
		}
		fillDirectory(n.directory, &n.codes, n.n)
	} else {
		n.directory = nil
	}
}
func (n *node[K, V]) lowerNumeric(code uint64) int {
	if n.compact {
		return lowerCompact(&n.codes, &n.highCodes, n.n, code)
	}
	return lowerCode(&n.codes, n.n, code)
}
func (n *node[K, V]) routeNumeric(code uint64) int {
	if n.directory != nil {
		return directoryRoute(&n.codes, n.directory, n.n, code)
	}
	if n.compact {
		if code == ^uint64(0) {
			return n.n - 1
		}
		return max(0, lowerCompact(&n.codes, &n.highCodes, n.n, code+1)-1)
	}
	return routeCode(&n.codes, n.n, code)
}
