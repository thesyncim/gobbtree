package gobbtree

import (
	"sync/atomic"
	"unsafe"
)

// What an insert does where the tree holds the key already, upstream's
// InsertPolicy: replace the value (insert), store merge's result
// (update_or_insert), or keep it (get_or_insert).
const (
	insertReplace = iota
	insertMerge
	insertKeep
)

// insert inserts key and value, or, where the tree holds key, does what
// policy says; it returns the stored value and whether key was there.
// As upstream's (tree/latched/insert.rs): an append to the last leaf, if the
// key goes after every key there and the leaf has room; otherwise a descent
// to the key's leaf, latched exclusively, which takes the key if it has room;
// otherwise the leaf splits.
func (t *latchedTree) insert(key, value uint64, policy int, merge func(key, old, value uint64) uint64) (uint64, bool) {
	if leaf, leafID, version, i, ok := t.upgradeHinted(key); ok {
		// A hint settles most keys; only a key no hint names may append.
		old, found, done := insertAtReleasing(leaf, version, i, key, value, policy, merge)
		if done {
			leaf.unlockExclusive(version)
			return old, found
		}
		// The leaf is full: split it through its parent, still held.
		if t.splitAtParentHint(leaf, leafID, version, key, value) {
			return 0, false
		}
		leaf.unlockExclusive(version)
		return t.insertSplitting(key, value, policy, merge)
	}
	if t.root.Load() == 0 {
		t.start()
	}
	if appended, full := t.append(key, value); appended {
		return 0, false
	} else if full {
		return t.insertSplitting(key, value, policy, merge)
	}
	if old, found, done := t.insertLatched(key, value, policy, merge); done {
		return old, found
	}
	return t.insertSplitting(key, value, policy, merge)
}

// insertHeld is insertIntoAt on leaf, held exclusively from version, which
// it lets go of.
func insertHeld(leaf *wordNode, version uint64, i int, key, value uint64, policy int, merge func(key, old, value uint64) uint64) (old uint64, found, done bool) {
	defer leaf.unlockExclusive(version)
	return insertIntoAt(leaf, i, key, value, policy, merge)
}

func (t *latchedTree) insertLatched(key, value uint64, policy int, merge func(key, old, value uint64) uint64) (old uint64, found, done bool) {
	leaf, version, i, ok := t.upgrade(key)
	if !ok {
		leaf, _, version = t.descend(key, exclusive, nil)
		i = leaf.exactEntry(key)
	}
	return insertHeld(leaf, version, i, key, value, policy, merge)
}

// upgrade reads key's leaf optimistically, searches it, and then takes it
// exclusively at the version it read, by a compare-and-swap, as a split
// takes internal nodes: the search then holds, and its loads overlapped
// the latch's instead of following it. ok is false if a thread changed or
// held the leaf meanwhile.
func (t *latchedTree) upgrade(key uint64) (leaf *wordNode, version uint64, i int, ok bool) {
	if !seqlockPlainStores {
		// Racing plain searches need plain-store builds.
		return nil, 0, 0, false
	}
	leaf, _, version, i, ok = t.upgradeOrdered(key)
	return leaf, version, i, ok
}

// exactEntryRacing is exactEntry for a leaf read while a writer may change
// it: it reads the count once and keeps every index in bounds. The caller's
// version check, or compare-and-swap at the version it read, decides
// whether the answer holds.
func (n *wordNode) exactEntryRacing(key uint64) int {
	count := min(int(n.n), nodeSize)
	i := firstTag(&n.tags, count, key)
	if i >= count || n.entries()[i&(nodeSize-1)].key == key {
		return min(i, count)
	}
	return n.lowerEntry(key)
}

