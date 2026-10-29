package gobbtree

// latchSet is the nodes an operation takes besides its leaf, upstream's
// HeldNodes, for the generic tree: internal nodes by a compare-and-swap at
// a version, leaves exclusively, all without waiting.
type latchSet struct {
	nodes    [3*maxDepth + 4]*gheader
	versions [3*maxDepth + 4]uint64
	len      int
}

func (s *latchSet) holds(h *gheader) bool {
	for i := 0; i < s.len; i++ {
		if s.nodes[i] == h {
			return true
		}
	}
	return false
}

func (s *latchSet) push(h *gheader, version uint64) {
	s.nodes[s.len], s.versions[s.len] = h, version
	s.len++
}

// takeAt takes h if it is still at version; false if it changed or is held.
func (s *latchSet) takeAt(h *gheader, version uint64) bool {
	if s.holds(h) {
		return true
	}
	if !h.latch.tryLockAt(version) {
		return false
	}
	s.push(h, version)
	return true
}

// takeInternal takes internal node h at the version it is at, unless held.
func (s *latchSet) takeInternal(h *gheader) bool {
	if s.holds(h) {
		return true
	}
	version, ok := h.latch.tryReadVersion()
	return ok && s.takeAt(h, version)
}

// takeLeaf latches leaf h exclusively, unless any goroutine holds it.
func (s *latchSet) takeLeaf(h *gheader) bool {
	if s.holds(h) {
		return true
	}
	version, ok := h.latch.tryLockExclusive()
	if !ok {
		return false
	}
	s.push(h, version)
	return true
}

func (s *latchSet) unlock() {
	for i := 0; i < s.len; i++ {
		s.nodes[i].latch.unlockExclusive(s.versions[i])
	}
	s.len = 0
}

// insert inserts key and value, or, where the tree holds key, does what
// policy says; it returns the stored value and whether key was there. As
// latchedTree.insert: the key's leaf, latched exclusively, takes it if it
// has room; otherwise the leaf splits.
func (t *genericTree[K, V, O]) insert(key K, value V, policy int, merge func(K, V, V) V) (V, bool) {
	if t.root.Load() == nil {
		t.start()
	}
	if old, found, done := t.insertLatched(key, value, policy, merge); done {
		return old, found
	}
	return t.insertSplitting(key, value, policy, merge)
}

func (t *genericTree[K, V, O]) insertLatched(key K, value V, policy int, merge func(K, V, V) V) (old V, found, done bool) {
	n, version := t.descend(key, seekKey, false, exclusive, nil)
	defer n.latch.unlockExclusive(version)
	return t.insertInto(n, key, value, policy, merge)
}

// insertInto inserts into leaf n, held exclusively, if it holds key or has
// room; done is false when the leaf is full and must split.
func (t *genericTree[K, V, O]) insertInto(n *gleaf[K, V], key K, value V, policy int, merge func(K, V, V) V) (old V, found, done bool) {
	i, ok := t.find(n, key)
	if ok {
		old = n.values[i]
		switch policy {
		case insertReplace:
			n.values[i] = value
		case insertMerge:
			n.values[i] = merge(n.keys[i], old, value)
		}
		return old, true, true
	}
	if int(n.count.Load()) == nodeSize {
		return old, false, false
	}
	insertEntry(n, i, ownKey[O](key), value)
	return old, false, true
}

// insertIntoReleasing is insertInto that lets go of n if merge panics.
func (t *genericTree[K, V, O]) insertIntoReleasing(n *gleaf[K, V], version uint64, key K, value V, policy int, merge func(K, V, V) V) (old V, found, done bool) {
	finished := false
	defer func() {
		if !finished {
			n.latch.unlockExclusive(version)
		}
	}()
	old, found, done = t.insertInto(n, key, value, policy, merge)
	finished = true
	return old, found, done
}

