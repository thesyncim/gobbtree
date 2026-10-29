package gobbtree

import "strings"

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
	if prefixed[O, K]() {
		t.admit(key)
	}
	code := t.code(key)
	if old, found, done := t.insertLatched(key, code, value, policy, merge); done {
		return old, found
	}
	return t.insertSplitting(key, code, value, policy, merge)
}

// admit makes the shared prefix one key begins with before the key goes in:
// the first key's sets it, and a key without it shortens it to what they
// share (D50). It only shortens, so a key that had it keeps it.
func (t *genericTree[K, V, O]) admit(key K) {
	s := textView(&key)
	for {
		cur := t.shared.Load()
		if cur == nil {
			// A copy: Bytes keys are the caller's to change.
			shared := strings.Clone(s[:min(len(s), 64)])
			if t.shared.CompareAndSwap(nil, &shared) {
				return
			}
			continue
		}
		p := commonPrefix(s, *cur)
		if p >= len(*cur) {
			return
		}
		t.shrinkShared(cur, p)
	}
}

// shrinkShared shortens the shared prefix cur to p bytes, as upstream's
// latched tree does: it takes the root and every internal node on the
// tree's borders, the nodes that take the shared prefix for a fence, without
// waiting, and refits them; another goroutine holding one makes it start
// over. It returns once the prefix is no longer cur.
func (t *genericTree[K, V, O]) shrinkShared(cur *string, p int) {
	var b backoff
	for t.shared.Load() == cur {
		var held latchSet
		root := t.root.Load()
		ok := false
		if root.leaf {
			ok = held.takeLeaf(root)
		} else {
			ok = held.takeInternal(root) && takeBorder(&held, asInner[K](root), false) && takeBorder(&held, asInner[K](root), true)
		}
		if ok && t.root.Load() == root && t.shared.Load() == cur {
			shorter := (*cur)[:p]
			t.shared.Store(&shorter)
			for i := 0; i < held.len; i++ {
				if n := held.nodes[i]; !n.leaf {
					t.refitInner(asInner[K](n))
				}
			}
			held.unlock()
			return
		}
		held.unlock()
		b.spin()
	}
}

// takeBorder takes the internal nodes below n, held, along its first
// children, or its last when right.
func takeBorder[K any](held *latchSet, n *ginner[K], right bool) bool {
	for {
		i := 0
		if right {
			i = int(n.count.Load()) - 1
		}
		c := n.children[max(i, 0)&(nodeSize-1)].Load()
		if c == nil || c.leaf {
			return c != nil
		}
		if !held.takeInternal(c) {
			return false
		}
		n = asInner[K](c)
	}
}

func (t *genericTree[K, V, O]) insertLatched(key K, code uint64, value V, policy int, merge func(K, V, V) V) (old V, found, done bool) {
	n, version := t.leafLatched(key, code, seekKey, exclusive)
	defer n.latch.unlockExclusive(version)
	return t.insertInto(n, key, code, value, policy, merge)
}

// insertInto inserts into leaf n, held exclusively, if it holds key, of
// code code, or has room; done is false when the leaf is full and must split.
func (t *genericTree[K, V, O]) insertInto(n *gleaf[K, V], key K, code uint64, value V, policy int, merge func(K, V, V) V) (old V, found, done bool) {
	i, ok := t.find(n, key, code)
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
	t.insertEntry(n, i, ownKey[O](key), value)
	return old, false, true
}

// insertIntoReleasing is insertInto that lets go of n if merge panics.
func (t *genericTree[K, V, O]) insertIntoReleasing(n *gleaf[K, V], version uint64, key K, code uint64, value V, policy int, merge func(K, V, V) V) (old V, found, done bool) {
	finished := false
	defer func() {
		if !finished {
			n.latch.unlockExclusive(version)
		}
	}()
	old, found, done = t.insertInto(n, key, code, value, policy, merge)
	finished = true
	return old, found, done
}

