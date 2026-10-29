package gobbtree

import (
	"math/bits"
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
	// A latched tree's node latch (word_latch.go); a Tree's nodes leave it.
	sequence nodeLatch
	// A latched tree's node fence above its keys: the lower bound of the
	// next node's, 0 on the tree's right border (word_latched_routes.go).
	high    uint64
	payload [nodeSize]uint64
	keys    [nodeSize]uint64
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
	// appends past the largest key. A split that adds a leaf after it fails
	// its next link check; release clears it.
	tail *wordNode
	// A deletion that leaves ancestors unchanged can be undone without
	// another descent. Every following mutation consumes or invalidates it.
	deletedLeaf  *wordNode
	deletedKey   uint64
	deletedIndex int
	// The last key inserted. A full leaf whose insertion point follows it
	// is in an ascending run. A stale hint only changes how a leaf splits.
	inserted uint64
	hints    wordHints
}

// noteInsert records that key was inserted at position i of leaf n.
func (t *wordTree) noteInsert(n *wordNode, i int, key uint64) {
	t.inserted = key
}

// continuesRun reports that inserting at position i of full leaf n follows
// the last insertion, as an ascending run does.
func (t *wordTree) continuesRun(n *wordNode, i int) bool {
	return n.entries()[i-1].key == t.inserted
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
	if reused {
		*n = wordNode{leaf: leaf}
	} else {
		n.leaf = leaf
	}
	if leaf {
		n.padLeafPlain(0, nodeSize)
	} else {
		for i := range n.keys {
			n.keys[i] = ^uint64(0)
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
		// A freed leaf holds nothing, so that no hint finds a key in it.
		n.n = 0
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
	if n.leaf {
		n.refitLeafPlain(count)
		return
	}
	t.invalidateRoutes(n)
	// Internal routing reads the key, inline and directory columns.
	for i := count; i < nodeSize; i++ {
		n.keys[i] = ^uint64(0)
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

// leafFilling is leafFor that fills key's hint cell from its leaf's parent,
// as an insert's descent does.
func (t *wordTree) leafFilling(key uint64) uint32 {
	id := t.routes.lookup(key)
	if id == 0 {
		id = t.root
	}
	var parent, grand *wordNode
	edge, parentEdge := 0, 0
	for {
		n := t.node(id)
		if n.leaf {
			if parent != nil {
				t.hintParent(key, parent, grand, parentEdge)
			}
			return id
		}
		grand, parentEdge = parent, edge
		parent, edge = n, t.route(n, key)
		id = n.inner().children[edge]
	}
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
	t.noteInsert(n, i, key)
	n.insertLeafPlain(i, key, value)
}

func (t *wordTree) insert(key, value uint64) (uint64, bool) {
	t.deletedLeaf = nil
	if t.root == 0 {
		t.root = t.alloc(true)
		t.rootNode = t.node(t.root)
	}
	id := t.leafFor(key)
	n := t.node(id)
	i := n.exactEntry(key)
	if i < int(n.n) && n.entries()[i].key == key {
		old := n.entries()[i].value
		n.entries()[i].value = value
		return old, true
	}
	return t.insertNew(n, i, key, value)
}

func (t *wordTree) insertNew(n *wordNode, i int, key, value uint64) (uint64, bool) {
	if n.n < nodeSize {
		if i == int(n.n) && n.n > 0 && key < n.entries()[n.n-1].key {
			i = n.lowerEntry(key)
		}
		// Separators are lower bounds: only a new minimum of the leftmost
		// leaf changes one.
		if i > 0 || n.prev != 0 {
			t.insertLeaf(n, i, key, value)
			return 0, false
		}
	}
	// A cached leaf parent can absorb a split or a new separator without
	// visiting ancestors, provided its own minimum and capacity stay valid.
	{
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
	if minimum := t.node(child).keyAt(0); minimum < n.keys[i] {
		t.storeKey(n, i, minimum)
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

// Separators bound their subtrees' keys from below; an inner node's first
// separator is its own. Leaf writers may remove a leaf's minimum, or insert
// a new one above its separator, without touching any other node, so
// separators are never raised except where entries cross a boundary, and
// only where the leaves on both sides are locked. lowerKey lowers separator
// i to key, for a subtree whose minimum may only have dropped.
func (t *wordTree) lowerKey(n *wordNode, i int, key uint64) {
	if key < n.keys[i] {
		t.storeKey(n, i, key)
	}
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
	t.deletedLeaf = nil
	if t.root == 0 {
		return 0, false
	}
	// A hinted leaf that holds key is key's leaf: keys are unique.
	n := t.hintedLeaf(key)
	i := 0
	if n != nil && n.leaf {
		i = n.exactEntry(key)
	}
	if n == nil || !n.leaf || i == int(n.n) || n.entries()[i].key != key {
		n = t.node(t.leafFilling(key))
		i = n.exactEntry(key)
		if i == int(n.n) || n.entries()[i].key != key {
			return 0, false
		}
	}
	// An entry in a leaf above minimum occupancy can be removed without
	// changing any ancestor: separators are lower bounds. The leftmost
	// leaf's minimum is the tree's, which stays exact.
	if (i > 0 || n.prev != 0) && n.n > minNodeSize {
		value := t.removeLeaf(n, i)
		t.deletedLeaf, t.deletedKey, t.deletedIndex = n, key, i
		return value, true
	}
	return t.delete(key)
}

func (t *wordTree) delete(key uint64) (uint64, bool) {
	t.deletedLeaf = nil
	if t.root == 0 {
		return 0, false
	}
	value, ok := t.remove(t.root, key, true)
	if !ok {
		return value, false
	}
	root := t.node(t.root)
	if root.n == 0 {
		t.dropHints()
		t.root, t.free, t.freeInner = 0, 0, 0
		t.leafCursor, t.innerCursor = 0, 0
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

// remove deletes key from subtree id. leftmost reports that the subtree's
// first separator is the tree's minimum, which stays exact: route tables span
// from it.
func (t *wordTree) remove(id uint32, key uint64, leftmost bool) (uint64, bool) {
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
	value, ok := t.remove(child, key, leftmost && i == 0)
	if !ok {
		return value, false
	}
	changed := false
	if t.node(child).n < minNodeSize {
		t.rebalance(n, i)
		changed = true
	}
	if leftmost && i == 0 {
		if minimum := t.node(n.inner().children[0]).keyAt(0); minimum != n.keys[0] {
			t.storeKey(n, 0, minimum)
			changed = true
		}
	}
	if changed {
		t.refit(n)
	}
	return value, true
}

// The caller has already found the entry.
func (t *wordTree) removeLeaf(n *wordNode, i int) uint64 {
	value := n.entries()[i].value
	t.moveEntries(n, i, i+1, int(n.n)-i-1)
	copy(n.tags[i:n.n-1], n.tags[i+1:n.n])
	n.n--
	t.storeEntry(n, int(n.n), wordEntry{key: ^uint64(0)})
	n.tags[n.n] = 0
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
		t.storeKey(parent, i, child.keyAt(0))
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
		t.storeKey(parent, i+1, right.keyAt(0))
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
		if i := leaf.lowerEntry(key); i > 0 && t.continuesRun(leaf, i) {
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
	if int(left.n)+int(right.n) > 2*nodeSize-pairRedistributionSlack {
		t.splitLeafPair(parent, leftIndex, key, value)
		return
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
	t.redistributePlain(left, right, before, rank, cut, key, value)
	t.lowerKey(parent, leftIndex, left.keyAt(0))
	t.storeKey(parent, leftIndex+1, right.keyAt(0))
	t.refitSeparators(parent, leftIndex, leftIndex+1)
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
	if i < nodeSize {
		t.settleLeafPlain(leaf, nodeSize)
	}
	// The new leaf is already padded.
	t.settleLeafPlain(extra, 0)
	t.insertChild(parent, index+1, extraID)
	if i == nodeSize {
		t.noteInsert(extra, 0, key)
	} else {
		t.noteInsert(leaf, i, key)
	}
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
}
func (n *wordNode) padLeafPlain(from, to int) {
	entries := n.entries()
	for i := from; i < to && i < nodeSize; i++ {
		entries[i] = wordEntry{key: ^uint64(0)}
		n.tags[i] = 0
	}
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
	// The new leaf is already padded.
	t.settleLeafPlain(left, leftCount)
	t.settleLeafPlain(right, rightCount)
	t.settleLeafPlain(extra, 0)
	t.lowerKey(parent, index, left.keyAt(0))
	t.storeKey(parent, index+1, right.keyAt(0))
	t.insertChild(parent, index+2, extraID)
}