// insertSplitting is insert where the leaf was full: it searches again,
// keeping its path, and splits, until no other goroutine holds a node the
// split needs.
func (t *genericTree[K, V, O]) insertSplitting(key K, value V, policy int, merge func(K, V, V) V) (V, bool) {
	for {
		var path gpath[K]
		n, version := t.descend(key, seekKey, false, exclusive, &path)
		old, found, done := t.insertIntoReleasing(n, version, key, value, policy, merge)
		if done {
			n.latch.unlockExclusive(version)
			return old, found
		}
		if t.split(&path, n, version, key, value) {
			return old, false
		}
		n.latch.unlockExclusive(version)
		// Another goroutine holds a node the split needs: wait a moment,
		// as upstream's Interleaving::wait(1).
		spinWait()
	}
}

// split splits leaf, full and held, and inserts key and value, as
// latchedTree.split: the lowest node on the path with room takes the
// separator, every node below it splits, and without one the root splits.
// It takes those path nodes at the versions the descent read; false, with
// nothing changed, if one changed or is held. Otherwise it lets go of
// everything, leaf included.
func (t *genericTree[K, V, O]) split(path *gpath[K], leaf *gleaf[K, V], leafVersion uint64, key K, value V) bool {
	depth := path.depth
	room := -1
	for level := depth - 1; level >= 0; level-- {
		if path.levels[level].count < nodeSize {
			room = level
			break
		}
	}
	top := max(room, 0)
	var held latchSet
	for level := top; level < depth; level++ {
		if !held.takeAt(&path.levels[level].n.gheader, path.levels[level].version) {
			held.unlock()
			return false
		}
	}
	var fresh latchSet
	right := newGLeaf[K, V]()
	fresh.push(&right.gheader, 0)
	index, _ := t.find(leaf, key)
	// A key past the leaf's last leaves it nearly full, as an ascending
	// run refills the new leaf.
	minimum := leafMinLen
	if index == nodeSize {
		minimum = leafMinLen / 2
	}
	cut, left := ofSplit(nodeSize, index, minimum)
	moveLeafEntries(right, 0, leaf, cut, nodeSize-cut)
	if left {
		insertEntry(leaf, index, ownKey[O](key), value)
	} else {
		insertEntry(right, index-cut, ownKey[O](key), value)
	}
	separator := new(K)
	*separator = right.keys[0]
	carried := &right.gheader
	for level := depth - 1; level >= top && carried != nil; level-- {
		n, edge := path.levels[level].n, path.levels[level].edge+1
		if int(n.count.Load()) < nodeSize {
			t.insertChild(n, edge, separator, carried)
			carried = nil
			break
		}
		split := newGInner[K]()
		fresh.push(&split.gheader, 0)
		cut, left := ofSplit(nodeSize, edge, innerMinLen)
		moveChildEntries(split, 0, n, cut, nodeSize-cut)
		if left {
			t.insertChild(n, edge, separator, carried)
		} else {
			t.insertChild(split, edge-cut, separator, carried)
		}
		separator, carried = split.seps[0].Load(), &split.gheader
	}
	if carried != nil {
		// The root split: a new root takes the old one and its new sibling.
		root := newGInner[K]()
		fresh.push(&root.gheader, 0)
		t.insertChild(root, 0, nil, t.root.Load())
		t.insertChild(root, 1, separator, carried)
		t.root.Store(&root.gheader)
	}
	held.unlock()
	leaf.latch.unlockExclusive(leafVersion)
	fresh.unlock()
	return true
}

// Node changes, made with the node held exclusively. Racing readers read
// only counts, codes, separators and children, all atomically; keys and
// values are read with their leaf latched.

// insertEntry inserts an entry at i into leaf n, which has room.
func insertEntry[K any, V any](n *gleaf[K, V], i int, key K, value V) {
	count := int(n.count.Load())
	copy(n.keys[i+1:count+1], n.keys[i:count])
	copy(n.values[i+1:count+1], n.values[i:count])
	n.keys[i], n.values[i] = key, value
	n.count.Store(int32(count + 1))
}

