package gobbtree

import (
	"encoding/binary"
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// latchedTree is the numeric Map's tree that threads share, ported from
// upstream's LatchedTree (tree/latched/, design/09-concurrency.md):
//
//   - Descent is optimistic through the internal nodes, by optimistic lock
//     coupling: at each node, route the key, read the child's edge and check
//     the node's version, then read the child's version and check the node's
//     again. The leaf is latched, shared or exclusive; a lookup reads it in
//     place at a version.
//   - Operations change a leaf with that leaf latched exclusively, and let
//     go before returning.
//   - A split, or a removal that rebalances, descends again keeping its path,
//     then takes, without waiting, every node it changes: path nodes by a
//     compare-and-swap at the versions the descent read, siblings at the
//     versions they are at. Any node another thread holds, or that changed,
//     makes it let go of all and start over. No lock spans the tree.
//   - Pages are type-stable: a freed node goes on the free list of its kind
//     and comes back only as a node of that kind, keeping its latch word,
//     whose version only grows. A stale pointer always reaches a node of the
//     kind it says, and its version check fails.
//   - The tree points at its last leaf, for appends (D45).
//
// Leaves keep their entries sorted, padded with MaxUint64 keys; an internal
// node's key i bounds child i's keys from below, key 0 being its own. Leaves
// keep no sibling links, as upstream's.
type latchedTree struct {
	root      atomic.Uint32
	rightmost atomic.Uint32
	blocks    atomic.Pointer[[]*wordNode]
	routes    atomic.Pointer[latchedRoutes]
	// Every descent reads the words above; splits and merges write the
	// pages below. Apart, a page taken costs no other core the line.
	_ [128 - 24]byte
	// appended is the largest key an append stored, which a run of them
	// rewrites on its own line: a key above it tries the last leaf first.
	appended atomic.Uint64
	_        [128 - 8]byte
	pages    latchedPages
	starting sync.Mutex
}

// Leaves and internal nodes below the root are underfull below these sizes,
// upstream's max(capacity/8, 4) (D22). Internal nodes count children.
const (
	leafMinLen  = max(nodeSize/8, 4)
	innerMinLen = max(nodeSize/8, 4) + 1
	// maxDepth bounds the levels above the leaves: an internal node below
	// the root has at least innerMinLen children.
	maxDepth = 24
)

// The pages of a latched tree: slabs that never move, and a free list per
// kind, linked through the directory field, which readers never read.
// Only splits and merges take its lock, to take or give a page.
type latchedPages struct {
	// A spin lock, as upstream's free lists': held only to take or give a
	// page, it never parks a goroutine.
	mu        storeLock
	slabs     []*wordNode
	used      int
	freeLeaf  uint32
	freeInner uint32
}

// storeLock is a spin lock for the pages. Where stores stay ordered it is
// released by a plain store instead of sync.Mutex's locked read-modify-write.
// Waiters yield instead of parking.
type storeLock struct{ state atomic.Uint32 }

func (l *storeLock) TryLock() bool { return l.state.CompareAndSwap(0, 1) }
func (l *storeLock) Lock() {
	for !l.TryLock() {
		runtime.Gosched()
	}
}
func (l *storeLock) Unlock() {
	if orderedStores {
		*(*uint32)(unsafe.Pointer(&l.state)) = 0
		return
	}
	l.state.Store(0)
}

func (t *latchedTree) dir() []*wordNode {
	if d := t.blocks.Load(); d != nil {
		return *d
	}
	return nil
}

// nodeIn resolves an ID through a directory, or nil for an ID past it: a
// node allocated after the directory was read, or no node.
func nodeIn(dir []*wordNode, id uint32) *wordNode {
	index := (id &^ wordLeafID) - 1
	block := int(index >> 8)
	if block >= len(dir) {
		return nil
	}
	return wordSlabNode(dir[block], index)
}

// node resolves an ID the caller reached through nodes it holds.
func (t *latchedTree) node(id uint32) *wordNode { return nodeIn(t.dir(), id) }

// alloc takes a page for a new node of the kind leaf says, off its free list
// if it can, latched exclusively, waiting for a thread that reached a free
// page through a stale pointer, which holds it a moment and waits on nothing.
// Only taking the page holds the pages' lock.
func (t *latchedTree) alloc(leaf bool) (uint32, *wordNode, uint64) {
	t.pages.mu.Lock()
	id := t.take(leaf)
	t.pages.mu.Unlock()
	n := t.node(id)
	version := n.lockExclusive()
	n.directory = 0
	// A page keeps its kind for life: only a fresh one takes it, as no
	// reader reaches it yet.
	if n.leaf != leaf {
		n.leaf = leaf
	}
	if leaf {
		for i := range n.entries() {
			storeEntryRacing(n, i, wordEntry{key: ^uint64(0)})
		}
		storeTagsRacing(n, [nodeSize]uint8{})
		setLeafFences(n, 0, 0)
	} else {
		for i := range n.keys {
			storeKeyRacing(n, i, ^uint64(0))
			storeChildRacing(n, i, 0)
		}
	}
	storeCountRacing(n, 0)
	if !leaf {
		setHighFence(n, 0)
		refitWindows(n)
	}
	return id, n, version
}

// take is a page for a new node of leaf's kind: off its free list, or the
// next slot of the arena. The caller holds pages.mu.
func (t *latchedTree) take(leaf bool) uint32 {
	p := &t.pages
	free := &p.freeInner
	if leaf {
		free = &p.freeLeaf
	}
	var id uint32
	if *free != 0 {
		id = *free
		*free = t.node(id).directory
	} else {
		slab := len(p.slabs) - 1
		if slab < 0 || p.used == wordSlabSize(slab) {
			if len(p.slabs) >= (1<<23)-1 {
				panic("gobbtree: numeric arena exhausted")
			}
			nodes := make([]wordNode, wordSlabSize(len(p.slabs)))
			// Readers index only below the length they loaded, so a slab
			// goes into spare capacity before a new header publishes it;
			// the directory doubles, not copies, as it grows.
			p.slabs = append(p.slabs, &nodes[0])
			dir := p.slabs
			t.blocks.Store(&dir)
			slab, p.used = len(p.slabs)-1, 0
		}
		id = uint32(slab)<<8 | uint32(p.used) + 1
		p.used++
		if leaf {
			id |= wordLeafID
		}
	}
	return id
}

// freedPages are the pages one operation freed. They go on their free lists
// once the operation let go of them, so that their latch words say they
// changed (upstream's NodePages).
type freedPages struct {
	ids [maxDepth + 1]uint32
	n   int
}

func (f *freedPages) add(id uint32) { f.ids[f.n] = id; f.n++ }

// release puts freed pages on their free lists. The caller let go of them.
func (t *latchedTree) release(f *freedPages) {
	if f.n == 0 {
		return
	}
	p := &t.pages
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range f.ids[:f.n] {
		free := &p.freeInner
		if id&wordLeafID != 0 {
			free = &p.freeLeaf
		}
		t.node(id).directory = *free
		*free = id
	}
}

// start makes the tree's root leaf, once: the first insert's.
func (t *latchedTree) start() {
	t.starting.Lock()
	defer t.starting.Unlock()
	if t.root.Load() != 0 {
		return
	}
	id, n, version := t.alloc(true)
	t.root.Store(id)
	t.rightmost.Store(id)
	n.unlockExclusive(version)
}

// Racing accessors. A node is read in place while a thread holding it
// changes it; the reader's version check throws away whatever it read
// meanwhile. Where plain stores order (seqlockPlainStores), both sides use
// plain accesses; elsewhere, and for the race detector, atomic ones.

var countWordShift = func() uint32 {
	if binary.NativeEndian.Uint16([]byte{1, 0}) == 1 {
		return 0
	}
	return 24
}()

func (n *wordNode) countWord() *uint32 { return (*uint32)(unsafe.Pointer(n)) }

func loadCountRacing(n *wordNode) int {
	if seqlockPlainStores {
		return int(n.n)
	}
	return int(atomic.LoadUint32(n.countWord()) >> countWordShift & 0xff)
}

func storeCountRacing(n *wordNode, count int) {
	if seqlockPlainStores {
		n.n = uint8(count)
		return
	}
	word := atomic.LoadUint32(n.countWord())
	atomic.StoreUint32(n.countWord(), word&^(0xff<<countWordShift)|uint32(count)<<countWordShift)
}

func storeEntryRacing(n *wordNode, i int, entry wordEntry) {
	if seqlockPlainStores {
		n.entries()[i] = entry
		return
	}
	atomic.StoreUint64(&n.entries()[i].key, entry.key)
	atomic.StoreUint64(&n.entries()[i].value, entry.value)
}

func storeValueRacing(n *wordNode, i int, value uint64) {
	if seqlockPlainStores {
		n.entries()[i].value = value
		return
	}
	atomic.StoreUint64(&n.entries()[i].value, value)
}

func storeTagsRacing(n *wordNode, tags [nodeSize]uint8) {
	if seqlockPlainStores {
		n.tags = tags
		return
	}
	n.storeTags(tags)
}

func storeKeyRacing(n *wordNode, i int, key uint64) {
	if seqlockPlainStores {
		n.keys[i] = key
		return
	}
	atomic.StoreUint64(&n.keys[i], key)
}

func storeChildRacing(n *wordNode, i int, child uint32) {
	if seqlockPlainStores {
		n.inner().children[i] = child
		return
	}
	atomic.StoreUint32(&n.inner().children[i], child)
}

func loadKeyRacing(n *wordNode, i int) uint64 {
	if seqlockPlainStores {
		return n.keys[i&(nodeSize-1)]
	}
	return atomic.LoadUint64(&n.keys[i&(nodeSize-1)])
}

func loadChildRacing(n *wordNode, i int) uint32 {
	if seqlockPlainStores {
		return n.inner().children[i&(nodeSize-1)]
	}
	return atomic.LoadUint32(&n.inner().children[i&(nodeSize-1)])
}

// routeRacing routes key through internal node n of count children read
// racing: the last child whose separator is at most key, or the first. acc
// carries the words that prove it; proven is false when they don't.
func routeRacing(n *wordNode, key uint64, count int) (i int, acc uint64, proven bool) {
	if seqlockPlainStores {
		return routeWindows(n, key, count)
	}
	return routeAtomic(n, key, count), 0, true
}

// routeWindows routes key by an internal node's 16-bit windows of its
// separators, as upstream's u64 nodes route by 2-byte inline keys: one line
// of windows instead of four of keys. Every separator shares the bits above
// shift+16 with the node's first (the prefix); a key outside that prefix
// goes to the first child or the last. Inside, the windows that bracket the
// key's are loaded again into acc, which proves the route in the validated
// snapshot however the search found it. A window equal to the key's leaves
// the full keys to decide (routeOrdered), as upstream's ties go to heads.
func routeWindows(n *wordNode, key uint64, count int) (i int, acc uint64, proven bool) {
	shift := uint(n.shift) & 63
	prefix := *n.prefixWord()
	acc = prefix ^ uint64(shift)
	projection := key >> shift
	low := (prefix >> shift) &^ 0xffff
	switch {
	case projection < low:
		return 0, acc, true
	case projection > low|0xffff:
		return count - 1, acc, true
	}
	window := uint16(projection)
	inline := &n.inner().inline
	i = max(0, upperWordInline(inline, count, window)-1)
	if i > 0 {
		below := inline[i&(nodeSize-1)]
		acc ^= uint64(below)
		if below >= window {
			if below > window {
				return i, acc, false
			}
			j, more, ok := routeOrdered(n, key, count)
			return j, acc ^ more, ok
		}
	}
	if i+1 < count {
		above := inline[(i+1)&(nodeSize-1)]
		acc ^= uint64(above)
		if above <= window {
			return i, acc, false
		}
	}
	return i, acc ^ uint64(i), true
}

// refitWindows derives internal node n's routing windows from its
// separators, while the node is held exclusively. Only plain-store builds
// route by them; elsewhere readers search the keys atomically.
func refitWindows(n *wordNode) {
	if !seqlockPlainStores {
		return
	}
	count := int(n.n)
	shift := 0
	if count > 0 {
		shift = max(0, bits.Len64(n.keys[0]^n.keys[count-1])-16)
	}
	n.shift = uint8(shift)
	*n.prefixWord() = n.keys[0]
	inline := &n.inner().inline
	for i, k := range n.keys {
		inline[i] = uint16(k >> shift)
	}
}

// routeOrdered routes key through an internal node with count children,
// with plain loads, to the last separator at most key. A search's result
// may depend on the words it read only through branches, which order
// nothing on arm64, so the separators that bracket key are loaded again and
// returned in acc: in the validated snapshot they prove the route, whatever
// the search read. proven is false when they do not bracket key.
func routeOrdered(n *wordNode, key uint64, count int) (i int, acc uint64, proven bool) {
	i = routeCode(&n.keys, count, key)
	acc = uint64(i)
	if i > 0 {
		low := n.keys[i&(nodeSize-1)]
		acc ^= low
		if low > key {
			return i, acc, false
		}
	}
	if i+1 < count {
		high := n.keys[(i+1)&(nodeSize-1)]
		acc ^= high
		if high <= key {
			return i, acc, false
		}
	}
	return i, acc, true
}

// routeAtomic is routeOrdered with atomic loads.
func routeAtomic(n *wordNode, key uint64, count int) int {
	// Only the count separators load; padding sorts above every key.
	var keys [nodeSize]uint64
	for i := range keys {
		keys[i] = ^uint64(0)
		if i < count {
			keys[i] = atomic.LoadUint64(&n.keys[i])
		}
	}
	return routeCode(&keys, count, key)
}

// findOrdered finds key among a leaf's count entries with plain loads, for
// a caller that folds the leaf's tag words into its validation. It returns
// key's slot, or the slot a miss stops at, the key loaded there, and in acc
// every other word it decided by. It proves a miss by loads the caller
// validates: no tag matches, the first matching slot holds a greater key, or
// the key below the lower bound is less. proven is false when those loads
// disagree.
func (n *wordNode) findOrdered(key uint64, count int) (i int, candidate, acc uint64, proven bool) {
	entries := n.entries()
	i = firstTag(&n.tags, count, key)
	if i >= count {
		return i, 0, 0, true
	}
	candidate = entries[i&(nodeSize-1)].key
	if candidate >= key {
		return i, candidate, 0, true
	}
	i = n.lowerEntry(key)
	if i > 0 {
		acc = entries[(i-1)&(nodeSize-1)].key
		if acc >= key {
			return i, candidate, acc, false
		}
	}
	if i < count {
		candidate = entries[i&(nodeSize-1)].key
		if candidate < key {
			return i, candidate, acc, false
		}
	}
	return i, candidate, acc, true
}

// findAtomic finds key among a leaf's count entries with atomic loads.
func (n *wordNode) findAtomic(key uint64, count int) (i int, found bool, value uint64) {
	entries := n.entries()
	i = firstSharedTag(n.tagWords(), count, key)
	var candidate uint64
	if i < count {
		candidate = atomic.LoadUint64(&entries[i].key)
		if candidate < key {
			lo, hi := 0, count
			for lo < hi {
				mid := int(uint(lo+hi) >> 1)
				if atomic.LoadUint64(&entries[mid].key) < key {
					lo = mid + 1
				} else {
					hi = mid
				}
			}
			i = lo
			if i < count {
				candidate = atomic.LoadUint64(&entries[i].key)
			}
		}
	}
	if i < count && candidate == key {
		return i, true, atomic.LoadUint64(&entries[i].value)
	}
	return i, false, 0
}

// Latch modes of a descent's leaf, upstream's LatchMode.
const (
	optimistic = iota
	shared
	exclusive
)

func latchLeaf(n *wordNode, mode int) uint64 {
	switch mode {
	case shared:
		n.lockShared()
		return 0
	case exclusive:
		return n.lockExclusive()
	}
	return n.readVersion()
}

func unlatchLeaf(n *wordNode, mode int, version uint64) {
	switch mode {
	case shared:
		n.unlockShared()
	case exclusive:
		n.unlockExclusive(version)
	}
}

// A descent's path, upstream's TreePath: each internal node it went
// through, the version it read it at, the edge it took and its length.
type pathLevel struct {
	n       *wordNode
	id      uint32
	version uint64
	edge    int
	count   int
	// The separators either side of the edge, the subtree's fences at this
	// level: low when edge > 0, high when edge+1 < count.
	low, high uint64
}

type treePath struct {
	levels [maxDepth]pathLevel
	depth  int
}

// descend finds key's leaf and latches it in mode: optimistic through the
// internal nodes, at each checking that the child was the node's child when
// its version was read, and at the leaf that its parent still routes key
// there. A root leaf checks the tree's root instead. Any failed check starts
// over from the root. path, if not nil, keeps the internal nodes. The tree
// has a root.
func (t *latchedTree) descend(key uint64, mode int, path *treePath) (leaf *wordNode, leafID uint32, version uint64) {
	leaf, leafID, version, _ = t.descendCarrying(key, mode, path)
	return leaf, leafID, version
}

// descendCarrying is descend, also returning the words an optimistic
// reader must fold into its leaf's check: the fences of a hinted leaf.
func (t *latchedTree) descendCarrying(key uint64, mode int, path *treePath) (leaf *wordNode, leafID uint32, version, carried uint64) {
restart:
	for {
		if path != nil {
			path.depth = 0
		}
		dir := t.dir()
		var cell *hintCell
		var routes *latchedRoutes
		if seqlockPlainStores && path == nil {
			// A descent that keeps no path may start at a hinted leaf
			// (word_latched_routes.go).
			routes = t.routes.Load()
			if routes == nil || routes.root != t.root.Load() {
				routes = t.refreshRoutes(dir)
			}
			if routes != nil {
				if cell = routes.cell(key); cell != nil {
					if hint := routes.leafFor(cell, key); hint != 0 {
						if c := nodeIn(dir, hint); c != nil {
							version = latchLeaf(c, mode)
							count, low, high := uint64(c.n), lowFence(c), highFence(c)
							if count != 0 && holds(low, high, key) {
								return c, hint, version, count ^ low ^ high
							}
							unlatchLeaf(c, mode, version)
						}
					}
				}
			}
		}
		id := t.root.Load()
		n := nodeIn(dir, id)
		if n == nil {
			continue
		}
		if id&wordLeafID != 0 {
			version = latchLeaf(n, mode)
			if t.root.Load() != id {
				unlatchLeaf(n, mode, version)
				continue
			}
			return n, id, version, 0
		}
		v := n.readVersion()
		if t.root.Load() != id {
			continue
		}
		var g *wordNode
		edge := 0
		for depth := 0; depth < maxDepth; depth++ {
			count := loadCountRacing(n)
			if count == 0 {
				continue restart
			}
			if depth == 0 && routes != nil && count >= 2*routes.rootCount {
				// The root doubled its children: the hints get finer.
				routes, cell = nil, nil
				t.refreshRoutes(dir)
			}
			i, acc, proven := routeRacing(n, key, count)
			if !proven {
				continue restart
			}
			child := loadChildRacing(n, i)
			var low, high uint64
			if path != nil {
				if i > 0 {
					low = loadKeyRacing(n, i)
				}
				if i+1 < count {
					high = loadKeyRacing(n, i+1)
				}
				acc ^= low ^ high
			}
			if !n.isUnchangedAfter(v, acc^uint64(child)) {
				continue restart
			}
			if path != nil {
				path.levels[path.depth] = pathLevel{n: n, id: id, version: v, edge: i, count: count, low: low, high: high}
				path.depth++
			}
			c := nodeIn(dir, child)
			if c == nil {
				continue restart
			}
			if child&wordLeafID != 0 {
				version = latchLeaf(c, mode)
				if !n.isUnchanged(v) {
					unlatchLeaf(c, mode, version)
					continue restart
				}
				if cell != nil {
					// A leaf parent fills the key's hint cell. Hints are
					// checked where used, so its reads need no check.
					routes.fill(dir, cell, key, n, g, edge)
				}
				return c, child, version, 0
			}
			cv := c.readVersion()
			if !n.isUnchanged(v) {
				continue restart
			}
			g, edge = n, i
			n, v, id = c, cv, child
		}
	}
}

// get is upstream's get_copied: an optimistic descent and a read of the
// leaf in place, settled once the leaf's version holds; a leaf that changed
// meanwhile sends the lookup the latched way, which waits for a writer
// rather than race it again.
func (t *latchedTree) get(key uint64) (uint64, bool) {
	if seqlockPlainStores {
		if value, found, ok := t.getHinted(key); ok {
			return value, found
		}
		if value, found, ok := t.getOrdered(key); ok {
			return value, found
		}
	}
	if t.root.Load() == 0 {
		return 0, false
	}
	leaf, _, version, carried := t.descendCarrying(key, optimistic, nil)
	count := loadCountRacing(leaf)
	if seqlockPlainStores {
		// The guarded words load after the leaf's version.
		n := after(leaf, version)
		tags := n.tagWords()
		acc := tags[0] ^ tags[1] ^ tags[2] ^ tags[3] ^ uint64(count) ^ carried
		i, candidate, read, proven := n.findOrdered(key, count)
		if proven && count <= nodeSize {
			acc ^= candidate ^ read ^ uint64(i)
			found := i < count && candidate == key
			var value uint64
			if found {
				value = n.entries()[i&(nodeSize-1)].value
				acc ^= value
			}
			if leaf.isUnchangedAfter(version, acc) {
				return value, found
			}
		}
	} else if count <= nodeSize {
		_, found, value := leaf.findAtomic(key, count)
		if leaf.isUnchanged(version) {
			return value, found
		}
	}
	return t.getLatched(key)
}

func (t *latchedTree) getLatched(key uint64) (value uint64, found bool) {
	leaf, _, _ := t.descend(key, shared, nil)
	if i := leaf.exactEntry(key); i < int(leaf.n) && leaf.entries()[i].key == key {
		value, found = leaf.entries()[i].value, true
	}
	leaf.unlockShared()
	return value, found
}

// read runs visit on key's entry while its leaf is latched shared.
func (t *latchedTree) read(key uint64, visit func(uint64, uint64)) bool {
	if t.root.Load() == 0 {
		return false
	}
	leaf, _, _ := t.descend(key, shared, nil)
	defer leaf.unlockShared()
	i := leaf.exactEntry(key)
	if i == int(leaf.n) || leaf.entries()[i].key != key {
		return false
	}
	visit(key, leaf.entries()[i].value)
	return true
}

// update stores change's result as key's value, with its leaf latched
// exclusively.
func (t *latchedTree) update(key uint64, change func(key, value uint64) uint64) (uint64, bool) {
	if t.root.Load() == 0 {
		return 0, false
	}
	leaf, _, version := t.descend(key, exclusive, nil)
	defer leaf.unlockExclusive(version)
	i := leaf.exactEntry(key)
	if i == int(leaf.n) || leaf.entries()[i].key != key {
		return 0, false
	}
	value := change(key, leaf.entries()[i].value)
	storeValueRacing(leaf, i, value)
	return value, true
}

// isEmpty reports that the tree held no entry at some moment during the
// call: its root is a leaf without entries. A root below the leaves always
// has entries under it.
func (t *latchedTree) isEmpty() bool {
	for {
		id := t.root.Load()
		if id == 0 {
			return true
		}
		if id&wordLeafID == 0 {
			return false
		}
		n := t.node(id)
		version := n.readVersion()
		empty := loadCountRacing(n) == 0
		loadFence()
		if n.isUnchanged(version) && t.root.Load() == id {
			return empty
		}
	}
}
