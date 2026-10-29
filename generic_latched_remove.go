package gobbtree

// remove removes seek's entry, if condition (when not nil) holds of it, and
// returns its stored key and value, as latchedTree.remove: an entry whose
// leaf keeps its minimum, or whose leaf is the root, comes out with that
// leaf latched exclusively; otherwise the removal descends again keeping its
// path, takes the nodes its fixing changes without waiting, and fixes the
// levels it leaves underfull.
func (t *genericTree[K, V, O]) remove(seek int, key K, condition func(K, V) bool) (K, V, bool) {
	if t.owner {
		return t.ownerRemove(seek, key, condition, true)
	}
	var k K
	var v V
	if t.root.Load() == nil {
		return k, v, false
	}
	var code uint64
	if seek == seekKey {
		code = t.code(key)
	}
	if k, v, removed, done := t.removeLatched(seek, key, code, condition); done {
		return k, v, removed
	}
	for {
		if k, v, removed, done := t.removeRebalancing(seek, key, code, condition, true); done {
			return k, v, removed
		}
		// Another goroutine holds a node the removal needs: wait a moment,
		// as upstream's Interleaving::wait(1).
		spinWait()
	}
}

// target is where seek's entry is in leaf n, or false without one.
func (t *genericTree[K, V, O]) target(n *gleaf[K, V], seek int, key K, code uint64) (int, bool) {
	count := int(n.count.Load())
	switch seek {
	case seekFirst:
		return 0, count > 0
	case seekLast:
		return count - 1, count > 0
	}
	return t.find(n, key, code)
}

func (t *genericTree[K, V, O]) removeLatched(seek int, key K, code uint64, condition func(K, V) bool) (k K, v V, removed, done bool) {
	n, version := t.leafLatched(key, code, seek, exclusive)
	defer n.latch.unlockExclusive(version)
	i, ok := t.target(n, seek, key, code)
	if !ok || condition != nil && !condition(t.key(n, i), n.values[i]) {
		return k, v, false, true
	}
	if int(n.count.Load()) <= leafMinLen && t.root.Load() != &n.gheader {
		return k, v, false, false
	}
	k, v = t.removeEntry(n, i, true)
	return k, v, true, true
}

// removeRebalancing is one try of a removal that leaves its leaf underfull;
// done is false, with nothing changed, if a node it takes is held by
// another goroutine or changed since the descent.
func (t *genericTree[K, V, O]) removeRebalancing(seek int, key K, code uint64, condition func(K, V) bool, handOut bool) (k K, v V, removed, done bool) {
	var path gpath[K]
	n, version := t.descend(key, code, seek, false, exclusive, &path)
	var held latchSet
	released := false
	defer func() {
		if !released {
			held.unlock()
			n.latch.unlockExclusive(version)
		}
	}()
	i, ok := t.target(n, seek, key, code)
	if !ok || condition != nil && !condition(t.key(n, i), n.values[i]) {
		return k, v, false, true
	}
	// Another goroutine may have filled the leaf since.
	if int(n.count.Load()) > leafMinLen || t.root.Load() == &n.gheader {
		k, v = t.removeEntry(n, i, handOut)
		return k, v, true, true
	}
	fixes, ok := t.takeFixes(&path, int(n.count.Load())-1, &held)
	if !ok {
		return k, v, false, false
	}
	k, v = t.removeEntry(n, i, handOut)
	depth := path.depth
	for fix := 0; fix < fixes; fix++ {
		level := path.levels[depth-1-fix]
		t.fixUnderfullChild(level.n, level.edge, fix == 0)
	}
	if fixes == depth && depth > 0 {
		// The root lost a separator; one without any is popped.
		if root := path.levels[0].n; root.count.Load() == 1 {
			t.root.Store(root.children[0].Load())
			root.freed()
		}
	}
	held.unlock()
	n.latch.unlockExclusive(version)
	released = true
	return k, v, true, true
}

