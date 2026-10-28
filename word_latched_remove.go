package gobbtree

// Which entry a removal takes, by its key or at an end of the tree.
const (
	removeKey = iota
	removeFirst
	removeLast
)

// remove removes an entry, if condition (when not nil) holds of it, and
// returns its key and value. As upstream's (tree/latched/remove.rs): an
// entry whose leaf keeps its minimum, or whose leaf is the root, comes out
// with that leaf latched exclusively and nothing else held. Otherwise the
// removal descends again keeping its path, takes the nodes its fixing
// changes without waiting, and fixes the levels it leaves underfull.
func (t *latchedTree) remove(which int, key uint64, condition func(key, value uint64) bool) (uint64, uint64, bool) {
	if t.root.Load() == 0 {
		return 0, 0, false
	}
	k, v, removed, done := t.removeLatched(which, key, condition)
	if done {
		return k, v, removed
	}
	var b backoff
	for {
		k, v, removed, done := t.removeRebalancing(which, key, condition)
		if done {
			return k, v, removed
		}
		// Another thread holds a node the removal needs.
		b.spin()
	}
}

// target is where which's entry is in leaf, or false without one.
func target(leaf *wordNode, which int, key uint64) (int, bool) {
	count := int(leaf.n)
	switch which {
	case removeFirst:
		return 0, count > 0
	case removeLast:
		return count - 1, count > 0
	}
	i := leaf.exactEntry(key)
	return i, i < count && leaf.entries()[i].key == key
}

// descentKey is the key whose descent reaches which's leaf.
func descentKey(which int, key uint64) uint64 {
	switch which {
	case removeFirst:
		return 0
	case removeLast:
		return ^uint64(0)
	}
	return key
}

func (t *latchedTree) removeLatched(which int, key uint64, condition func(key, value uint64) bool) (k, v uint64, removed, done bool) {
	var leaf *wordNode
	var leafID uint32
	var version uint64
	var i int
	var ok bool
	if seqlockPlainStores {
		// Search optimistically, then take the leaf at the version read, as
		// insert's upgrade does.
		leaf, leafID, version = t.descend(descentKey(which, key), optimistic, nil)
		i, ok = target(leaf, which, key)
	}
	if leaf == nil || !leaf.tryLockAt(version) {
		// Changed or held since read: latch it the waiting way.
		leaf, leafID, version = t.descend(descentKey(which, key), exclusive, nil)
		i, ok = target(leaf, which, key)
	}
	defer leaf.unlockExclusive(version)
	if !ok {
		return 0, 0, false, true
	}
	k, v = leaf.entries()[i].key, leaf.entries()[i].value
	if condition != nil && !condition(k, v) {
		return 0, 0, false, true
	}
	if int(leaf.n) <= leafMinLen && t.root.Load() != leafID {
		return 0, 0, false, false
	}
	removeAt(leaf, i)
	return k, v, true, true
}

// removeRebalancing is one try of a removal that leaves its leaf underfull.
// done is false, with nothing changed, if a node it takes is held by another
// thread or changed since the descent.
func (t *latchedTree) removeRebalancing(which int, key uint64, condition func(key, value uint64) bool) (k, v uint64, removed, done bool) {
	var path treePath
	leaf, leafID, version := t.descend(descentKey(which, key), exclusive, &path)
	var held heldNodes
	released := false
	defer func() {
		if !released {
			held.unlock()
			leaf.unlockExclusive(version)
		}
	}()
	i, ok := target(leaf, which, key)
	if !ok {
		return 0, 0, false, true
	}
	k, v = leaf.entries()[i].key, leaf.entries()[i].value
	if condition != nil && !condition(k, v) {
		return 0, 0, false, true
	}
	// Another thread may have filled the leaf since.
	if int(leaf.n) > leafMinLen || t.root.Load() == leafID {
		removeAt(leaf, i)
		return k, v, true, true
	}
	fixes, ok := t.takeFixes(&path, int(leaf.n)-1, &held)
	if !ok {
		return 0, 0, false, false
	}
	t.pages.mu.Lock()
	defer t.pages.mu.Unlock()
	removeAt(leaf, i)
	depth := path.depth
	for fix := 0; fix < fixes; fix++ {
		level := path.levels[depth-1-fix]
		t.fixUnderfullChild(level.n, level.edge, fix == 0)
	}
	if fixes == depth && depth > 0 {
		// The root lost a separator; one without any is popped.
		if root := path.levels[0].n; root.n == 1 {
			t.root.Store(root.inner().children[0])
			// A freed node holds no children, so that a route that
			// reaches it through a stale hint starts over.
			storeCountRacing(root, 0)
			t.free(path.levels[0].id)
		}
	}
	// Let go first, so that every freed page's latch word says it changed.
	held.unlock()
	leaf.unlockExclusive(version)
	released = true
	t.release()
	return k, v, true, true
}

