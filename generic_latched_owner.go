package gobbtree

import (
	"sync/atomic"
	"unsafe"
)

// An owner's tree is a Tree's: one goroutine changes it, and others read it
// only while none does, as upstream's unsync tree is used. Its lookups and
// the writes that keep to one leaf descend with plain loads, taking no latch
// and checking no version, as nothing changes the nodes meanwhile; a write
// that splits or rebalances takes the latched way, whose latches no other
// goroutine contends. A plain write leaves the leaf's version as it was,
// which only racing readers, an owner's tree has none, would need moved.

// owned loads a word of an owner's tree plainly. The latched paths' atomic
// loads are acquires, which on arm64 hold the loads after them, a leaf's
// codes behind its header: two waves of misses for one.
func owned(p *atomic.Int32) int { return int(*(*int32)(unsafe.Pointer(p))) }

// ownerLeaf is key's leaf, by a descent with plain loads, for keys without
// prefixes; ownerSeek descends for text keys.
func (t *genericTree[K, V, O]) ownerLeaf(key K, code uint64) *gleaf[K, V] {
	h := plainPointer(&t.root)
	for !h.leaf {
		n := asInner[K](h)
		i, _ := t.route(n, key, code, owned(&n.count), false)
		h = plainPointer(&n.children[i&(nodeSize-1)])
	}
	return asLeaf[K, V](h)
}

// ownerSeek is key's leaf, its slot there and whether key is there, and
// whether key fits the leaf's windows, so begins with its prefix. A text
// key descends and is checked in its leaf in one loop, calls costing a
// lookup what its loads do: a key that fits its windows is there exactly
// where a slot's code, tail and length are its;
// any other goes to the leaf's search. A key without the shared prefix
// routes anywhere, but it is in no leaf, and a writer admits its key first.
//
// Where place is false, a key of integers that isn't there has no slot.
func (t *genericTree[K, V, O]) ownerSeek(key K, code uint64, place bool) (n *gleaf[K, V], i int, found, fits bool) {
	if t.wordCodes() {
		// A key the hinted leaf holds is there; one between its first
		// and last keys is absent, and goes in there.
		if h, i, found, ok := hintedCode(plainPointer(&t.hints), &t.leaves, code, place); ok {
			return asLeaf[K, V](h), i, found, false
		}
		return t.ownerDescend(code, place)
	}
	if !prefixed[O, K]() {
		n := t.ownerLeaf(key, code)
		i, ok := t.find(n, key, code)
		return n, i, ok, false
	}
	return t.ownerSeekText(key, code)
}

// ownerDescend is ownerSeek's descent for integer keys their hints didn't
// settle, filling their cells as it goes.
func (t *genericTree[K, V, O]) ownerDescend(code uint64, place bool) (n *gleaf[K, V], i int, found, fits bool) {
	{
		root := plainPointer(&t.root)
		if !root.leaf {
			h, i, ok := descendCodeFilling(root, code, currentHints(&t.hints, &t.making, &t.reshaped, root), place)
			return asLeaf[K, V](h), i, ok, false
		}
		h, i, ok := descendCode(root, code, place)
		return asLeaf[K, V](h), i, ok, false
	}
}

// ownerSeekText is ownerSeek for text keys.
func (t *genericTree[K, V, O]) ownerSeekText(key K, code uint64) (n *gleaf[K, V], i int, found, fits bool) {
	s := textView(&key)
	h := plainPointer(&t.root)
	for {
		at, lo, found := descendText(h, s)
		switch found {
		case textTie:
			// Summaries tie: the boxed separators settle it.
			n := asInner[K](at)
			i, _ := t.route(n, key, code, owned(&n.count), false)
			h = plainPointer(&n.children[i&(nodeSize-1)])
			continue
		case textAbsent:
			return asLeaf[K, V](at), lo, false, true
		}
		n := asLeaf[K, V](at)
		if found == textCode {
			tail, ok := fastWindow(s, owned(&n.prefix)+8)
			if !ok {
				tail = stringCode(s, owned(&n.prefix)+8)
			}
			j := lo & (nodeSize - 1)
			if slot := &n.text().slots[j]; slot.tail == tail && refLen(slot.ref) == len(s) {
				return n, lo, true, true
			}
		}
		i, ok := t.find(n, key, code)
		return n, i, ok, found == textCode
	}
}