// removeEntry removes leaf n's entry i, clearing the slot it frees so that
// the collector can take what it held.
func removeEntry[K any, V any](n *gleaf[K, V], i int) (K, V) {
	count := int(n.count.Load())
	key, value := n.keys[i], n.values[i]
	copy(n.keys[i:count-1], n.keys[i+1:count])
	copy(n.values[i:count-1], n.values[i+1:count])
	var zk K
	var zv V
	n.keys[count-1], n.values[count-1] = zk, zv
	n.count.Store(int32(count - 1))
	return key, value
}

// moveLeafEntries moves count entries of src from from to dst at at, which
// has room, closing src's gap and clearing the slots it frees.
func moveLeafEntries[K any, V any](dst *gleaf[K, V], at int, src *gleaf[K, V], from, count int) {
	srcCount, dstCount := int(src.count.Load()), int(dst.count.Load())
	copy(dst.keys[at+count:dstCount+count], dst.keys[at:dstCount])
	copy(dst.values[at+count:dstCount+count], dst.values[at:dstCount])
	copy(dst.keys[at:at+count], src.keys[from:from+count])
	copy(dst.values[at:at+count], src.values[from:from+count])
	copy(src.keys[from:srcCount-count], src.keys[from+count:srcCount])
	copy(src.values[from:srcCount-count], src.values[from+count:srcCount])
	clear(src.keys[srcCount-count : srcCount])
	clear(src.values[srcCount-count : srcCount])
	dst.count.Store(int32(dstCount + count))
	src.count.Store(int32(srcCount - count))
}

// insertChild inserts child, whose keys are at least separator, at i in
// internal node n, which has room.
func (t *genericTree[K, V, O]) insertChild(n *ginner[K], i int, separator *K, child *gheader) {
	count := int(n.count.Load())
	for j := count; j > i; j-- {
		n.codes[j].Store(n.codes[j-1].Load())
		n.seps[j].Store(n.seps[j-1].Load())
		n.children[j].Store(n.children[j-1].Load())
	}
	var code uint64
	if separator != nil {
		code = windowKey[O](*separator, 0)
	}
	n.codes[i].Store(code)
	n.seps[i].Store(separator)
	n.children[i].Store(child)
	n.count.Store(int32(count + 1))
}

// removeGChild removes internal node n's child i.
func removeGChild[K any](n *ginner[K], i int) {
	count := int(n.count.Load())
	for j := i; j < count-1; j++ {
		n.codes[j].Store(n.codes[j+1].Load())
		n.seps[j].Store(n.seps[j+1].Load())
		n.children[j].Store(n.children[j+1].Load())
	}
	n.codes[count-1].Store(0)
	n.seps[count-1].Store(nil)
	n.children[count-1].Store(nil)
	n.count.Store(int32(count - 1))
}

// moveChildEntries moves count children of src from from, with their
// separators and codes, to dst at at, as moveLeafEntries moves entries.
func moveChildEntries[K any](dst *ginner[K], at int, src *ginner[K], from, count int) {
	srcCount, dstCount := int(src.count.Load()), int(dst.count.Load())
	for j := dstCount - 1; j >= at; j-- {
		dst.codes[j+count].Store(dst.codes[j].Load())
		dst.seps[j+count].Store(dst.seps[j].Load())
		dst.children[j+count].Store(dst.children[j].Load())
	}
	for j := 0; j < count; j++ {
		dst.codes[at+j].Store(src.codes[from+j].Load())
		dst.seps[at+j].Store(src.seps[from+j].Load())
		dst.children[at+j].Store(src.children[from+j].Load())
	}
	for j := from; j+count < srcCount; j++ {
		src.codes[j].Store(src.codes[j+count].Load())
		src.seps[j].Store(src.seps[j+count].Load())
		src.children[j].Store(src.children[j+count].Load())
	}
	for j := srcCount - count; j < srcCount; j++ {
		src.codes[j].Store(0)
		src.seps[j].Store(nil)
		src.children[j].Store(nil)
	}
	dst.count.Store(int32(dstCount + count))
	src.count.Store(int32(srcCount - count))
}