// insertIntoAt inserts into leaf, held exclusively, at i, leaf.exactEntry(key),
// if it holds key or has room; done is false when the leaf is full and must
// split.
func insertIntoAt(leaf *wordNode, i int, key, value uint64, policy int, merge func(key, old, value uint64) uint64) (old uint64, found, done bool) {
	if i < int(leaf.n) && leaf.entries()[i].key == key {
		old = leaf.entries()[i].value
		switch policy {
		case insertReplace:
			storeValueRacing(leaf, i, value)
		case insertMerge:
			storeValueRacing(leaf, i, merge(key, old, value))
		}
		return old, true, true
	}
	if leaf.n == nodeSize {
		return 0, false, false
	}
	if i == int(leaf.n) && leaf.n > 0 && key < leaf.entries()[leaf.n-1].key {
		i = leaf.lowerEntry(key)
	}
	insertFit(leaf, i, key, value)
	return 0, false, true
}

// append is upstream's (tree/latched/append.rs, D45): an insert whose key
// goes after every key, into a last leaf with room, latches that leaf and
// inserts there without a descent. Only a thread holding the last leaf
// exclusively changes the pointer to it, so a leaf the pointer still names
// once latched is last, and stays so while held.
func (t *latchedTree) append(key, value uint64) (appended, full bool) {
	var id uint32
	var n *wordNode
	if seqlockPlainStores {
		// Plain looks: the latch's compare-and-swap below settles them.
		id = *(*uint32)(unsafe.Pointer(&t.rightmost))
		if d := plainPointer(&t.blocks); d != nil {
			n = nodeIn(*d, id)
		}
	} else {
		id = t.rightmost.Load()
		n = t.node(id)
	}
	if n == nil {
		return false, false
	}
	// A racing look first, so that a key that doesn't go last takes no latch.
	count := loadCountRacing(n)
	if count == 0 || count > nodeSize || key <= loadEntryKeyRacing(n, count-1) {
		return false, false
	}
	var version uint64
	locked := false
	if seqlockPlainStores {
		if word := *(*uint64)(unsafe.Pointer(&n.sequence)); word&(latchExclusive|latchSharedMask) == 0 && n.tryLockAt(word) {
			version, locked = word, true
		}
	}
	if !locked {
		version = n.lockExclusive()
	}
	if count == nodeSize {
		// The last leaf must split: through its parent's hint if it can,
		// else the caller goes straight to splitting.
		if t.rightmost.Load() == id && n.n == nodeSize && key > n.entries()[nodeSize-1].key &&
			t.splitAtParentHint(n, id, version, key, value) {
			return true, false
		}
		n.unlockExclusive(version)
		return false, true
	}
	ok := t.rightmost.Load() == id && n.n > 0 && n.n < nodeSize && key > n.entries()[n.n-1].key
	if ok {
		insertFit(n, int(n.n), key, value)
	}
	n.unlockExclusive(version)
	return ok, false
}

func loadEntryKeyRacing(n *wordNode, i int) uint64 {
	if seqlockPlainStores {
		return n.entries()[i&(nodeSize-1)].key
	}
	return atomic.LoadUint64(&n.entries()[i&(nodeSize-1)].key)
}

// insertSplitting is insert where the leaf was full: it searches again,
// keeping the internal nodes it goes through, and splits, until no other
// thread holds a node the split needs.
func (t *latchedTree) insertSplitting(key, value uint64, policy int, merge func(key, old, value uint64) uint64) (uint64, bool) {
	for {
		var path treePath
		leaf, leafID, version := t.descend(key, exclusive, &path)
		if path.depth > 0 {
			// Its parent, for the next split's hint; the leaf is held.
			leaf.directory = path.levels[path.depth-1].id
		}
		old, found, done := insertIntoReleasing(leaf, version, key, value, policy, merge)
		if done {
			leaf.unlockExclusive(version)
			return old, found
		}
		// A key past a full leaf's last splits it nearly full instead, as
		// an ascending run brings more keys after it.
		appending := key > leaf.entries()[nodeSize-1].key
		if !appending && t.redistribute(&path, leaf, key, value) {
			leaf.unlockExclusive(version)
			return 0, false
		}
		if t.split(&path, leaf, leafID, version, key, value) {
			return 0, false
		}
		leaf.unlockExclusive(version)
		// Another thread holds a node the split needs: wait a moment, as
		// upstream's Interleaving::wait(1).
		spinWait()
	}
}

