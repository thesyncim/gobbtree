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
		if seqlockPlainStores && t.wordCodes() && condition == nil {
			if k, v, done := t.removeHinted(code); done {
				return k, v, true
			}
		}
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
	if t.root.Load() != &n.gheader && (int(n.count.Load()) <= leafMinLen || i == 0 && seek != seekFirst && !t.whole) {
		// Underfull, or the leaf's first key, which may be a separator.
		return k, v, false, false
	}
	k, v = t.removeEntry(n, i, true)
	return k, v, true, true
}

// Separators are keys the tree holds (D49), but for keys their codes hold
// whole (codeKeys), whose separators are their codes, decoded where a key
// is needed: any code above the left subtree's keys and at most the
// right's first separates them, so removing a leaf's first key leaves its
// separator as it was.
//
// Separator i of an internal node
// is the first key of the leftmost leaf below its child i. A removal of a
// leaf's first key, where the leaf has one, the separator of the lowest
// ancestor its path turns right at, promotes the leaf's next key: that
// ancestor's separator, the own separators of the nodes between, and the
// high fences of the left subtree's right border take it, and the nodes
// whose fences it moves refit. The leftmost leaf has no separator.

// separatorLevel is the level of path whose separator the first key of the
// path's leaf is, or -1 for the tree's leftmost leaf.
func (p *gpath[K]) separatorLevel() int {
	for level := p.depth - 1; level >= 0; level-- {
		if p.levels[level].edge > 0 {
			return level
		}
	}
	return -1
}

// takePromotion takes what promoting the separator at level of path
// changes: the path's nodes from it down, and the right border, leaf
// included, of the subtree left of the separator.
func (t *genericTree[K, V, O]) takePromotion(path *gpath[K], level int, held *latchSet) bool {
	for l := level; l < path.depth; l++ {
		if !held.takeAt(&path.levels[l].n.gheader, path.levels[l].version) {
			return false
		}
	}
	at := path.levels[level]
	left := at.n.children[(at.edge-1)&(nodeSize-1)].Load()
	if left == nil {
		return false
	}
	if left.leaf {
		return held.takeLeaf(left)
	}
	return held.takeInternal(left) && takeBorder(held, asInner[K](left), true)
}

// promote makes leaf n's first key, its old one removed, the separator at
// level of path, held as takePromotion took it. The separator's node keeps
// its fences and so its prefix, and takes the new key's code alone; the
// nodes whose fences move refit only where what the fences share changes,
// which it rarely does, as the new separator is the old one's successor.
func (t *genericTree[K, V, O]) promote(path *gpath[K], level int, n *gleaf[K, V]) {
	t.setHead(n)
	sep := t.firstKey(n)
	at := path.levels[level]
	t.replaceSeparator(at.n, at.edge, sep)
	// The right subtree's left border, n's path below the separator: their
	// low fences.
	for l := level + 1; l < path.depth; l++ {
		inner := path.levels[l].n
		if high := t.nodeHigh(path, l); prefixed[O, K]() && t.fencePrefix(&sep, high) != int(inner.prefix.Load()) {
			t.refitInner(inner, high)
		}
	}
	if path.depth > 0 {
		last := path.levels[path.depth-1]
		t.refitMoved(n, t.childHigh(path, path.depth-1, last.edge))
	}
	// The left subtree's right border: their high fences.
	h := at.n.children[(at.edge-1)&(nodeSize-1)].Load()
	for !h.leaf {
		inner := asInner[K](h)
		if prefixed[O, K]() && t.fencePrefix(t.lowFence(inner), &sep) != int(inner.prefix.Load()) {
			t.refitInner(inner, &sep)
		}
		h = inner.children[(inner.count.Load()-1)&(nodeSize-1)].Load()
	}
	t.refitMoved(asLeaf[K, V](h), &sep)
}

// replaceSeparator makes sep, a held leaf's head, internal node n's
// separator i, held, where n's fences stay: its prefix holds, and only code
// i changes.
func (t *genericTree[K, V, O]) replaceSeparator(n *ginner[K], i int, sep K) {
	if prefixed[O, K]() {
		storeSum(n.sums(), i, summaryOf(textView(&sep), int(n.prefix.Load())))
		return
	}
	n.codes[i].Store(windowKey[O](sep, 0))
}

