package gobbtree

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

// genericTree is the Map engine for keys or values that don't fit a word,
// upstream's latched tree (tree/latched/) over nodes of any key and value
// type, as latchedTree is for words:
//
//   - Descent is optimistic through the internal nodes, by lock coupling:
//     each node's version is checked after its child's is read. Leaves are
//     latched, shared to read and exclusive to change.
//   - A split, or a removal that rebalances, takes without waiting every
//     node it changes, at the versions its descent read, and starts over if
//     another goroutine holds one.
//   - A racing reader reads only words that can't tear: counts, separator
//     codes and pointers. Keys and values are read with their leaf latched,
//     as upstream reads what isn't Copy. A separator is a boxed key, never
//     changed once boxed, so a racing reader compares it whole.
//   - Freed nodes hold nothing and keep their latch, whose version their
//     release bumps; the garbage collector keeps one alive while a stale
//     pointer reaches it, so a reader that does fails its check.
type genericTree[K any, V any, O keyOrder[K]] struct {
	root     atomic.Pointer[gheader]
	starting sync.Mutex
}

// A node's header. Its count is read racing; whether it is a leaf never
// changes.
type gheader struct {
	latch nodeLatch
	count atomic.Int32
	leaf  bool
}

// A leaf: entries in key order, with each key's code, its window at offset
// 0, which narrows a search before keys are compared. Code slots past the
// count hold codePad, as lowerCode's vector kernels count every slot.
type gleaf[K any, V any] struct {
	gheader
	codes  [nodeSize]uint64
	keys   [nodeSize]K
	values [nodeSize]V
}

// An internal node: child i holds keys at least separator i, whose code,
// its window at offset 0, routes without reading the key; code slots past
// the count hold codePad. Separator 0 is the node's own, nil on the tree's
// left border.
type ginner[K any] struct {
	gheader
	codes    [nodeSize]atomic.Uint64
	seps     [nodeSize]atomic.Pointer[K]
	children [nodeSize]atomic.Pointer[gheader]
}

const codePad = ^uint64(0)

func asLeaf[K any, V any](h *gheader) *gleaf[K, V] { return (*gleaf[K, V])(unsafe.Pointer(h)) }
func asInner[K any](h *gheader) *ginner[K]         { return (*ginner[K])(unsafe.Pointer(h)) }

// freed marks a node a split or merge let go of: it holds nothing, so a
// stale route to it starts over, and its version moves when it is let go.
func (h *gheader) freed() { h.count.Store(0) }

// newGLeaf and newGInner make a node latched exclusively, at version 0,
// until the operation making it lets go.
func newGLeaf[K any, V any]() *gleaf[K, V] {
	n := &gleaf[K, V]{}
	n.leaf = true
	for i := range n.codes {
		n.codes[i] = codePad
	}
	n.latch.Store(latchExclusive)
	return n
}

func newGInner[K any]() *ginner[K] {
	n := &ginner[K]{}
	for i := range n.codes {
		n.codes[i].Store(codePad)
	}
	n.latch.Store(latchExclusive)
	return n
}

// Which entry a descent seeks: a key, or an end of the tree.
const (
	seekKey = iota
	seekFirst
	seekLast
)

// exactCodes reports keys whose codes order them exactly: built-in word
// keys in their natural order. Their equal codes are equal keys.
func exactCodes[O keyOrder[K], K any]() bool { return wordKeyKind[O, K]() != 0 }