// redistributionSlack is how much room a full leaf's sibling must have for
// an insert to move entries over instead of splitting.
const redistributionSlack = 8

// redistribute makes room in leaf, full and held exclusively, by moving
// entries to a sibling under the same parent with room for at least
// redistributionSlack more, and inserts key and value into whichever of the
// two then holds its place, so the pair ends evenly filled: leaves stay
// denser than splitting alone leaves them. It takes the parent at the
// version the descent read and the sibling without waiting; false, with
// nothing changed, if it can't, or no sibling has the room.
func (t *latchedTree) redistribute(path *treePath, leaf *wordNode, key, value uint64) bool {
	if path.depth == 0 {
		return false
	}
	level := path.levels[path.depth-1]
	parent, edge := level.n, level.edge
	var held heldNodes
	// Unless it moves entries, it changes nothing it took.
	defer held.restore()
	if !held.takeAt(parent, level.version) {
		return false
	}
	children := &parent.inner().children
	best, bestLen := -1, nodeSize-redistributionSlack+1
	for _, s := range [2]int{edge - 1, edge + 1} {
		if s >= 0 && s < int(parent.n) {
			// A look before latching it, which the latch settles.
			if n := loadCountRacing(t.node(children[s])); n < bestLen {
				best, bestLen = s, n
			}
		}
	}
	if best < 0 {
		return false
	}
	sibling := t.node(children[best])
	if !held.takeLeaf(sibling) || int(sibling.n) > nodeSize-redistributionSlack {
		return false
	}
	left, right, leftEdge := leaf, sibling, edge
	if best < edge {
		left, right, leftEdge = sibling, leaf, best
	}
	leftCount, rightCount := int(left.n), int(right.n)
	rank := leftCount + right.lowerEntry(key)
	if key < right.entries()[0].key {
		rank = left.lowerEntry(key)
	}
	// The pair ends with cut entries left of the separator, the new one
	// among them if it ranks below cut; before of the old ones stay left.
	cut := (leftCount + rightCount + 1) / 2
	before := cut
	if rank < cut {
		before--
	}
	if before < leftCount {
		moveEntries(right, 0, left, before, leftCount-before)
	} else if before > leftCount {
		moveEntries(left, leftCount, right, 0, before-leftCount)
	}
	if rank < cut {
		insertFit(left, rank, key, value)
	} else {
		insertFit(right, rank-before, key, value)
	}
	separator := right.entries()[0].key
	storeKeyRacing(parent, leftEdge+1, separator)
	refitWindows(parent)
	setHighFence(left, separator)
	setLeafFences(right, separator, highFence(right))
	held.unlock()
	return true
}

// insertIntoReleasing is insertAtReleasing at leaf.exactEntry(key).
func insertIntoReleasing(leaf *wordNode, version, key, value uint64, policy int, merge func(key, old, value uint64) uint64) (old uint64, found, done bool) {
	return insertAtReleasing(leaf, version, leaf.exactEntry(key), key, value, policy, merge)
}

// insertAtReleasing is insertIntoAt that lets go of leaf if merge panics.
func insertAtReleasing(leaf *wordNode, version uint64, i int, key, value uint64, policy int, merge func(key, old, value uint64) uint64) (old uint64, found, done bool) {
	if policy != insertMerge {
		// Only a merge runs the caller's code.
		return insertIntoAt(leaf, i, key, value, policy, merge)
	}
	finished := false
	defer func() {
		if !finished {
			leaf.unlockExclusive(version)
		}
	}()
	old, found, done = insertIntoAt(leaf, i, key, value, policy, merge)
	finished = true
	return old, found, done
}