// takeFixes takes the nodes that fixing the leaf of path changes once a
// removal leaves it with leafLen entries, as latchedTree.takeFixes: for each
// level left underfull, its parent and the sibling the fixing pairs it with.
func (t *genericTree[K, V, O]) takeFixes(path *gpath[K], leafLen int, held *latchSet) (int, bool) {
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
		if !held.takeAt(&parent.n.gheader, parent.version) {
			return 0, false
		}
		sibling := parent.n.children[fixingSibling(parent.edge)].Load()
		if sibling == nil {
			return 0, false
		}
		if fixes == 0 {
			if !held.takeLeaf(sibling) {
				return 0, false
			}
		} else if !held.takeInternal(sibling) {
			return 0, false
		}
		fixes++
		if !fitMerged(childLen, int(sibling.count.Load())) {
			return fixes, true
		}
		childLen = parent.count - 1
	}
}

// fixUnderfullChild fixes parent's child at index, one entry short of its
// minimum (D22): it merges the child with its fixing sibling when both fit
// in one node, and otherwise moves one entry over. Every node it changes is
// held; a merged node is left holding nothing.
func (t *genericTree[K, V, O]) fixUnderfullChild(parent *ginner[K], index int, leaves bool) {
	sibling := fixingSibling(index)
	child, other := parent.children[index].Load(), parent.children[sibling].Load()
	if fitMerged(int(child.count.Load()), int(other.count.Load())) {
		left, right := min(index, sibling), max(index, sibling)
		l, r := parent.children[left].Load(), parent.children[right].Load()
		if leaves {
			t.moveLeafEntries(asLeaf[K, V](l), int(l.count.Load()), asLeaf[K, V](r), 0, int(r.count.Load()))
			// The merged leaf spans both: l's low fence and r's high.
			low, _ := leafFences(parent, left)
			_, high := leafFences(parent, right)
			t.fitLeaf(asLeaf[K, V](l), low, high)
		} else {
			// The merged node spans both: its fences are l's low and r's.
			asInner[K](l).high.Store(asInner[K](r).high.Load())
			t.moveChildEntries(asInner[K](l), int(l.count.Load()), asInner[K](r), 0, int(r.count.Load()))
		}
		t.removeChild(parent, right)
		r.freed()
		return
	}
	switch {
	case sibling < index && leaves:
		c, o := asLeaf[K, V](child), asLeaf[K, V](other)
		t.moveLeafEntries(c, 0, o, int(o.count.Load())-1, 1)
		t.setSeparator(parent, index, t.separator(c))
		t.fitChild(c, parent, index)
		t.fitChild(o, parent, sibling)
	case sibling < index:
		c, o := asInner[K](child), asInner[K](other)
		// o's last child moves to c: its separator divides them now.
		o.high.Store(o.seps[int(o.count.Load())-1].Load())
		t.moveChildEntries(c, 0, o, int(o.count.Load())-1, 1)
		t.setSeparator(parent, index, c.seps[0].Load())
	case leaves:
		c, o := asLeaf[K, V](child), asLeaf[K, V](other)
		t.moveLeafEntries(c, int(c.count.Load()), o, 0, 1)
		t.setSeparator(parent, sibling, t.separator(o))
		t.fitChild(c, parent, index)
		t.fitChild(o, parent, sibling)
	default:
		c, o := asInner[K](child), asInner[K](other)
		// o's first child moves to c: o's second's separator divides them.
		c.high.Store(o.seps[1].Load())
		t.moveChildEntries(c, int(c.count.Load()), o, 0, 1)
		t.setSeparator(parent, sibling, o.seps[0].Load())
	}
}

// box is a separator for key: a copy no one changes.
func box[K any](key K) *K {
	b := new(K)
	*b = key
	return b
}

// setSeparator makes separator i of internal node n, held, sep.
func (t *genericTree[K, V, O]) setSeparator(n *ginner[K], i int, sep *K) {
	n.seps[i].Store(sep)
	t.refitInner(n)
}