// takeFixes takes the nodes that fixing the leaf of path changes, once a
// removal leaves it with leafLen entries: for each level left underfull,
// from the leaf up, its parent and the sibling the fixing merges it with or
// moves an entry from. It returns how many levels to fix; false if a node is
// held by another thread or changed since the descent.
func (t *latchedTree) takeFixes(path *treePath, leafLen int, held *heldNodes) (int, bool) {
	fixes, childLen := 0, leafLen
	for {
		level := path.depth - fixes
		minLen := innerMinLen
		if fixes == 0 {
			minLen = leafMinLen
		}
		if childLen >= minLen || level == 0 {
			return fixes, true
		}
		parent := path.levels[level-1]
		if !held.takeAt(parent.n, parent.version) {
			return 0, false
		}
		sibling := t.node(parent.n.inner().children[fixingSibling(parent.edge)])
		if fixes == 0 {
			if !held.takeLeaf(sibling) {
				return 0, false
			}
		} else if !held.takeInternal(sibling) {
			return 0, false
		}
		fixes++
		if !fitMerged(childLen, int(sibling.n)) {
			// The child takes an entry from its sibling: the parent keeps
			// its separators.
			return fixes, true
		}
		childLen = parent.count - 1
	}
}

// fixingSibling is the sibling a fixing pairs child index with: the left one
// if there is one (upstream's Internal::fixing_sibling).
func fixingSibling(index int) int {
	if index > 0 {
		return index - 1
	}
	return index + 1
}

// fitMerged reports that siblings of left and right entries fit in one node.
func fitMerged(left, right int) bool { return left+right <= nodeSize }

// fixUnderfullChild fixes parent's child at index, one entry short of its
// minimum (D22): it merges the child with its fixing sibling when both fit
// in one node, and otherwise moves one entry over from the sibling. Only a
// merge takes a separator from the parent. Every node it changes is held.
func (t *latchedTree) fixUnderfullChild(parent *wordNode, index int, leaves bool) {
	children := &parent.inner().children
	sibling := fixingSibling(index)
	child, other := t.node(children[index]), t.node(children[sibling])
	if fitMerged(int(child.n), int(other.n)) {
		left, right := min(index, sibling), max(index, sibling)
		leftID, rightID := children[left], children[right]
		l, r := t.node(leftID), t.node(rightID)
		if leaves {
			moveEntries(l, int(l.n), r, 0, int(r.n))
			setHighFence(l, highFence(r))
			if t.rightmost.Load() == rightID {
				t.rightmost.Store(leftID)
			}
		} else {
			moveChildren(l, int(l.n), r, 0, int(r.n))
			setHighFence(l, highFence(r))
		}
		removeChild(parent, right)
		t.free(rightID)
		return
	}
	switch {
	case sibling < index && leaves:
		moveEntries(child, 0, other, int(other.n)-1, 1)
		storeKeyRacing(parent, index, child.entries()[0].key)
		setLeafFences(child, child.entries()[0].key, highFence(child))
		setHighFence(other, child.entries()[0].key)
	case sibling < index:
		moveChildren(child, 0, other, int(other.n)-1, 1)
		storeKeyRacing(parent, index, child.keys[0])
		setHighFence(other, child.keys[0])
	case leaves:
		moveEntries(child, int(child.n), other, 0, 1)
		storeKeyRacing(parent, sibling, other.entries()[0].key)
		setLeafFences(other, other.entries()[0].key, highFence(other))
		setHighFence(child, other.entries()[0].key)
	default:
		moveChildren(child, int(child.n), other, 0, 1)
		storeKeyRacing(parent, sibling, other.keys[0])
		setHighFence(child, other.keys[0])
	}
	refitWindows(parent)
}