// heldNodes are the nodes an operation takes besides its leaf, upstream's
// HeldNodes (tree/latched/held.rs): internal nodes by a compare-and-swap at
// a version, leaves exclusively, all without waiting. They are let go of
// together, each at its next version.
type heldNodes struct {
	nodes    [3*maxDepth + 4]*wordNode
	versions [3*maxDepth + 4]uint64
	len      int
}

func (h *heldNodes) holds(n *wordNode) bool {
	for i := 0; i < h.len; i++ {
		if h.nodes[i] == n {
			return true
		}
	}
	return false
}

func (h *heldNodes) push(n *wordNode, version uint64) {
	h.nodes[h.len], h.versions[h.len] = n, version
	h.len++
}

// takeAt takes internal node n if it is still at version, the one the caller
// read it at; false if it changed or another thread holds it.
func (h *heldNodes) takeAt(n *wordNode, version uint64) bool {
	if h.holds(n) {
		return true
	}
	if !n.tryLockAt(version) {
		return false
	}
	h.push(n, version)
	return true
}

// takeInternal takes internal node n at the version it is at, unless another
// thread holds it: for a node reached through nodes the caller holds.
func (h *heldNodes) takeInternal(n *wordNode) bool {
	if h.holds(n) {
		return true
	}
	version, ok := n.tryReadVersion()
	return ok && h.takeAt(n, version)
}

// takeLeaf latches leaf exclusively, unless another thread holds it.
func (h *heldNodes) takeLeaf(n *wordNode) bool {
	if h.holds(n) {
		return true
	}
	version, ok := n.tryLockExclusive()
	if !ok {
		return false
	}
	h.push(n, version)
	return true
}

func (h *heldNodes) unlock() {
	for i := 0; i < h.len; i++ {
		h.nodes[i].unlockExclusive(h.versions[i])
	}
	h.len = 0
}

// restore lets go of nodes taken but left unchanged, at the versions they
// were taken at: a reader that read one meanwhile failed its check, and one
// that reads it after finds it as it was. An operation that then takes them
// at those versions, as a split takes its path, still can.
func (h *heldNodes) restore() {
	for i := 0; i < h.len; i++ {
		h.nodes[i].sequence.Store(h.versions[i])
	}
	h.len = 0
}

// ofSplit is upstream's InsertionSide::of_split (tree/node/mod.rs): where a
// full node of capacity entries splits for an insertion at index. An append
// at either end leaves the arriving side twice the minimum, so ordered
// inserts fill nodes densely; an interior insertion divides the free space
// evenly. The left half keeps entries [0, cut), the right takes the rest,
// and the new entry goes left at index when index <= cut, else right at
// index - cut.
func ofSplit(capacity, index, minimum int) (cut int, left bool) {
	arriving := min(2*minimum, capacity/2)
	switch {
	case index == 0:
		cut = arriving - 1
	case index == capacity:
		cut = capacity - arriving
	default:
		cut = capacity / 2
	}
	return cut, index <= cut
}