func (t *genericTree[K, V, O]) ownerGet(key K) (value V, found bool) {
	if t.wordCodes() {
		code := numericKey[O](key)
		h, i, found, ok := hintedCode(plainPointer(&t.hints), &t.leaves, code, false)
		if !ok {
			if plainPointer(&t.root) == nil {
				return value, false
			}
			var n *gleaf[K, V]
			n, i, found, _ = t.ownerDescend(code, false)
			h = &n.gheader
		}
		if found {
			return asLeaf[K, V](h).values[i&(nodeSize-1)], true
		}
		return value, false
	}
	if plainPointer(&t.root) == nil {
		return value, false
	}
	if prefixed[O, K]() {
		if n, i, ok, _ := t.ownerSeekText(key, 0); ok {
			return n.values[i&(nodeSize-1)], true
		}
		return value, false
	}
	if n, i, ok, _ := t.ownerSeek(key, numericKey[O](key), false); ok {
		return n.values[i&(nodeSize-1)], true
	}
	return value, false
}

func (t *genericTree[K, V, O]) ownerRead(key K, visit func(K, V)) bool {
	if plainPointer(&t.root) == nil {
		return false
	}
	code := t.code(key)
	n, i, ok, _ := t.ownerSeek(key, code, false)
	if ok {
		visit(t.key(n, i), n.values[i])
	}
	return ok
}

func (t *genericTree[K, V, O]) ownerUpdate(key K, change func(K, V) V) (V, bool) {
	var zero V
	if plainPointer(&t.root) == nil {
		return zero, false
	}
	code := t.code(key)
	n, i, ok, _ := t.ownerSeek(key, code, false)
	if !ok {
		return zero, false
	}
	value := change(t.key(n, i), n.values[i])
	n.values[i] = value
	return value, true
}

// ownerInsert inserts key with value into an owner's tree where it is
// absent: into its leaf if it has room, else by the latched split. Where
// key is there, it changes nothing and returns its leaf and slot, for the
// caller to apply its policy to the value: a store there is then the
// caller's last act, and the loads after it, which the core may hold until
// it knows the store's address, are the caller's caller's alone.
func (t *genericTree[K, V, O]) ownerInsert(key K, value V) (n *gleaf[K, V], i int, found bool) {
	if plainPointer(&t.root) == nil {
		t.start()
	}
	code := uint64(0)
	if !prefixed[O, K]() {
		code = numericKey[O](key)
	}
	var fits bool
	switch {
	case prefixed[O, K]():
		n, i, found, fits = t.ownerSeekText(key, code)
		return t.ownerInsertText(n, i, found, fits, key, value)
	case t.wordCodes():
		h, j, f, ok := hintedCode(plainPointer(&t.hints), &t.leaves, code, true)
		if ok && f {
			return asLeaf[K, V](h), j & (nodeSize - 1), true
		}
		return t.ownerPlace(key, code, value, asLeaf[K, V](h), j, ok)
	default:
		n, i, found, _ = t.ownerSeek(key, code, true)
	}
	if found {
		return n, i & (nodeSize - 1), true
	}
	return t.ownerPlace(key, code, value, n, i, true)
}

// ownerPlace inserts key, absent, at slot i of leaf n, where a seek or the
// hints placed it. A key not placed, as one past the hint table's domain,
// goes past the tail, which starts the tree appending, or where a descent
// places it; one the descent finds is returned, as ownerInsert returns it.
func (t *genericTree[K, V, O]) ownerPlace(key K, code uint64, value V, n *gleaf[K, V], i int, placed bool) (*gleaf[K, V], int, bool) {
	if !placed {
		if t.ownerAppend(key, code, value) {
			return nil, 0, false
		}
		var found bool
		if n, i, found, _ = t.ownerDescend(code, true); found {
			return n, i & (nodeSize - 1), true
		}
	}
	if owned(&n.count) < nodeSize {
		t.insertEntry(n, i, key, value)
	} else {
		t.ownerSplit(n, i, key, code, value)
	}
	return nil, 0, false
}