// codeRange is where codes, count of them in order, equal code: [lo, hi).
// It reads them racing, by plain loads where a version check covers them.
func codeRange(codes *[nodeSize]atomic.Uint64, count int, code uint64) (lo, hi int) {
	if seqlockPlainStores {
		plain := (*[nodeSize]uint64)(unsafe.Pointer(codes))
		lo, hi = lowerCode(plain, count, code), count
		if code != ^uint64(0) {
			hi = lowerCode(plain, count, code+1)
		}
		return lo, hi
	}
	lo, hi = 0, count
	for lo < hi {
		mid := int(uint(lo+hi)>>1) & (nodeSize - 1)
		if codes[mid].Load() < code {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	end := count
	for a := lo; a < end; {
		mid := int(uint(a+end)>>1) & (nodeSize - 1)
		if codes[mid].Load() <= code {
			a = mid + 1
		} else {
			end = mid
		}
	}
	return lo, end
}

// route is the child of internal node n, of count children, whose keys
// hold key, read racing: the last whose separator is at most key, or below
// it when strict. Codes settle it unless they tie inexactly; then boxed
// separators, never changed, are compared whole. ok is false where a racing
// read finds no separator.
func (t *genericTree[K, V, O]) route(n *ginner[K], key K, code uint64, count int, strict bool) (int, bool) {
	lo, hi := codeRange(&n.codes, count, code)
	if lo == hi || exactCodes[O, K]() {
		if strict || lo == hi {
			return max(lo-1, 0), true
		}
		return hi - 1, true
	}
	// Separator 0 bounds the node from below, so it is never above key.
	lo = max(lo, 1)
	for lo < hi {
		mid := int(uint(lo+hi)>>1) & (nodeSize - 1)
		sep := n.seps[mid].Load()
		if sep == nil {
			return 0, false
		}
		order := t.compare(*sep, key)
		if order > 0 || strict && order == 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo - 1, true
}

// A descent's path: each internal node, its version, the edge taken, its
// count, and the separators beside the edge, its subtree's fences.
type gpathLevel[K any] struct {
	n         *ginner[K]
	version   uint64
	edge      int
	count     int
	low, high *K
}

type gpath[K any] struct {
	levels [maxDepth]gpathLevel[K]
	depth  int
}

// highFence is the lowest key past the leaf the path leads to, or nil.
func (p *gpath[K]) highFence() *K {
	for level := p.depth - 1; level >= 0; level-- {
		if l := &p.levels[level]; l.edge+1 < l.count {
			return l.high
		}
	}
	return nil
}

// lowFence is the lowest key of the leaf the path leads to, or nil for the
// first leaf.
func (p *gpath[K]) lowFence() *K {
	for level := p.depth - 1; level >= 0; level-- {
		if l := &p.levels[level]; l.edge > 0 {
			return l.low
		}
	}
	return nil
}

func latchHeader(h *gheader, mode int) uint64 {
	switch mode {
	case shared:
		h.latch.lockShared()
		return 0
	case exclusive:
		return h.latch.lockExclusive()
	}
	return h.latch.readVersion()
}

func unlatchHeader(h *gheader, mode int, version uint64) {
	switch mode {
	case shared:
		h.latch.unlockShared()
	case exclusive:
		h.latch.unlockExclusive(version)
	}
}

func (t *genericTree[K, V, O]) compare(a, b K) int { return compareKeys[O](a, b) }

// descend finds the leaf that seek's entry belongs in and latches it in
// mode, by optimistic lock coupling through the internal nodes; strict
// seeks the leaf of keys below key, whose code is code. path, if not nil,
// keeps the internal nodes. The tree has a root.
func (t *genericTree[K, V, O]) descend(key K, code uint64, seek int, strict bool, mode int, path *gpath[K]) (*gleaf[K, V], uint64) {
restart:
	for {
		if path != nil {
			path.depth = 0
		}
		h := t.root.Load()
		if h.leaf {
			version := latchHeader(h, mode)
			if t.root.Load() != h {
				unlatchHeader(h, mode, version)
				continue
			}
			return asLeaf[K, V](h), version
		}
		n := asInner[K](h)
		v := h.latch.readVersion()
		if t.root.Load() != h {
			continue
		}
		for depth := 0; depth < maxDepth; depth++ {
			count := int(n.count.Load())
			if count == 0 || count > nodeSize {
				continue restart
			}
			var i int
			switch seek {
			case seekFirst:
				i = 0
			case seekLast:
				i = count - 1
			default:
				var ok bool
				if i, ok = t.route(n, key, code, count, strict); !ok {
					continue restart
				}
			}
			child := n.children[i].Load()
			var low, high *K
			if path != nil {
				if i > 0 {
					low = n.seps[i].Load()
				}
				if i+1 < count {
					high = n.seps[i+1].Load()
				}
			}
			if child == nil || !h.latch.isUnchanged(v) {
				continue restart
			}
			if path != nil {
				path.levels[path.depth] = gpathLevel[K]{n: n, version: v, edge: i, count: count, low: low, high: high}
				path.depth++
			}
			if child.leaf {
				version := latchHeader(child, mode)
				if !h.latch.isUnchanged(v) {
					unlatchHeader(child, mode, version)
					continue restart
				}
				return asLeaf[K, V](child), version
			}
			cv := child.latch.readVersion()
			if !h.latch.isUnchanged(v) {
				continue restart
			}
			h, n, v = child, asInner[K](child), cv
		}
	}
}

func (t *genericTree[K, V, O]) code(key K) uint64 { return windowKey[O](key, 0) }

// find is key's slot in leaf n, latched, and whether key is there; code is
// key's code.
func (t *genericTree[K, V, O]) find(n *gleaf[K, V], key K, code uint64) (int, bool) {
	count := int(n.count.Load())
	lo := lowerCode(&n.codes, count, code)
	if exactCodes[O, K]() {
		return lo, lo < count && n.codes[lo&(nodeSize-1)] == code
	}
	hi := count
	if code != ^uint64(0) {
		hi = lowerCode(&n.codes, count, code+1)
	}
	for lo < hi {
		mid := int(uint(lo+hi)>>1) & (nodeSize - 1)
		if t.compare(n.keys[mid], key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < count && t.compare(n.keys[lo&(nodeSize-1)], key) == 0
}

// start makes the tree's root leaf, once.
func (t *genericTree[K, V, O]) start() {
	t.starting.Lock()
	defer t.starting.Unlock()
	if t.root.Load() == nil {
		n := newGLeaf[K, V]()
		n.latch.unlockExclusive(0)
		t.root.Store(&n.gheader)
	}
}

// optimisticLeaves reports trees whose lookups read a leaf without
// latching it, checking its version after: exact codes settle the search
// without comparing keys, so no user code runs on a racing read. A value is
// copied racing, perhaps torn, and returned only once the check passes;
// until then nothing reads through it, and each of its words is a pointer
// or not as its type says, which is all the collector needs.
func optimisticLeaves[O keyOrder[K], K any]() bool {
	return seqlockPlainStores && exactCodes[O, K]()
}

// get is key's value. With optimistic leaves it is read without latching
// the leaf, a few times while writers change it, before it latches shared.
func (t *genericTree[K, V, O]) get(key K) (value V, found bool) {
	if t.root.Load() == nil {
		return value, false
	}
	code := t.code(key)
	if optimisticLeaves[O, K]() {
		for try := 0; try < 4; try++ {
			n, version := t.descend(key, code, seekKey, false, optimistic, nil)
			count := min(int(n.count.Load()), nodeSize)
			i := lowerCode(&n.codes, count, code) & (nodeSize - 1)
			found = i < count && n.codes[i] == code
			value = n.values[i]
			loadFence()
			if n.latch.isUnchanged(version) {
				if !found {
					var zero V
					return zero, false
				}
				return value, true
			}
		}
	}
	n, _ := t.descend(key, code, seekKey, false, shared, nil)
	if i, ok := t.find(n, key, code); ok {
		value, found = n.values[i], true
	} else {
		var zero V
		value, found = zero, false
	}
	n.latch.unlockShared()
	return value, found
}

// read runs visit on key's entry, its stored key and value, with its leaf
// latched shared.
func (t *genericTree[K, V, O]) read(key K, visit func(K, V)) bool {
	if t.root.Load() == nil {
		return false
	}
	code := t.code(key)
	n, _ := t.descend(key, code, seekKey, false, shared, nil)
	defer n.latch.unlockShared()
	i, ok := t.find(n, key, code)
	if ok {
		visit(n.keys[i], n.values[i])
	}
	return ok
}

// update stores change's result as key's value, with its leaf latched
// exclusively; a panicking change stores nothing.
func (t *genericTree[K, V, O]) update(key K, change func(K, V) V) (V, bool) {
	var zero V
	if t.root.Load() == nil {
		return zero, false
	}
	code := t.code(key)
	n, version := t.descend(key, code, seekKey, false, exclusive, nil)
	defer n.latch.unlockExclusive(version)
	i, ok := t.find(n, key, code)
	if !ok {
		return zero, false
	}
	value := change(n.keys[i], n.values[i])
	n.values[i] = value
	return value, true
}

// isEmpty reports that the tree held no entry at some moment during the
// call: its root is a leaf without entries.
func (t *genericTree[K, V, O]) isEmpty() bool {
	for {
		h := t.root.Load()
		if h == nil {
			return true
		}
		if !h.leaf {
			return false
		}
		version := h.latch.readVersion()
		empty := h.count.Load() == 0
		if h.latch.isUnchanged(version) && t.root.Load() == h {
			return empty
		}
	}
}

// clear drops every entry. As upstream's, it is the owner's.
func (t *genericTree[K, V, O]) clear() { t.root.Store(nil) }