// split splits leaf, the full leaf path leads to, held exclusively, and
// inserts key and value. As upstream's (tree/latched/insert.rs): the lowest
// node on the path with room takes the separator, and every node below it
// splits; without one, the root splits, and the tree grows. The split takes
// those path nodes at the versions the descent read; if another thread holds
// one or changed it, it changes nothing, lets go of them and reports false.
// Otherwise it lets go of everything, leaf included.
func (t *latchedTree) split(path *treePath, leaf *wordNode, leafID uint32, leafVersion, key, value uint64) bool {
	depth := path.depth
	room := -1
	for level := depth - 1; level >= 0; level-- {
		if path.levels[level].count < nodeSize {
			room = level
			break
		}
	}
	top := max(room, 0)
	var held heldNodes
	for level := top; level < depth; level++ {
		if !held.takeAt(path.levels[level].n, path.levels[level].version) {
			held.unlock()
			return false
		}
	}

	var fresh heldNodes
	rightID, right, version := t.alloc(true)
	fresh.push(right, version)
	index := leaf.lowerEntry(key)
	// A key past the leaf's last leaves it nearly full: the new leaf takes
	// only the minimum, as an ascending run refills it.
	minimum := leafMinLen
	if index == nodeSize {
		minimum = leafMinLen / 2
	}
	cut, left := ofSplit(nodeSize, index, minimum)
	moveEntries(right, 0, leaf, cut, nodeSize-cut)
	if left {
		insertFit(leaf, index, key, value)
	} else {
		insertFit(right, index-cut, key, value)
	}
	if t.rightmost.Load() == leafID {
		t.rightmost.Store(rightID)
	}
	separator, carried := right.entries()[0].key, rightID
	setLeafFences(right, separator, highFence(leaf))
	setHighFence(leaf, separator)
	for level := depth - 1; level >= top && carried != 0; level-- {
		n, edge := path.levels[level].n, path.levels[level].edge+1
		if int(n.n) < nodeSize {
			insertChildFit(n, edge, separator, carried)
			if level == depth-1 {
				right.directory = path.levels[level].id
			}
			carried = 0
			break
		}
		splitID, split, version := t.alloc(false)
		fresh.push(split, version)
		split.level = n.level
		cut, left := ofSplit(nodeSize, edge, innerMinLen)
		moveChildren(split, 0, n, cut, nodeSize-cut)
		setHighFence(split, highFence(n))
		setHighFence(n, split.keys[0])
		if left {
			insertChildFit(n, edge, separator, carried)
		} else {
			insertChildFit(split, edge-cut, separator, carried)
		}
		if level == depth-1 {
			// The leaves' parents, for the next splits' hints.
			leaf.directory, right.directory = path.levels[level].id, path.levels[level].id
			if edge-1 >= cut {
				leaf.directory = splitID
			}
			if !left {
				right.directory = splitID
			}
		}
		separator, carried = split.keys[0], splitID
	}
	if carried != 0 {
		// The root split: a new root takes the old one and its new sibling.
		rootID, root, version := t.alloc(false)
		fresh.push(root, version)
		root.level = 1
		if old := t.node(t.root.Load()); !old.leaf {
			root.level = old.level + 1
		}
		insertChildFit(root, 0, 0, t.root.Load())
		insertChildFit(root, 1, separator, carried)
		if depth == 0 {
			leaf.directory, right.directory = rootID, rootID
		}
		t.root.Store(rootID)
	}
	held.unlock()
	leaf.unlockExclusive(leafVersion)
	fresh.unlock()
	return true
}

// Node changes, made with the node held exclusively, in the stores that
// optimistic readers race with (word_latched.go).

// insertFit inserts an entry at i into leaf n, which has room.
func insertFit(n *wordNode, i int, key, value uint64) {
	count := int(n.n)
	if seqlockPlainStores {
		entries := n.entries()
		copy(entries[i+1:count+1], entries[i:count])
		entries[i] = wordEntry{key, value}
		copy(n.tags[i+1:count+1], n.tags[i:count])
		n.tags[i] = uint8(key)
		n.n++
		return
	}
	entries := n.entries()
	for j := count; j > i; j-- {
		storeEntryRacing(n, j, entries[j-1])
	}
	storeEntryRacing(n, i, wordEntry{key, value})
	tags := n.tags
	copy(tags[i+1:count+1], tags[i:count])
	tags[i] = uint8(key)
	storeTagsRacing(n, tags)
	storeCountRacing(n, count+1)
}

// removeAt removes leaf n's entry i and returns its value.
func removeAt(n *wordNode, i int) uint64 {
	count := int(n.n)
	entries := n.entries()
	value := entries[i].value
	if seqlockPlainStores {
		copy(entries[i:count-1], entries[i+1:count])
		entries[count-1] = wordEntry{key: ^uint64(0)}
		copy(n.tags[i:count-1], n.tags[i+1:count])
		n.tags[count-1] = 0
		n.n--
		return value
	}
	for j := i; j < count-1; j++ {
		storeEntryRacing(n, j, entries[j+1])
	}
	storeEntryRacing(n, count-1, wordEntry{key: ^uint64(0)})
	tags := n.tags
	copy(tags[i:count-1], tags[i+1:count])
	tags[count-1] = 0
	storeTagsRacing(n, tags)
	storeCountRacing(n, count-1)
	return value
}