// insertSplitting is insert where the leaf was full: it searches again,
// keeping its path, and splits, until no other goroutine holds a node the
// split needs.
func (t *genericTree[K, V, O]) insertSplitting(key K, code uint64, value V, policy int, merge func(K, V, V) V) (V, bool) {
	for {
		var path gpath[K]
		n, version := t.descend(key, code, seekKey, false, exclusive, &path)
		old, found, done := t.insertIntoReleasing(n, version, key, code, value, policy, merge)
		if done {
			n.latch.unlockExclusive(version)
			return old, found
		}
		if t.split(&path, n, version, key, code, value) {
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
func (t *genericTree[K, V, O]) split(path *gpath[K], leaf *gleaf[K, V], leafVersion uint64, key K, code uint64, value V) bool {
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
	right := newGLeaf[K, V](prefixed[O, K]())
	fresh.push(&right.gheader, 0)
	index, _ := t.find(leaf, key, code)
	// A key past the leaf's last leaves it nearly full, as an ascending
	// run refills the new leaf.
	minimum := leafMinLen
	if index == nodeSize {
		minimum = leafMinLen / 2
	}
	cut, left := ofSplit(nodeSize, index, minimum)
	t.moveLeafEntries(right, 0, leaf, cut, nodeSize-cut)
	if left {
		t.insertEntry(leaf, index, ownKey[O](key), value)
	} else {
		t.insertEntry(right, index-cut, ownKey[O](key), value)
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
		high := n.high.Load()
		cut, left := ofSplit(nodeSize, edge, innerMinLen)
		t.moveChildEntries(split, 0, n, cut, nodeSize-cut)
		if left {
			t.insertChild(n, edge, separator, carried)
		} else {
			t.insertChild(split, edge-cut, separator, carried)
		}
		separator, carried = split.seps[0].Load(), &split.gheader
		// The halves' fences: the separator between them, and n's.
		n.high.Store(separator)
		split.high.Store(high)
		t.refitInner(n)
		t.refitInner(split)
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

// Node changes, made with the node held exclusively. Racing readers of
// internal nodes read only words that can't tear, atomically; readers of
// leaves read them latched, or racing where a version check covers them.

// insertEntry inserts an entry at i into leaf n, which has room; a key
// without the leaf's prefix shortens it.
func (t *genericTree[K, V, O]) insertEntry(n *gleaf[K, V], i int, key K, value V) {
	p, refit := 0, false
	if prefixed[O, K]() {
		p = int(n.prefix.Load())
		refit = p > 0 && t.leafOrder(n, key, p) != 0
	}
	count := int(n.count.Load())
	copy(n.codes[i+1:count+1], n.codes[i:count])
	copy(n.keys[i+1:count+1], n.keys[i:count])
	copy(n.values[i+1:count+1], n.values[i:count])
	n.codes[i], n.keys[i], n.values[i] = windowKey[O](key, p), key, value
	if prefixed[O, K]() {
		copy(n.text().tails[i+1:count+1], n.text().tails[i:count])
		copy(n.text().lens[i+1:count+1], n.text().lens[i:count])
		s := textView(&key)
		n.text().tails[i], n.text().lens[i] = stringCode(s, p+8), textLen(s)
	}
	n.count.Store(int32(count + 1))
	if refit {
		t.refitLeaf(n)
	}
}

// removeEntry removes leaf n's entry i, clearing the slot it frees so that
// the collector can take what it held. An emptied leaf has no prefix: no key
// is left to share one.
func (t *genericTree[K, V, O]) removeEntry(n *gleaf[K, V], i int) (K, V) {
	count := int(n.count.Load())
	key, value := n.keys[i], n.values[i]
	copy(n.codes[i:count-1], n.codes[i+1:count])
	copy(n.keys[i:count-1], n.keys[i+1:count])
	copy(n.values[i:count-1], n.values[i+1:count])
	var zk K
	var zv V
	n.codes[count-1], n.keys[count-1], n.values[count-1] = codePad, zk, zv
	if prefixed[O, K]() {
		c := n.text()
		copy(c.tails[i:count-1], c.tails[i+1:count])
		copy(c.lens[i:count-1], c.lens[i+1:count])
		c.tails[count-1], c.lens[count-1] = 0, 0
	}
	n.count.Store(int32(count - 1))
	if count == 1 {
		n.setPrefix("", 0)
	}
	return key, value
}

// moveLeafEntries moves count entries of src from from to dst at at, which
// has room, closing src's gap and clearing the slots it frees.
func (t *genericTree[K, V, O]) moveLeafEntries(dst *gleaf[K, V], at int, src *gleaf[K, V], from, count int) {
	srcCount, dstCount := int(src.count.Load()), int(dst.count.Load())
	copy(dst.codes[at+count:dstCount+count], dst.codes[at:dstCount])
	copy(dst.keys[at+count:dstCount+count], dst.keys[at:dstCount])
	copy(dst.values[at+count:dstCount+count], dst.values[at:dstCount])
	copy(dst.codes[at:at+count], src.codes[from:from+count])
	copy(dst.keys[at:at+count], src.keys[from:from+count])
	copy(dst.values[at:at+count], src.values[from:from+count])
	copy(src.codes[from:srcCount-count], src.codes[from+count:srcCount])
	copy(src.keys[from:srcCount-count], src.keys[from+count:srcCount])
	copy(src.values[from:srcCount-count], src.values[from+count:srcCount])
	for j := srcCount - count; j < srcCount; j++ {
		src.codes[j] = codePad
	}
	if prefixed[O, K]() {
		// Text leaves' tails and lengths follow their prefixes, which the
		// refits below set; the freed slots hold none.
		clear(src.text().tails[srcCount-count : srcCount])
		clear(src.text().lens[srcCount-count : srcCount])
	}
	clear(src.keys[srcCount-count : srcCount])
	clear(src.values[srcCount-count : srcCount])
	dst.count.Store(int32(dstCount + count))
	src.count.Store(int32(srcCount - count))
	t.refitLeaf(dst)
	t.refitLeaf(src)
}

// insertChild inserts child, whose keys are at least separator, at i in
// internal node n, which has room.
func (t *genericTree[K, V, O]) insertChild(n *ginner[K], i int, separator *K, child *gheader) {
	count := int(n.count.Load())
	for j := count; j > i; j-- {
		n.seps[j].Store(n.seps[j-1].Load())
		n.children[j].Store(n.children[j-1].Load())
	}
	n.seps[i].Store(separator)
	n.children[i].Store(child)
	n.count.Store(int32(count + 1))
	t.refitInner(n)
}

// removeChild removes internal node n's child i.
func (t *genericTree[K, V, O]) removeChild(n *ginner[K], i int) {
	count := int(n.count.Load())
	for j := i; j < count-1; j++ {
		n.seps[j].Store(n.seps[j+1].Load())
		n.children[j].Store(n.children[j+1].Load())
	}
	n.seps[count-1].Store(nil)
	n.children[count-1].Store(nil)
	n.count.Store(int32(count - 1))
	t.refitInner(n)
}

// moveChildEntries moves count children of src from from, with their
// separators, to dst at at, as moveLeafEntries moves entries.
func (t *genericTree[K, V, O]) moveChildEntries(dst *ginner[K], at int, src *ginner[K], from, count int) {
	srcCount, dstCount := int(src.count.Load()), int(dst.count.Load())
	for j := dstCount - 1; j >= at; j-- {
		dst.seps[j+count].Store(dst.seps[j].Load())
		dst.children[j+count].Store(dst.children[j].Load())
	}
	for j := 0; j < count; j++ {
		dst.seps[at+j].Store(src.seps[from+j].Load())
		dst.children[at+j].Store(src.children[from+j].Load())
	}
	for j := from; j+count < srcCount; j++ {
		src.seps[j].Store(src.seps[j+count].Load())
		src.children[j].Store(src.children[j+count].Load())
	}
	for j := srcCount - count; j < srcCount; j++ {
		src.seps[j].Store(nil)
		src.children[j].Store(nil)
	}
	dst.count.Store(int32(dstCount + count))
	src.count.Store(int32(srcCount - count))
	t.refitInner(dst)
	t.refitInner(src)
}
