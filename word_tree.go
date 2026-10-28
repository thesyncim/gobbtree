package gobbtree

import (
	"math/bits"
	"sync/atomic"
	"unsafe"
)

// All fields are numbers. Slabs containing these nodes are GC noscan objects;
// child and sibling links are indexes, not hidden Go pointers.
type wordNode struct {
	n                     uint8
	leaf                  bool
	shift                 uint8
	level                 uint8
	directory, prev, next uint32
	tags                  [nodeSize]uint8
	sequence              atomic.Uint64
	mu                    storeLock
	payload               [nodeSize]uint64
	keys                  [nodeSize]uint64
}

type wordEntry struct{ key, value uint64 }

func (n *wordNode) entries() *[nodeSize]wordEntry {
	// The two adjacent 256-byte numeric columns form the 512-byte leaf
	// view. Its entries contain no pointers and have the same alignment.
	return (*[nodeSize]wordEntry)(unsafe.Add(unsafe.Pointer(n), unsafe.Offsetof(wordNode{}.payload)))
}
func (n *wordNode) keyAt(i int) uint64 {
	if n.leaf {
		return n.entries()[i].key
	}
	return n.keys[i]
}
func (n *wordNode) lowerEntry(key uint64) int { return lowerEntries(n.entries(), key) }