// ownerAppend inserts key last in the tail, splitting it where full, where
// code is past its last key, which nothing holds, and records whether it
// could: the tree is then appending.
func (t *genericTree[K, V, O]) ownerAppend(key K, code uint64, value V) bool {
	n, count, ok := t.tailFor(code)
	t.appending = ok
	if !ok {
		return false
	}
	switch {
	case count == nodeSize:
		t.ownerSplit(n, nodeSize, key, code, value)
		// A tree grown by appends descends none: its hints follow its
		// growth here, as a descent's would.
		currentHints(&t.hints, &t.making, &t.reshaped, plainPointer(&t.root))
	case t.wholeCodes():
		n.pushCode(count, code, value)
	default:
		t.insertEntry(n, count, key, value)
	}
	return true
}

// ownerPush is upstream's last-leaf shortcut (D45), for a tree appending:
// code, a key its codes hold whole, goes last in the tail, where it has
// room and code is past its last, with no search. A key that isn't one
// stops the appending, so inserts of other orders pay only its test; the
// next append past the tail (ownerAppend), which also splits a full one,
// starts it again.
func (t *genericTree[K, V, O]) ownerPush(code uint64, value V) bool {
	n, count, ok := t.tailFor(code)
	if ok && count < nodeSize && t.wholeCodes() {
		n.pushCode(count, code, value)
		return true
	}
	t.appending = ok
	return false
}

// tailFor is the tail and its count, and whether code goes past its last
// key: an owner's tail is its rightmost leaf, which a split or a merge
// there moves, and holds the keys nothing holds after.
func (t *genericTree[K, V, O]) tailFor(code uint64) (n *gleaf[K, V], count int, ok bool) {
	h := plainPointer(&t.tail)
	if h == nil {
		return nil, 0, false
	}
	n, count = asLeaf[K, V](h), owned(&h.count)
	// An empty tail's last slot holds codePad, past every code.
	_, high := bounds(h)
	return n, count, high == 0 && n.codes[(count-1)&(nodeSize-1)] < code
}

// ownerInsertText is ownerInsert for a text key, from ownerSeekText's
// result: a Tree's insert seeks itself, and replaces a key it finds
// without the call between, whose frame its store would wait behind.
func (t *genericTree[K, V, O]) ownerInsertText(n *gleaf[K, V], i int, found, fits bool, key K, value V) (*gleaf[K, V], int, bool) {
	if !found {
		// A key with the prefix of a leaf whose keys share the shared
		// prefix has it too; any other is checked.
		shared := plainPointer(&t.shared)
		switch {
		case shared == nil:
			t.admit(key)
		case (!fits || owned(&n.prefix) < len(*shared)) && t.sharedOrder(key) != 0:
			// A new key without the shared prefix shortens it, which
			// refits the borders its route may have crossed: route it
			// again.
			t.admit(key)
			n, i, found, _ = t.ownerSeekText(key, 0)
		}
	}
	if found {
		return n, i & (nodeSize - 1), true
	}
	if owned(&n.count) < nodeSize {
		t.insertEntry(n, i, key, value)
	} else {
		t.ownerSplit(n, i, key, 0, value)
	}
	return nil, 0, false
}

// ownerPath records key's path to leaf n, by a plain descent.
func (t *genericTree[K, V, O]) ownerPath(path *gpath[K], n *gleaf[K, V], key K, code uint64) {
	h := plainPointer(&t.root)
	for !h.leaf {
		inner := asInner[K](h)
		count := owned(&inner.count)
		edge, _ := t.route(inner, key, code, count, false)
		path.levels[path.depth] = gpathLevel[K]{n: inner, edge: edge, count: count}
		path.depth++
		h = plainPointer(&inner.children[edge&(nodeSize-1)])
	}
	if h != &n.gheader {
		panic("gobbtree: an owner's descent lost its leaf")
	}
}

