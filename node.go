package gobbtree

import (
	"strings"
	"sync"
	"sync/atomic"
)

const nodeSize = 32
const minNodeSize = nodeSize / 2

// Internal nodes store one minimum key per child. Leaves alone own values.
// Separate columns keep SIMD searches from loading values or child pointers.
// These are GC-visible Go objects, with no pointer tagging or unsafe reads.
type numericColumns struct {
	highCodes [nodeSize]uint32
	tags      [nodeSize]uint8
	codes     [nodeSize]uint64
}

type node[K any, V any, O keyOrder[K]] struct {
	*numericColumns
	arena                    *nodeArena
	children                 *[nodeSize]*node[K, V, O]
	leaf                     *leaf[K, V, O]
	directory                *[256]uint8
	image                    atomic.Pointer[searchImage[K, V, O]]
	parentSlot               *atomic.Pointer[searchImage[K, V, O]]
	readEdges                *[nodeSize]atomic.Pointer[searchImage[K, V, O]]
	n, prefix                int
	prefixHead               [2]uint64
	strings, shared, compact bool
	level                    uint8
	columnsID, pairsID       uint32
	keys                     [nodeSize]K
}

type leaf[K any, V any, O keyOrder[K]] struct {
	sequence   atomic.Uint64
	words      *atomicWords
	pairs      *atomicPairs
	wordKind   uint8
	mu         sync.RWMutex
	next, prev *node[K, V, O]
	values     [nodeSize]V
}

func newLeaf[K any, V any, O keyOrder[K]](shared bool, arena *nodeArena) *node[K, V, O] {
	n := &node[K, V, O]{arena: arena, leaf: new(leaf[K, V, O]), shared: shared}
	n.numericColumns, n.columnsID = arena.columns.alloc()
	*n.numericColumns = numericColumns{}
	var order O
	n.strings = order.textual()
	initWordValues(n)
	n.pad()
	return n
}
func newInner[K any, V any, O keyOrder[K]](shared bool, arena *nodeArena) *node[K, V, O] {
	n := &node[K, V, O]{arena: arena, children: new([nodeSize]*node[K, V, O]), shared: shared}
	if shared {
		n.readEdges = new([nodeSize]atomic.Pointer[searchImage[K, V, O]])
	}
	n.numericColumns, n.columnsID = arena.columns.alloc()
	*n.numericColumns = numericColumns{}
	var order O
	n.strings = order.textual()
	n.pad()
	return n
}
func (n *node[K, V, O]) pad() {
	for i := n.n; i < nodeSize; i++ {
		n.codes[i] = ^uint64(0)
		n.highCodes[i] = ^uint32(0)
		n.tags[i] = 0
	}
}

// refit derives a prefix from the live keys, instead of upstream's ancestor
// fences. Searches check that prefix before skipping it. This makes prefix
// changes local and avoids reading mutable ancestors in the concurrent map.
func (n *node[K, V, O]) refit() {
	n.prefix = 0
	if n.strings && n.n > 1 {
		n.prefix = sharedPrefix[O](n.keys[0], n.keys[n.n-1])
	}
	n.prefixHead = prefixHead[O](n.keys[0], n.prefix)
	for i := 0; i < n.n; i++ {
		n.codes[i] = windowKey[O](n.keys[i], n.prefix)
		n.highCodes[i] = uint32(n.codes[i] >> 32)
		n.tags[i] = uint8(n.codes[i])
	}
	n.pad()
	n.refreshNumeric()
	n.publish()
}

func (n *node[K, V, O]) lower(key K) int {
	i, _ := n.lowerExact(key)
	return i
}

// lowerExact also reports whether keys[i] equals key. Equal keys share
// their encoding, so a window mismatch rules equality out without Compare.
func (n *node[K, V, O]) lowerExact(key K) (int, bool) {
	if !n.strings {
		code := numericKey[O](key)
		i := n.lowerNumeric(code)
		return i, i < n.n && n.codes[i] == code
	}
	s := newKeySearch[O](key)
	return n.lowerSearch(&s)
}
func (n *node[K, V, O]) lowerSearch(s *keySearch[K, O]) (int, bool) {
	c, code := s.locate(&n.keys[0], &n.prefixHead, n.prefix)
	if c < 0 {
		return 0, false
	}
	if c > 0 {
		return n.n, false
	}
	return lowerTied[O](&n.keys, &n.codes, n.n, s, code, n.prefix+8)
}

// lowerTied finds key's position from its window code. Usually one entry
// shares the code; full ordering settles ties in truncated windows, and makes
// zero-padding and total-order floating point semantics exact. Tied keys'
// encodings agree on their first equal bytes.
func lowerTied[O keyOrder[K], K any](keys *[nodeSize]K, codes *[nodeSize]uint64, n int, s *keySearch[K, O], code uint64, equal int) (int, bool) {
	i := lowerCode(codes, n, code)
	if i == n || codes[i] != code {
		return i, false
	}
	c := s.compareTied(&keys[i], equal)
	if c >= 0 {
		return i, c == 0
	}
	lo, hi := i+1, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if codes[mid] == code && s.compareTied(&keys[mid], equal) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < n && codes[lo] == code && s.compareTied(&keys[lo], equal) == 0
}