// refitMoved refits text leaf n, held, whose windows are its keys' at its
// prefix, where its fences, its head and high, one of them moved, share
// another.
func (t *genericTree[K, V, O]) refitMoved(n *gleaf[K, V], high *K) {
	if !prefixed[O, K]() {
		return
	}
	if p := t.fencePrefix(t.leafLow(n), high); p != int(n.prefix.Load()) {
		t.refitLeaf(n, p)
	}
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
	promoted := -1
	if i == 0 && seek != seekFirst && !t.whole {
		promoted = path.separatorLevel()
	}
	// Another goroutine may have filled the leaf since.
	if promoted < 0 && (int(n.count.Load()) > leafMinLen || t.root.Load() == &n.gheader) {
		k, v = t.removeEntry(n, i, handOut)
		return k, v, true, true
	}
	if promoted >= 0 && !t.takePromotion(&path, promoted, &held) {
		return k, v, false, false
	}
	fixes := 0
	if t.root.Load() != &n.gheader {
		if fixes, ok = t.takeFixes(&path, int(n.count.Load())-1, &held); !ok {
			return k, v, false, false
		}
	}
	// The nodes the removal refits take their fences from the path, the
	// highest held at or above the highest node it changes.
	changed := path.depth - fixes
	if promoted >= 0 {
		changed = min(changed, promoted)
	}
	if path.depth > 0 && !path.takeHighHolder(changed, &held) {
		return k, v, false, false
	}
	k, v = t.removeEntry(n, i, handOut)
	// The separator first, so that the fixes below find every separator a
	// key the tree holds.
	if promoted >= 0 {
		t.promote(&path, promoted, n)
	}
	depth := path.depth
	for fix := 0; fix < fixes; fix++ {
		t.fixUnderfullChild(&path, depth-1-fix, fix == 0)
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

// fixUnderfullChild fixes the child of path's node at level that the path
// takes, one entry short of its minimum (D22): it merges the child with its
// fixing sibling when both fit in one node, and otherwise moves one entry
// over. Every node it changes is held; a merged node is left holding
// nothing. The nodes whose fences change refit to them, their heads and the
// separators the path tells.
func (t *genericTree[K, V, O]) fixUnderfullChild(path *gpath[K], level int, leaves bool) {
	parent, index := path.levels[level].n, path.levels[level].edge
	sibling := fixingSibling(index)
	child, other := parent.children[index].Load(), parent.children[sibling].Load()
	if fitMerged(int(child.count.Load()), int(other.count.Load())) {
		left, right := min(index, sibling), max(index, sibling)
		l, r := parent.children[left].Load(), parent.children[right].Load()
		// The merged node spans both: l's low fence and r's high.
		high := t.childHigh(path, level, right)
		if leaves {
			if t.wordCodes() {
				_, high := bounds(r)
				setHigh(l, high)
			}
			t.moveLeafEntries(asLeaf[K, V](l), int(l.count.Load()), asLeaf[K, V](r), 0, int(r.count.Load()))
			t.removeChild(parent, right)
			t.fitLeaf(asLeaf[K, V](l), t.leafLow(asLeaf[K, V](l)), high)
			t.clearHead(asLeaf[K, V](r))
			t.leaves.remove(r)
		} else {
			t.moveChildEntries(asInner[K](l), int(l.count.Load()), asInner[K](r), 0, int(r.count.Load()))
			t.removeChild(parent, right)
			t.refitInner(asInner[K](l), high)
		}
		r.freed()
		return
	}
	// One entry crosses: the right one of the pair gets a new first key,
	// the separator between them.
	pairLeft, pairRight := min(index, sibling), max(index, sibling)
	high := t.childHigh(path, level, pairRight)
	switch {
	case sibling < index && leaves:
		c, o := asLeaf[K, V](child), asLeaf[K, V](other)
		t.moveLeafEntries(c, 0, o, int(o.count.Load())-1, 1)
		t.setHead(c)
	case sibling < index:
		c, o := asInner[K](child), asInner[K](other)
		t.moveChildEntries(c, 0, o, int(o.count.Load())-1, 1)
	case leaves:
		c, o := asLeaf[K, V](child), asLeaf[K, V](other)
		t.moveLeafEntries(c, int(c.count.Load()), o, 0, 1)
		t.setHead(o)
	default:
		c, o := asInner[K](child), asInner[K](other)
		t.moveChildEntries(c, int(c.count.Load()), o, 0, 1)
	}
	l, r := parent.children[pairLeft].Load(), parent.children[pairRight].Load()
	sep, _ := t.separatorKey(parent, pairRight)
	t.replaceSeparator(parent, pairRight, sep)
	if leaves {
		if t.wordCodes() {
			setHigh(l, t.code(sep))
		}
		t.fitLeaf(asLeaf[K, V](l), t.leafLow(asLeaf[K, V](l)), &sep)
		t.fitLeaf(asLeaf[K, V](r), &sep, high)
		return
	}
	t.refitInner(asInner[K](l), &sep)
	t.refitInner(asInner[K](r), high)
}