// Unused slots hold MaxUint64, which is never below a key, so every leaf
// search may cover all slots. Writers that lower a count pad the slots freed.
func lowerEntriesScalar(entries *[nodeSize]wordEntry, key uint64) int {
	lo, hi := 0, nodeSize
	for lo < hi {
		mid := int(uint(lo+hi)>>1) & (nodeSize - 1)
		if entries[mid].key < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
func (n *wordNode) exactEntry(key uint64) int {
	i := firstTag(&n.tags, int(n.n), key)
	if i == int(n.n) || n.entries()[i].key == key {
		return i
	}
	return n.lowerEntry(key)
}

// The ordered 16-bit column fits the existing node allocation. Leaves retain
// their byte hash tags; they do not pay for an extra search column.
type wordInner struct {
	children [nodeSize]uint32
	inline   [nodeSize]uint16
	_        [nodeSize * 2]byte
}

// The union contains only integers. Both views occupy exactly 256 bytes;
// uint64 alignment satisfies uint32 alignment. Slabs never move or resize.
// No Go pointer is stored in, or hidden from the GC by, this conversion.
func (n *wordNode) inner() *wordInner { return (*wordInner)(unsafe.Pointer(&n.payload)) }

var _ [unsafe.Sizeof(wordInner{}) - unsafe.Sizeof([nodeSize]uint64{})]byte
var _ [unsafe.Sizeof([nodeSize]uint64{}) - unsafe.Sizeof(wordInner{})]byte

type wordTree struct {
	root, free              uint32
	freeInner               uint32
	shared, atomicWrites    bool
	readBlocks              []atomic.Pointer[wordNode]
	rootNode                *wordNode
	slabs                   []*wordNode
	leafCursor, innerCursor uint32
	directories             [][256]uint8
	freeDirectories         []uint32
	keyKind, valueKind      uint8
	routes                  wordRoutes
	routesDirty             bool
	routeNode               *wordNode
	// routesTop is the highest route bucket filled since the last clearing.
	routesTop int
	// tail is the last leaf inserted into while it was the rightmost, for
	// appends past the largest key: by Tree, or by a Map's serial writers
	// before readers. A split that adds a leaf after it fails its next link
	// check; release clears it.
	tail *wordNode
	// A deletion that leaves ancestors unchanged can be undone without
	// another descent. Every following mutation consumes or invalidates it.
	deletedLeaf  *wordNode
	deletedKey   uint64
	deletedIndex int
	// The last key inserted. A full leaf whose insertion point follows it
	// is in an ascending run. Shared trees' leaf writers set it concurrently;
	// a stale key only changes how a leaf splits.
	inserted       uint64
	insertedShared atomic.Uint64
}

// noteInsert records key as the last key inserted.
func (t *wordTree) noteInsert(key uint64) {
	if !t.shared {
		t.inserted = key
	} else {
		t.insertedShared.Store(key)
	}
}
func (t *wordTree) lastInserted() uint64 {
	if !t.shared {
		return t.inserted
	}
	return t.insertedShared.Load()
}

func (t *wordTree) node(id uint32) *wordNode {
	id = (id &^ wordLeafID) - 1
	// IDs name initialized slots in stable Go allocations. The base remains a
	// traced pointer; the resulting pointer stays inside that same allocation.
	return wordSlabNode(t.slabs[id>>8], id)
}
func wordSlabNode(base *wordNode, index uint32) *wordNode {
	return (*wordNode)(unsafe.Add(unsafe.Pointer(base), uintptr(index&255)*unsafe.Sizeof(wordNode{})))
}

// Allocation grows through 1, 2, ... 128 slots; every later slab has 256.
// Keep this rule shared by allocation and optimistic-reader bounds checks.
func wordSlabSize(index int) int { return 1 << min(index, 8) }

func (t *wordTree) alloc(leaf bool) uint32 {
	var id uint32
	free := &t.freeInner
	if leaf {
		free = &t.free
	}
	reused := *free != 0
	if reused {
		id = *free
		*free = t.node(id).directory
	} else {
		// Keep the initial growing slabs compact. Full-size slabs group
		// internal nodes separately from leaves to improve page locality.
		separate := len(t.slabs) > 8 || len(t.slabs) == 8 && t.leafCursor&511 == 128
		cursor := &t.leafCursor
		if separate && !leaf {
			cursor = &t.innerCursor
		}
		// Nine low bits hold the number of used slots, including 256.
		slab, used := int(*cursor>>9), int(*cursor&511)
		if len(t.slabs) == 0 || separate && slab < 8 || used == wordSlabSize(slab) {
			if len(t.slabs) >= (1<<23)-1 {
				panic("gobbtree: numeric arena exhausted")
			}
			nodes := make([]wordNode, wordSlabSize(len(t.slabs)))
			t.slabs = append(t.slabs, &nodes[0])
			if t.shared {
				t.readBlocks = appendWordBlock(t.readBlocks, nodes)
			}
			slab, used = len(t.slabs)-1, 0
			*cursor = uint32(slab) << 9
		}
		id = (uint32(slab)<<8 | uint32(used)) + 1
		*cursor++
	}
	if leaf {
		id |= wordLeafID
	}
	n := t.node(id)
	// Once optimistic reads start, shared slots keep their kind and mutex.
	// Before that barrier, the structural lock excludes every reader and writer.
	if t.shared && t.atomicWrites {
		n.n, n.leaf, n.shift, n.level, n.directory = 0, leaf, 0, 0, 0
		if leaf {
			n.prev, n.next = 0, 0
		} else {
			atomic.StoreUint64(n.prefixWord(), 0)
		}
		for i := range n.keys {
			atomic.StoreUint64(&n.keys[i], 0)
		}
		if leaf {
			for i := range n.payload {
				atomic.StoreUint64(&n.payload[i], 0)
			}
		} else {
			for i := 0; i < nodeSize; i++ {
				t.storeChild(n, i, 0)
			}
			clear(n.inner().inline[:])
		}
		n.storeTags([nodeSize]uint8{})
		n.sequence.Store(0)
	} else if reused {
		*n = wordNode{leaf: leaf}
	} else {
		n.leaf = leaf
	}
	if !t.atomicWrites {
		if leaf {
			n.padLeafPlain(0, nodeSize)
		} else {
			for i := range n.keys {
				n.keys[i] = ^uint64(0)
			}
		}
	} else if leaf {
		for i := range n.entries() {
			t.storeEntry(n, i, wordEntry{key: ^uint64(0)})
		}
	} else {
		for i := range n.keys {
			t.storeKey(n, i, ^uint64(0))
		}
	}
	return id
}
func (t *wordTree) release(id uint32) {
	n := t.node(id)
	if n == t.tail {
		t.tail = nil
	}
	if n.directory != 0 {
		t.freeDirectories = append(t.freeDirectories, n.directory)
	}
	free := &t.freeInner
	if n.leaf {
		free = &t.free
	}
	n.directory = *free
	*free = id
}

// Updating an adjacent pair of interior separators leaves the count, key
// domain, padding, and compression shifts unchanged. Update just its summaries.
func (t *wordTree) refitSeparators(n *wordNode, first, last int) {
	if first == 0 || last == int(n.n)-1 {
		t.refit(n)
		return
	}
	t.invalidateRoutes(n)
	if n.directory != 0 {
		refillDirectory(&t.directories[n.directory-1], &n.keys, first, last)
	}
	inner := n.inner()
	for i := first; i <= last; i++ {
		inner.inline[i] = uint16(n.keys[i] >> n.shift)
	}
}

func (t *wordTree) invalidateRoutes(n *wordNode) {
	if !n.leaf && t.rootNode != nil && (n.level == t.rootNode.level || n.level+t.routes.depth > t.rootNode.level) {
		t.routesDirty = true
		if t.routeNode == nil || n.level > t.routeNode.level {
			t.routeNode = n
		}
	}
}

func (t *wordTree) refit(n *wordNode) {
	count := int(n.n)
	if n.leaf && !t.atomicWrites {
		n.refitLeafPlain(count)
		if t.shared {
			// publishNumeric's leaf summary: shift zero and the new count.
			sequence := n.sequence.Load()
			n.sequence.Store(sequence&^(wordSequenceStep-2) | uint64(count)<<1)
		}
		return
	}
	t.invalidateRoutes(n)
	if n.leaf {
		for i := 0; i < count; i++ {
			if !t.shared {
				n.tags[i] = uint8(n.keyAt(i))
			}
		}
		for i := count; i < nodeSize; i++ {
			t.storeEntry(n, i, wordEntry{key: ^uint64(0)})
			if !t.shared {
				n.tags[i] = 0
			}
		}
		if t.shared {
			n.publishNumeric(t.atomicWrites)
		}
		return
	}
	// Internal routing reads the key, inline and directory columns. Only the
	// writer routes this way in a shared tree; its optimistic readers search
	// the keys.
	if t.atomicWrites {
		for i := count; i < nodeSize; i++ {
			t.storeKey(n, i, ^uint64(0))
		}
	} else {
		for i := count; i < nodeSize; i++ {
			n.keys[i] = ^uint64(0)
		}
	}

	n.shift = 0
	if count > 0 {
		n.shift = uint8(max(0, bits.Len64(n.keys[0]^n.keys[count-1])-16))
	}
	inner := n.inner()
	for i, k := range n.keys {
		inner.inline[i] = uint16(k >> n.shift)
	}
	*n.prefixWord() = n.keys[0]
	if count >= 4 && n.keys[count-1]>>56-n.keys[0]>>56 >= uint64(2*count) {
		if n.directory == 0 {
			if end := len(t.freeDirectories); end > 0 {
				n.directory = t.freeDirectories[end-1]
				t.freeDirectories = t.freeDirectories[:end-1]
			} else {
				t.directories = append(t.directories, [256]uint8{})
				n.directory = uint32(len(t.directories))
			}
		}
		fillDirectory(&t.directories[n.directory-1], &n.keys, count)
	} else if n.directory != 0 {
		t.freeDirectories = append(t.freeDirectories, n.directory)
		n.directory = 0
	}
	if t.shared {
		n.publishNumeric(t.atomicWrites)
	}
}

// Before optimistic readers start, nothing loads a leaf concurrently: derive
// its tags and pad the unused slots without per-slot mode checks.
func (n *wordNode) refitLeafPlain(count int) {
	entries := n.entries()
	tags := &n.tags
	for i := 0; i < count && i < nodeSize; i++ {
		tags[i] = uint8(entries[i].key)
	}
	for i := count; i < nodeSize; i++ {
		entries[i] = wordEntry{key: ^uint64(0)}
		tags[i] = 0
	}
}
func (t *wordTree) route(n *wordNode, key uint64) int {
	if n.directory != 0 {
		return directoryRoute(&n.keys, &t.directories[n.directory-1], int(n.n), key)
	}
	projection := key >> n.shift
	prefix := (*n.prefixWord() >> n.shift) &^ uint64(65535)
	if projection < prefix {
		return 0
	}
	if projection > prefix|65535 {
		return int(n.n) - 1
	}
	i := max(0, upperWordInline(&n.inner().inline, int(n.n), uint16(projection))-1)
	if i > 0 && n.inner().inline[i] == uint16(projection) && n.keys[i] > key {
		// This guard excludes MaxUint64, so the upper-bound key cannot wrap.
		i = max(0, lowerCodeScalar(&n.keys, i+1, key+1)-1)
	}
	return i
}
func (t *wordTree) leafFor(key uint64) uint32 {
	id := t.routes.lookup(key)
	if id == 0 {
		id = t.root
	}
	for {
		n := t.node(id)
		if n.leaf {
			return id
		}
		id = n.inner().children[t.route(n, key)]
	}
}

func (n *wordNode) insertLeafPlain(i int, key, value uint64) {
	count := int(n.n)
	entries := n.entries()
	copy(entries[i+1:count+1], entries[i:count])
	entries[i] = wordEntry{key, value}
	copy(n.tags[i+1:count+1], n.tags[i:count])
	n.tags[i] = uint8(key)
	n.n++
}

// appendLeafPlain adds key after a non-full leaf's entries, shifting none.
func (n *wordNode) appendLeafPlain(key, value uint64) {
	count := int(n.n) & (nodeSize - 1)
	n.entries()[count] = wordEntry{key, value}
	n.tags[count] = uint8(key)
	n.n++
}

func (t *wordTree) insertLeaf(n *wordNode, i int, key, value uint64) {
	t.noteInsert(key)
	if !t.atomicWrites {
		n.insertLeafPlain(i, key, value)
		if t.shared {
			n.sequence.Store(uint64(n.n) << 1)
		}
		return
	}
	if seqlockPlainStores {
		// Inside the caller's leaf write; keep the epoch and odd bits.
		n.insertLeafPlain(i, key, value)
		n.sequence.Store(n.sequence.Load()&^(wordSequenceStep-2) | uint64(n.n)<<1)
		return
	}
	count := int(n.n)
	t.moveEntries(n, i+1, i, count-i)
	t.storeEntry(n, i, wordEntry{key, value})
	n.n++
	if t.shared {
		if t.atomicWrites {
			n.insertTag(i, key)
		} else {
			copy(n.tags[i+1:count+1], n.tags[i:count])
			n.tags[i] = uint8(key)
			n.sequence.Store(uint64(n.n) << 1)
		}
	} else {
		copy(n.tags[i+1:count+1], n.tags[i:count])
		n.tags[i] = uint8(key)
	}
}

func (t *wordTree) insert(key, value uint64) (uint64, bool) {
	if !t.shared {
		t.deletedLeaf = nil
	}
	if t.root == 0 {
		t.root = t.alloc(true)
		t.rootNode = t.node(t.root)
	}
	id := t.leafFor(key)
	n := t.node(id)
	i := n.exactEntry(key)
	if i < int(n.n) && n.entries()[i].key == key {
		old := n.entries()[i].value
		if t.shared {
			atomic.StoreUint64(&n.entries()[i].value, value)
		} else {
			n.entries()[i].value = value
		}
		return old, true
	}
	return t.insertNew(n, i, key, value)
}

func (t *wordTree) insertNew(n *wordNode, i int, key, value uint64) (uint64, bool) {
	if n.n < nodeSize {
		if i == int(n.n) && n.n > 0 && key < n.entries()[n.n-1].key {
			i = n.lowerEntry(key)
		}
		if i > 0 {
			t.insertLeaf(n, i, key, value)
			return 0, false
		}
	}
	// A cached leaf parent can absorb a split or a new separator without
	// visiting ancestors, provided its own minimum and capacity stay valid.
	if !t.shared {
		if id := t.routes.lookup(key); id != 0 {
			parent := t.node(id)
			if parent.n < nodeSize && key >= parent.keys[0] {
				t.insertAbsent(id, key, value)
				if t.routesDirty {
					t.rebuildRoutes()
				}
				return 0, false
			}
		}
	}
	if t.node(t.root).n == nodeSize {
		left := t.root
		right := t.split(left)
		t.root = t.alloc(false)
		t.rootNode = t.node(t.root)
		t.insertChild(t.node(t.root), 0, left)
		t.insertChild(t.node(t.root), 1, right)
	}
	t.insertAbsent(t.root, key, value)
	if t.routesDirty {
		t.rebuildRoutes()
	}
	return 0, false
}

// The shared insertion path can start at a cached leaf parent while holding
// the structural lock. Recheck the key after acquiring that lock.
func (t *wordTree) insertSharedAtRoute(key, value uint64) bool {
	id := t.routes.lookup(key)
	if id == 0 {
		return false
	}
	parent := t.node(id)
	for parent.level > 1 {
		parent = t.node(parent.inner().children[t.route(parent, key)])
	}
	if parent.n == nodeSize {
		return false
	}
	index := t.route(parent, key)
	leaf := t.node(parent.inner().children[index])
	if leaf.n != nodeSize || key < leaf.entries()[0].key {
		return false
	}
	position := leaf.exactEntry(key)
	if position < int(leaf.n) && leaf.entries()[position].key == key {
		return false
	}
	t.makeLeafRoom(parent, index, key, value)
	if t.routesDirty {
		t.rebuildRoutes()
	}
	return true
}

// The caller holds the structural lock, reached leaf through parent's child
// index, and found key absent. Insert into a full leaf, or below an interior
// separator, without another descent; otherwise report false.
func (t *wordTree) insertAtParent(parent *wordNode, index int, leaf *wordNode, key, value uint64) bool {
	if leaf.n == nodeSize {
		if parent.n == nodeSize || key < leaf.entries()[0].key {
			return false
		}
		t.makeLeafRoom(parent, index, key, value)
	} else {
		if index == 0 || leaf.n == 0 || key > leaf.entries()[0].key {
			return false
		}
		t.insertLeaf(leaf, 0, key, value)
		t.storeKey(parent, index, key)
		t.refitSeparators(parent, index, index)
	}
	if t.routesDirty {
		t.rebuildRoutes()
	}
	return true
}

func (t *wordTree) insertAbsent(id uint32, key, value uint64) {
	n := t.node(id)
	if n.leaf {
		t.insertLeaf(n, n.lowerEntry(key), key, value)
		return
	}
	i := t.route(n, key)
	child := n.inner().children[i]
	if t.node(child).n == nodeSize {
		if t.node(child).leaf && n.n > 1 {
			t.makeLeafRoom(n, i, key, value)
			return
		} else {
			right := t.split(child)
			t.insertChild(n, i+1, right)
			if key >= t.node(right).keyAt(0) {
				i++
				child = right
			}
		}
	}
	t.insertAbsent(child, key, value)
	if n.keys[i] != t.node(child).keyAt(0) {
		t.storeKey(n, i, t.node(child).keyAt(0))
		t.refit(n)
	}
}
func (t *wordTree) split(id uint32) uint32 {
	n := t.node(id)
	rightID := t.alloc(n.leaf)
	right := t.node(rightID)
	right.level = n.level
	if n.leaf {
		t.copyEntries(right, 0, n.entries()[minNodeSize:])
		right.next, right.prev = n.next, id
		if n.next != 0 {
			t.node(n.next).prev = rightID
		}
		n.next = rightID
	} else {
		t.copyKeys(right, 0, n.keys[minNodeSize:])
		t.copyChildren(right, 0, n.inner().children[minNodeSize:])
		t.clearChildren(n, minNodeSize)
	}
	n.n, right.n = minNodeSize, nodeSize-minNodeSize
	t.refit(n)
	t.refit(right)
	return rightID
}
func (t *wordTree) insertChild(n *wordNode, i int, child uint32) {
	n.level = t.node(child).level + 1
	count := int(n.n)
	t.moveKeys(n, i+1, i, count-i)
	t.moveChildren(n, i+1, i, count-i)
	t.storeKey(n, i, t.node(child).keyAt(0))
	t.storeChild(n, i, child)
	n.n++
	t.refit(n)
}
func (t *wordTree) removeChild(n *wordNode, i int) {
	count := int(n.n)
	t.moveKeys(n, i, i+1, count-i-1)
	t.moveChildren(n, i, i+1, count-i-1)
	t.storeChild(n, count-1, 0)
	n.n--
	t.refit(n)
}
func (t *wordTree) deleteOwner(key uint64) (uint64, bool) {
	if !t.shared {
		t.deletedLeaf = nil
	}
	if t.root == 0 {
		return 0, false
	}
	n := t.node(t.leafFor(key))
	i := n.exactEntry(key)
	if i == int(n.n) || n.entries()[i].key != key {
		return 0, false
	}
	// A nonminimum entry in a leaf above minimum occupancy can be removed
	// without changing any ancestor.
	if i > 0 && n.n > minNodeSize {
		value := t.removeLeaf(n, i)
		t.deletedLeaf, t.deletedKey, t.deletedIndex = n, key, i
		return value, true
	}
	return t.delete(key)
}

func (t *wordTree) delete(key uint64) (uint64, bool) {
	if !t.shared {
		t.deletedLeaf = nil
	}
	if t.root == 0 {
		return 0, false
	}
	value, ok := t.remove(t.root, key)
	if !ok {
		return value, false
	}
	root := t.node(t.root)
	if root.n == 0 {
		t.root, t.free, t.freeInner = 0, 0, 0
		t.leafCursor, t.innerCursor = 0, 0
		t.readBlocks = nil
		t.rootNode = nil
		t.tail = nil
		t.routes = wordRoutes{}
		t.routesDirty = false
		t.routeNode = nil
		t.slabs = nil
		t.directories = nil
		t.freeDirectories = nil
	} else if !root.leaf && root.n == 1 {
		old := t.root
		t.root = root.inner().children[0]
		t.rootNode = t.node(t.root)
		t.routesDirty = true
		t.release(old)
	}
	if t.routesDirty {
		t.rebuildRoutes()
	}
	return value, true
}
func (t *wordTree) remove(id uint32, key uint64) (uint64, bool) {
	n := t.node(id)
	if n.leaf {
		i := n.exactEntry(key)
		entries := n.entries()
		if i == int(n.n) || entries[i].key != key {
			return 0, false
		}
		return t.removeLeaf(n, i), true
	}

	i := t.route(n, key)
	child := n.inner().children[i]
	value, ok := t.remove(child, key)
	if !ok {
		return value, false
	}
	if t.node(child).n < minNodeSize {
		t.rebalance(n, i)
		for j := 0; j < int(n.n); j++ {
			t.storeKey(n, j, t.node(n.inner().children[j]).keyAt(0))
		}
		t.refit(n)
	} else if n.keys[i] != t.node(child).keyAt(0) {
		t.storeKey(n, i, t.node(child).keyAt(0))
		t.refit(n)
	}
	return value, true
}

// The caller has already found the entry and holds any required leaf latch.
func (t *wordTree) removeLeaf(n *wordNode, i int) uint64 {
	value := n.entries()[i].value
	t.moveEntries(n, i, i+1, int(n.n)-i-1)
	if !t.shared || t.plainWrites() {
		copy(n.tags[i:n.n-1], n.tags[i+1:n.n])
	}
	n.n--
	t.storeEntry(n, int(n.n), wordEntry{key: ^uint64(0)})
	if t.shared {
		if t.atomicWrites && seqlockPlainStores {
			// Inside the caller's leaf write; keep the epoch and odd bits.
			n.tags[n.n] = 0
			n.sequence.Store(n.sequence.Load()&^(wordSequenceStep-2) | uint64(n.n)<<1)
		} else if t.atomicWrites {
			n.deleteTag(i)
		} else {
			n.tags[n.n] = 0
			n.sequence.Store(uint64(n.n) << 1)
		}
	} else {
		n.tags[n.n] = 0
	}
	return value
}

func (t *wordTree) rebalance(parent *wordNode, i int) {
	children := &parent.inner().children
	child := t.node(children[i])
	if i > 0 && t.node(children[i-1]).n > minNodeSize {
		left := t.node(children[i-1])
		count := int(child.n)
		if child.leaf {
			t.moveEntries(child, 1, 0, count)
			t.storeEntry(child, 0, left.entries()[left.n-1])
		} else {
			t.moveKeys(child, 1, 0, count)
			t.storeKey(child, 0, left.keys[left.n-1])
			t.moveChildren(child, 1, 0, count)
			t.storeChild(child, 0, left.inner().children[left.n-1])
			t.storeChild(left, int(left.n)-1, 0)
		}
		left.n--
		child.n++
		t.refit(left)
		t.refit(child)
		return
	}
	if i+1 < int(parent.n) && t.node(children[i+1]).n > minNodeSize {
		right := t.node(children[i+1])
		if child.leaf {
			t.storeEntry(child, int(child.n), right.entries()[0])
			t.moveEntries(right, 0, 1, int(right.n)-1)
		} else {
			t.storeKey(child, int(child.n), right.keyAt(0))
			t.moveKeys(right, 0, 1, int(right.n)-1)
			t.storeChild(child, int(child.n), right.inner().children[0])
			t.moveChildren(right, 0, 1, int(right.n)-1)
			t.storeChild(right, int(right.n)-1, 0)
		}
		right.n--
		child.n++
		t.refit(right)
		t.refit(child)
		return
	}
	if i > 0 {
		i--
	}
	leftID, rightID := children[i], children[i+1]
	left, right := t.node(leftID), t.node(rightID)
	if left.leaf {
		t.copyEntries(left, int(left.n), right.entries()[:right.n])
		left.next = right.next
		if right.next != 0 {
			t.node(right.next).prev = leftID
		}
	} else {
		t.copyKeys(left, int(left.n), right.keys[:right.n])
		t.copyChildren(left, int(left.n), right.inner().children[:right.n])
	}
	left.n += right.n
	t.refit(left)
	t.removeChild(parent, i+1)
	t.release(rightID)
}

func (t *wordTree) end(last bool) uint32 {
	id := t.root
	for id != 0 {
		n := t.node(id)
		if n.leaf {
			break
		}
		i := 0
		if last {
			i = int(n.n) - 1
		}
		id = n.inner().children[i]
	}
	return id
}

// scan visits keys in [lower, upper), or at or above lower when open.
func (t *wordTree) scan(lower, upper uint64, open, reverse bool, visit func(uint64, uint64) bool) {
	if t.root == 0 || !open && lower >= upper {
		return
	}
	var id uint32
	var i int
	switch {
	case !reverse:
		id = t.leafFor(lower)
		i = t.node(id).lowerEntry(lower)
	case open:
		id = t.end(true)
		i = int(t.node(id).n) - 1
	default:
		id = t.leafFor(upper)
		i = t.node(id).lowerEntry(upper) - 1
	}
	for id != 0 {
		n := t.node(id)
		if reverse {
			for ; i >= 0; i-- {
				if n.entries()[i].key < lower || !visit(n.entries()[i].key, n.entries()[i].value) {
					return
				}
			}
			id = n.prev
			if id != 0 {
				i = int(t.node(id).n) - 1
			}
		} else {
			for ; i < int(n.n); i++ {
				if !open && n.entries()[i].key >= upper || !visit(n.entries()[i].key, n.entries()[i].value) {
					return
				}
			}
			id = n.next
			i = 0
		}
	}
}
func (t *wordTree) all(reverse bool, visit func(uint64, uint64) bool) {
	for id := t.end(reverse); id != 0; {
		n := t.node(id)
		if reverse {
			for i := int(n.n) - 1; i >= 0; i-- {
				if !visit(n.entries()[i].key, n.entries()[i].value) {
					return
				}
			}
			id = n.prev
		} else {
			for i := 0; i < int(n.n); i++ {
				if !visit(n.entries()[i].key, n.entries()[i].value) {
					return
				}
			}
			id = n.next
		}
	}
}
func (t *wordTree) getMany(keys []uint64, dst []Result[uint64]) {
	if t.root == 0 {
		clear(dst[:len(keys)])
		return
	}
	for start := 0; start < len(keys); start += batchLanes {
		count := min(batchLanes, len(keys)-start)
		var ids [batchLanes]uint32
		for j := 0; j < count; j++ {
			ids[j] = t.routes.lookup(keys[start+j])
			if ids[j] == 0 {
				ids[j] = t.root
				if len(t.routes.ids) != 0 {
					for step := 0; step < int(t.routes.depth); step++ {
						n := t.node(ids[j])
						ids[j] = n.inner().children[t.route(n, keys[start+j])]
					}
				}
			}
		}
		// Routed lanes may start at different levels.
		for descending := true; descending; {
			descending = false
			for j := 0; j < count; j++ {
				if n := t.node(ids[j]); !n.leaf {
					ids[j] = n.inner().children[t.route(n, keys[start+j])]
					descending = true
				}
			}
		}
		for j := 0; j < count; j++ {
			n := t.node(ids[j])
			key := keys[start+j]
			i := n.exactEntry(key)
			var result Result[uint64]
			if i < int(n.n) && n.entries()[i].key == key {
				result = Result[uint64]{n.entries()[i].value, true}
			}
			dst[start+j] = result
		}
	}
}

// Redistribute into a neighboring leaf, moving only the boundary entries.
func (t *wordTree) makeLeafRoom(parent *wordNode, index int, key, value uint64) {
	children := &parent.inner().children
	if leaf := t.node(children[index]); leaf.n == nodeSize {
		if i := leaf.lowerEntry(key); i > 0 && leaf.entries()[i-1].key == t.lastInserted() {
			t.splitRun(parent, index, i, key, value)
			return
		}
	}
	leftIndex := index
	if index > 0 && (index+1 == int(parent.n) || t.node(children[index-1]).n <= t.node(children[index+1]).n) {
		leftIndex--
	}
	leftID, rightID := children[leftIndex], children[leftIndex+1]
	left, right := t.node(leftID), t.node(rightID)
	borrowedOuter := false
	if int(left.n)+int(right.n) > 2*nodeSize-pairRedistributionSlack {
		borrowed := -1
		if t.shared && int(left.n)+int(right.n) == 2*nodeSize {
			borrowed = t.borrowOuterLeaf(parent, index)
		}
		if borrowed >= 0 {
			borrowedOuter = true
			leftIndex = borrowed
			left, right = t.node(children[leftIndex]), t.node(children[leftIndex+1])
		} else {
			t.splitLeafPair(parent, leftIndex, key, value)
			return
		}
	}
	leftCount, rightCount := int(left.n), int(right.n)
	rank := 0
	if key < right.entries()[0].key {
		rank = left.lowerEntry(key)
	} else {
		rank = leftCount + right.lowerEntry(key)
	}
	cut := (leftCount + rightCount + 1) / 2
	before := cut
	if rank < cut {
		before--
	}
	if !t.atomicWrites && !borrowedOuter {
		t.redistributePlain(left, right, before, rank, cut, key, value)
	} else {
		if before < leftCount {
			moved := leftCount - before
			t.moveEntries(right, moved, 0, rightCount)
			t.copyEntries(right, 0, left.entries()[before:leftCount])
		} else if before > leftCount {
			moved := before - leftCount
			t.copyEntries(left, leftCount, right.entries()[:moved])
			t.moveEntries(right, 0, moved, rightCount-moved)
		}
		left.n, right.n = uint8(before), uint8(leftCount+rightCount-before)
		target, position := left, rank
		if rank >= cut {
			target, position = right, rank-before
		}
		t.moveEntries(target, position+1, position, int(target.n)-position)
		t.storeEntry(target, position, wordEntry{key, value})
		target.n++
		t.refit(left)
		t.refit(right)
	}
	t.storeKey(parent, leftIndex, left.keyAt(0))
	t.storeKey(parent, leftIndex+1, right.keyAt(0))
	if borrowedOuter {
		// Borrowing can also change the outer sibling's separator.
		t.refit(parent)
	} else {
		t.refitSeparators(parent, leftIndex, leftIndex+1)
	}
}

// splitRun splits full leaf index where an ascending run inserts, at
// position i: the leaf keeps its entries before i and takes the key, and a
// new leaf after it takes the rest. The run then appends to the leaf until it
// is full, and the next split starts a new leaf for it. Balanced splits would
// leave every leaf of the run a third empty, and the tree a level deeper.
// Either new leaf may hold fewer than minNodeSize entries, but the two share
// nodeSize+1, so leaves still average at least half full whatever the order
// of keys; deletions rebalance them as any other.
func (t *wordTree) splitRun(parent *wordNode, index, i int, key, value uint64) {
	leafID := parent.inner().children[index]
	leaf := t.node(leafID)
	extraID := t.alloc(true)
	extra := t.node(extraID)
	if i == nodeSize {
		t.copyEntries(extra, 0, []wordEntry{{key, value}})
		extra.n = 1
	} else {
		t.copyEntries(extra, 0, leaf.entries()[i:nodeSize])
		extra.n = uint8(nodeSize - i)
		t.storeEntry(leaf, i, wordEntry{key, value})
		leaf.n = uint8(i + 1)
	}
	extra.prev, extra.next = leafID, leaf.next
	if leaf.next != 0 {
		t.node(leaf.next).prev = extraID
	}
	leaf.next = extraID
	// Past the leaf's last key, only its next link changed.
	if t.atomicWrites {
		if i < nodeSize {
			t.refit(leaf)
		}
		t.refit(extra)
	} else {
		if i < nodeSize {
			t.settleLeafPlain(leaf, nodeSize)
		}
		// The new leaf is already padded.
		t.settleLeafPlain(extra, 0)
	}
	t.insertChild(parent, index+1, extraID)
	t.noteInsert(key)
}

// appendFull appends key past the largest, which full rightmost leaf tail
// holds, by starting a new rightmost leaf beside it, as splitRun does. It
// reports false when tail's parent is full and the split needs ancestors.
func (t *wordTree) appendFull(tail *wordNode, key, value uint64) bool {
	parent := t.rootNode
	if parent.leaf {
		return false
	}
	for parent.level > 1 {
		parent = t.node(parent.inner().children[parent.n-1])
	}
	index := int(parent.n) - 1
	if parent.n == nodeSize || t.node(parent.inner().children[index]) != tail {
		return false
	}
	t.splitRun(parent, index, nodeSize, key, value)
	t.tail = t.node(parent.inner().children[index+1])
	return true
}

// Before optimistic readers start, a redistribution moves only the entries and
// tags that change leaves, and pads only the slots they vacate.
func (t *wordTree) redistributePlain(left, right *wordNode, before, rank, cut int, key, value uint64) {
	leftCount, rightCount := int(left.n), int(right.n)
	if before < leftCount {
		moved := leftCount - before
		copy(right.entries()[moved:], right.entries()[:rightCount])
		copy(right.tags[moved:], right.tags[:rightCount])
		copy(right.entries()[:moved], left.entries()[before:leftCount])
		copy(right.tags[:moved], left.tags[before:leftCount])
		left.padLeafPlain(before, leftCount)
	} else if before > leftCount {
		moved := before - leftCount
		copy(left.entries()[leftCount:], right.entries()[:moved])
		copy(left.tags[leftCount:], right.tags[:moved])
		copy(right.entries()[:], right.entries()[moved:rightCount])
		copy(right.tags[:], right.tags[moved:rightCount])
		right.padLeafPlain(rightCount-moved, rightCount)
	}
	left.n, right.n = uint8(before), uint8(leftCount+rightCount-before)
	if rank < cut {
		left.insertLeafPlain(rank, key, value)
	} else {
		right.insertLeafPlain(rank-before, key, value)
	}
	if t.shared {
		for _, n := range [2]*wordNode{left, right} {
			sequence := n.sequence.Load()
			n.sequence.Store(sequence&^(wordSequenceStep-2) | uint64(n.n)<<1)
		}
	}
}

// After a plain rewrite of a leaf's first n entries, derive their tags and pad
// only the slots beyond them that the leaf previously used.
func (t *wordTree) settleLeafPlain(n *wordNode, previous int) {
	entries := n.entries()
	count := int(n.n)
	for i := 0; i < count && i < nodeSize; i++ {
		n.tags[i] = uint8(entries[i].key)
	}
	n.padLeafPlain(count, previous)
	if t.shared {
		sequence := n.sequence.Load()
		n.sequence.Store(sequence&^(wordSequenceStep-2) | uint64(count)<<1)
	}
}
func (n *wordNode) padLeafPlain(from, to int) {
	entries := n.entries()
	for i := from; i < to && i < nodeSize; i++ {
		entries[i] = wordEntry{key: ^uint64(0)}
		n.tags[i] = 0
	}
}

// Move some free space across a full sibling before allocating another leaf.
// The adjacent pair is refitted by makeLeafRoom after the insertion.
func (t *wordTree) borrowOuterLeaf(parent *wordNode, index int) int {
	// Avoid repeatedly borrowing one or two slots from nearly full leaves.
	const maxBorrowCount = 3 * nodeSize / 4
	children := &parent.inner().children
	outside := -1
	if index >= 2 && t.node(children[index-2]).n <= maxBorrowCount {
		outside = index - 2
	}
	if index+2 < int(parent.n) && t.node(children[index+2]).n <= maxBorrowCount &&
		(outside < 0 || t.node(children[index+2]).n < t.node(children[outside]).n) {
		outside = index + 2
	}
	if outside < 0 {
		return -1
	}
	outer := t.node(children[outside])
	count := int(outer.n)
	// Balance the available space across all three leaves.
	moved := (2*(nodeSize-count) + 1) / 3
	pair := index
	var middle *wordNode
	if outside < index {
		middle = t.node(children[index-1])
		t.copyEntries(outer, count, middle.entries()[:moved])
		t.moveEntries(middle, 0, moved, nodeSize-moved)
		pair--
	} else {
		middle = t.node(children[index+1])
		t.moveEntries(outer, moved, 0, count)
		t.copyEntries(outer, 0, middle.entries()[nodeSize-moved:])
	}
	middle.n -= uint8(moved)
	// The caller searches this leaf before refitting it, and leaf searches
	// count padded unused slots.
	for i := int(middle.n); i < nodeSize; i++ {
		t.storeEntry(middle, i, wordEntry{key: ^uint64(0)})
	}
	outer.n += uint8(moved)
	t.refit(outer)
	t.storeKey(parent, outside, outer.keyAt(0))
	return pair
}

// Redistribute a full leaf only when its pair has this many free slots;
// otherwise split the pair into three. Smaller moves keep leaves denser but
// touch a cold sibling far more often; eight keeps construction fast at a
// modest density cost. Each of the three keeps at least the minimum occupancy.
const pairRedistributionSlack = 8

var _ [(2*nodeSize-pairRedistributionSlack+1)/3 - minNodeSize]struct{}

// Two nearly full leaves split into three; only this path needs a merged buffer.
func (t *wordTree) splitLeafPair(parent *wordNode, index int, key, value uint64) {
	leftID, rightID := parent.inner().children[index], parent.inner().children[index+1]
	left, right := t.node(leftID), t.node(rightID)
	leftCount, rightCount := int(left.n), int(right.n)
	total := leftCount + rightCount
	var entries [2*nodeSize + 1]wordEntry
	copy(entries[:leftCount], left.entries()[:leftCount])
	copy(entries[leftCount:total], right.entries()[:rightCount])
	position := leftCount + right.lowerEntry(key)
	if key < right.entries()[0].key {
		position = left.lowerEntry(key)
	}
	copy(entries[position+1:total+1], entries[position:total])
	entries[position] = wordEntry{key, value}
	total++
	extraID := t.alloc(true)
	extra := t.node(extraID)
	first, second := total/3, 2*total/3
	t.copyEntries(left, 0, entries[:first])
	t.copyEntries(right, 0, entries[first:second])
	t.copyEntries(extra, 0, entries[second:total])
	left.n, right.n, extra.n = uint8(first), uint8(second-first), uint8(total-second)
	extra.prev, extra.next = rightID, right.next
	if right.next != 0 {
		t.node(right.next).prev = extraID
	}
	right.next = extraID
	if t.atomicWrites {
		t.refit(left)
		t.refit(right)
		t.refit(extra)
	} else {
		// The new leaf is already padded.
		t.settleLeafPlain(left, leftCount)
		t.settleLeafPlain(right, rightCount)
		t.settleLeafPlain(extra, 0)
	}
	t.storeKey(parent, index, left.keyAt(0))
	t.storeKey(parent, index+1, right.keyAt(0))
	t.insertChild(parent, index+2, extraID)
}