// compareTied orders a stored key whose window ties the search key's, their
// encodings agreeing on their first equal bytes. Byte strings compare from
// there, as upstream's cmp_from does.
func (s *keySearch[K, O]) compareTied(stored *K, equal int) int {
	if s.direct {
		x, y := textView(stored), textView(&s.key)
		equal = min(equal, len(x), len(y))
		return strings.Compare(x[equal:], y[equal:])
	}
	var o O
	return o.compare(*stored, s.key)
}

// Exact probes use the byte fingerprints; ordered positions still use lower.
func (n *node[K, V, O]) exact(key K) (int, bool) {
	if !n.strings {
		code := numericKey[O](key)
		i := findCode(&n.codes, &n.tags, n.n, code)
		return i, i < n.n && n.codes[i] == code
	}
	return n.lowerExact(key)
}
func (n *node[K, V, O]) childIndex(key K) int {
	if !n.strings {
		return n.routeNumeric(numericKey[O](key))
	}
	s := newKeySearch[O](key)
	return n.routeSearch(&s)
}
func (n *node[K, V, O]) routeSearch(s *keySearch[K, O]) int {
	i, equal := n.lowerSearch(s)
	if equal {
		return i
	}
	return max(0, i-1)
}
func (n *node[K, V, O]) findLeaf(key K) *node[K, V, O] {
	if !n.strings {
		return n.findLeafCode(numericKey[O](key))
	}
	s := newKeySearch[O](key)
	return n.findLeafSearch(&s)
}

// findLeafSearchForInsert also reports a full node on the path, which
// rules out inserting without the splitting descent.
func (n *node[K, V, O]) findLeafSearchForInsert(s *keySearch[K, O]) (*node[K, V, O], bool) {
	full := false
	for n.children != nil {
		full = full || n.n == nodeSize
		n = n.children[n.routeSearch(s)]
	}
	return n, full
}
func (n *node[K, V, O]) findLeafSearch(s *keySearch[K, O]) *node[K, V, O] {
	for n.children != nil {
		n = n.children[n.routeSearch(s)]
	}
	return n
}

// Numeric codes encode the complete ordering. Routing needs an upper bound
// on those codes, with no full-key load or re-encoding at each tree level.
func (n *node[K, V, O]) findLeafCode(code uint64) *node[K, V, O] {
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

// The shortcut may skip descent only when no ancestor needs its usual split.
func (n *node[K, V, O]) findLeafForInsert(code uint64) (*node[K, V, O], bool) {
	full := false
	for n.children != nil {
		if n.n == nodeSize {
			full = true
		}
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
	return n, full
}
func (n *node[K, V, O]) insertLeaf(i int, key K, value V) {
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
		n.codes[i] = windowKey[O](key, n.prefix)
		n.highCodes[i] = uint32(n.codes[i] >> 32)
		n.tags[i] = uint8(n.codes[i])
		n.refreshNumeric()
		n.publishFrom(i)
	}
}
func (n *node[K, V, O]) deleteLeaf(i int) V {
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
func (n *node[K, V, O]) split() *node[K, V, O] {
	var right *node[K, V, O]
	if n.leaf != nil {
		right = newLeaf[K, V, O](n.shared, n.arena)
		copy(right.leaf.values[:], n.leaf.values[minNodeSize:])
		clear(n.leaf.values[minNodeSize:])
		right.leaf.next = n.leaf.next
		if right.leaf.next != nil {
			right.leaf.next.leaf.prev = right
		}
		right.leaf.prev = n
		n.leaf.next = right
	} else {
		right = newInner[K, V, O](n.shared, n.arena)
		copy(right.children[:], n.children[minNodeSize:])
		clear(n.children[minNodeSize:])
	}
	right.level = n.level
	copy(right.keys[:], n.keys[minNodeSize:])
	clear(n.keys[minNodeSize:])
	n.n = minNodeSize
	right.n = nodeSize - minNodeSize
	n.refit()
	right.refit()
	return right
}
func (n *node[K, V, O]) insertChild(i int, child *node[K, V, O]) {
	n.level = child.level + 1
	copy(n.keys[i+1:n.n+1], n.keys[i:n.n])
	copy(n.children[i+1:n.n+1], n.children[i:n.n])
	n.keys[i], n.children[i] = child.keys[0], child
	n.n++
	n.refit()
}
func (n *node[K, V, O]) removeChild(i int) {
	copy(n.keys[i:n.n-1], n.keys[i+1:n.n])
	copy(n.children[i:n.n-1], n.children[i+1:n.n])
	n.n--
	var k K
	n.keys[n.n] = k
	n.children[n.n] = nil
	n.refit()
}

func (n *node[K, V, O]) refreshNumeric() {
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
func (n *node[K, V, O]) lowerNumeric(code uint64) int {
	if n.compact {
		return lowerCompact(&n.codes, &n.highCodes, n.n, code)
	}
	return lowerCode(&n.codes, n.n, code)
}
func (n *node[K, V, O]) routeNumeric(code uint64) int {
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

func (n *node[K, V, O]) release() {
	if image := n.image.Load(); image != nil {
		image.valid.Store(false)
	}
	n.arena.columns.release(n.columnsID)
	if n.pairsID != 0 {
		n.arena.pairs.release(n.pairsID)
	}
}