// moveEntries moves count entries of leaf src from from to the end of leaf
// dst at at, which has room: src keeps those before from, followed by those
// after the run.
func moveEntries(dst *wordNode, at int, src *wordNode, from, count int) {
	srcCount, dstCount := int(src.n), int(dst.n)
	se, de := src.entries(), dst.entries()
	dstTags, srcTags := dst.tags, src.tags
	// Make room in dst at at.
	for j := dstCount - 1; j >= at; j-- {
		storeEntryRacing(dst, j+count, de[j])
	}
	copy(dstTags[at+count:dstCount+count], dstTags[at:dstCount])
	for j := 0; j < count; j++ {
		storeEntryRacing(dst, at+j, se[from+j])
		dstTags[at+j] = srcTags[from+j]
	}
	// Close the gap in src and pad the slots freed.
	for j := from; j+count < srcCount; j++ {
		storeEntryRacing(src, j, se[j+count])
	}
	copy(srcTags[from:srcCount-count], srcTags[from+count:srcCount])
	for j := srcCount - count; j < srcCount; j++ {
		storeEntryRacing(src, j, wordEntry{key: ^uint64(0)})
		srcTags[j] = 0
	}
	storeTagsRacing(dst, dstTags)
	storeTagsRacing(src, srcTags)
	storeCountRacing(dst, dstCount+count)
	storeCountRacing(src, srcCount-count)
}

// insertChildFit inserts child, whose keys are at least separator, at i in
// internal node n, which has room.
func insertChildFit(n *wordNode, i int, separator uint64, child uint32) {
	count := int(n.n)
	children := &n.inner().children
	for j := count; j > i; j-- {
		storeKeyRacing(n, j, n.keys[j-1])
		storeChildRacing(n, j, children[j-1])
	}
	storeKeyRacing(n, i, separator)
	storeChildRacing(n, i, child)
	storeCountRacing(n, count+1)
	refitWindows(n)
}

// removeChild removes internal node n's child i.
func removeChild(n *wordNode, i int) {
	count := int(n.n)
	children := &n.inner().children
	for j := i; j < count-1; j++ {
		storeKeyRacing(n, j, n.keys[j+1])
		storeChildRacing(n, j, children[j+1])
	}
	storeKeyRacing(n, count-1, ^uint64(0))
	storeChildRacing(n, count-1, 0)
	storeCountRacing(n, count-1)
	refitWindows(n)
}

// moveChildren moves count children of internal node src from from, with
// their separators, to dst at at, as moveEntries moves entries.
func moveChildren(dst *wordNode, at int, src *wordNode, from, count int) {
	srcCount, dstCount := int(src.n), int(dst.n)
	sc, dc := &src.inner().children, &dst.inner().children
	for j := dstCount - 1; j >= at; j-- {
		storeKeyRacing(dst, j+count, dst.keys[j])
		storeChildRacing(dst, j+count, dc[j])
	}
	for j := 0; j < count; j++ {
		storeKeyRacing(dst, at+j, src.keys[from+j])
		storeChildRacing(dst, at+j, sc[from+j])
	}
	for j := from; j+count < srcCount; j++ {
		storeKeyRacing(src, j, src.keys[j+count])
		storeChildRacing(src, j, sc[j+count])
	}
	for j := srcCount - count; j < srcCount; j++ {
		storeKeyRacing(src, j, ^uint64(0))
		storeChildRacing(src, j, 0)
	}
	storeCountRacing(dst, dstCount+count)
	storeCountRacing(src, srcCount-count)
	refitWindows(dst)
	refitWindows(src)
}