// rightPath records the path to leaf n, the rightmost, down the last child
// of every node, by a plain descent.
func (t *genericTree[K, V, O]) rightPath(path *gpath[K], n *gleaf[K, V]) {
	h := plainPointer(&t.root)
	for !h.leaf {
		inner := asInner[K](h)
		count := owned(&inner.count)
		path.levels[path.depth] = gpathLevel[K]{n: inner, edge: count - 1, count: count}
		path.depth++
		h = plainPointer(&inner.children[(count-1)&(nodeSize-1)])
	}
	if h != &n.gheader {
		panic("gobbtree: an owner's right border lost its leaf")
	}
}

// ownerSplit splits leaf n, full, as split does, and inserts key and value
// at i, where they go: by a plain descent that records key's path, and
// with no latches taken, which no other goroutine of an owner's tree
// contends; the nodes it makes start latched, and it lets them go.
func (t *genericTree[K, V, O]) ownerSplit(n *gleaf[K, V], i int, key K, code uint64, value V) {
	var path gpath[K]
	if _, high := bounds(&n.gheader); t.wordCodes() && i == nodeSize && high == 0 {
		// An append past the rightmost leaf, which the right border
		// leads to, without a route.
		t.rightPath(&path, n)
	} else {
		t.ownerPath(&path, n, key, code)
	}
	if i > 0 && i < nodeSize && t.ownerRedistribute(&path, n, i, key, value) {
		return
	}
	var fresh splitSet
	t.splitHeld(&path, path.room(), n, i, key, value, &fresh)
	fresh.unlock()
}

// ownerRedistribute is redistribute in an owner's tree, for a key inside
// leaf n, full, at i: its own frame keeps the set of nodes it takes off
// the splits that never redistribute, an append's among them.
func (t *genericTree[K, V, O]) ownerRedistribute(path *gpath[K], n *gleaf[K, V], i int, key K, value V) bool {
	var held splitSet
	ok := t.redistribute(path, n, i, key, value, &held)
	held.unlock()
	return ok
}

// ownerRemove is remove from an owner's tree: from its leaf where the leaf
// keeps its minimum or is the root, else by the latched rebalancing.
func (t *genericTree[K, V, O]) ownerRemove(seek int, key K, condition func(K, V) bool, handOut bool) (K, V, bool) {
	var k K
	var v V
	if plainPointer(&t.root) == nil {
		return k, v, false
	}
	var n *gleaf[K, V]
	var i int
	var ok bool
	code := uint64(0)
	if seek == seekKey {
		if !prefixed[O, K]() {
			code = t.code(key)
		}
		n, i, ok, _ = t.ownerSeek(key, code, false)
	} else {
		n, _ = t.descend(key, 0, seek, false, optimistic, nil)
		i, ok = t.target(n, seek, key, 0)
	}
	if !ok || condition != nil && !condition(t.key(n, i), n.values[i]) {
		return k, v, false
	}
	if plainPointer(&t.root) == &n.gheader || owned(&n.count) > leafMinLen && (i > 0 || seek == seekFirst || t.whole) {
		k, v = t.removeEntry(n, i, handOut)
		return k, v, true
	}
	if owned(&n.count) > leafMinLen && seek == seekKey {
		// The leaf's first key, which may be a separator, from a leaf that
		// keeps its minimum: the path, plainly, and no latches.
		var path gpath[K]
		t.ownerPath(&path, n, key, code)
		k, v = t.removeEntry(n, 0, handOut)
		if level := path.separatorLevel(); level >= 0 {
			t.promote(&path, level, n)
		}
		return k, v, true
	}
	for {
		if k, v, removed, done := t.removeRebalancing(seek, key, code, condition, handOut); done {
			return k, v, removed
		}
	}
}
