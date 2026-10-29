package gobbtree

import (
	"sync/atomic"
	"unsafe"
)

// Hinted lookups. A lookup whose hint names a leaf reads it with plain
// loads only, ordered by address dependencies as the descent's checks are:
// the latch word's load feeds the addresses of the words it guards, and
// every word read feeds the address of the closing load. An acquire load
// would hold every later load, the next lookup's included, until its own
// miss returned; plain ones let independent lookups overlap theirs. Only
// plain-store builds take this path. A hint that doesn't settle the lookup
// sends it the ordinary way.

// plainPointer loads a pointer published by an atomic store, plainly: what
// it points to is immutable once published, and reached by an address that
// depends on the load.
func plainPointer[T any](p *atomic.Pointer[T]) *T { return *(**T)(unsafe.Pointer(p)) }

// hintedLeaf is the leaf key's hint names, or nil.
func (t *latchedTree) hintedLeaf(r *latchedRoutes, dir []*wordNode, key uint64) *wordNode {
	n, _ := t.hintedLeafID(r, dir, key)
	return n
}

func (t *latchedTree) hintedLeafID(r *latchedRoutes, dir []*wordNode, key uint64) (*wordNode, uint32) {
	c := r.cell(key)
	if c == nil {
		return nil, 0
	}
	id := r.leafFor(c, key)
	if id == 0 {
		return nil, 0
	}
	return nodeIn(dir, id), id
}

// readHinted settles key in leaf n, whose latch word the caller loaded:
// the value and whether key is there, or false if n doesn't hold key or
// changed meanwhile.
func readHinted(n *wordNode, word, key uint64) (value uint64, found, ok bool) {
	if word&latchExclusive != 0 {
		return 0, false, false
	}
	m := after(n, word)
	count := int(m.n)
	low, high := *m.prefixWord(), m.high
	if count == 0 || count > nodeSize || !holds(low, high, key) {
		return 0, false, false
	}
	tags := m.tagWords()
	acc := tags[0] ^ tags[1] ^ tags[2] ^ tags[3] ^ uint64(count) ^ low ^ high
	i, candidate, read, proven := m.findOrdered(key, count)
	if !proven {
		return 0, false, false
	}
	acc ^= candidate ^ read ^ uint64(i)
	found = i < count && candidate == key
	if found {
		value = m.entries()[i&(nodeSize-1)].value
		acc ^= value
	}
	closing := *after((*uint64)(unsafe.Pointer(&n.sequence)), acc)
	return value, found, closing&^latchSharedMask == word&^latchSharedMask
}

// getHinted is get through a leaf hint; ok is false where it can't settle.
func (t *latchedTree) getHinted(key uint64) (value uint64, found, ok bool) {
	r, d := plainPointer(&t.routes), plainPointer(&t.blocks)
	if r == nil || d == nil {
		return 0, false, false
	}
	n := t.hintedLeaf(r, *d, key)
	if n == nil {
		return 0, false, false
	}
	return readHinted(n, *(*uint64)(unsafe.Pointer(&n.sequence)), key)
}

// getMany8 looks up eight keys at once, as upstream's get_many_copied: each
// stage runs for every key before the next, so their misses overlap. Each
// result is consistent on its own; a key its hint doesn't settle is looked
// up alone.
func (t *latchedTree) getMany8(keys *[batchLanes]uint64, dst *[batchLanes]Result[uint64]) {
	var nodes [batchLanes]*wordNode
	if seqlockPlainStores {
		if r, d := plainPointer(&t.routes), plainPointer(&t.blocks); r != nil && d != nil {
			dir := *d
			for j := range keys {
				nodes[j] = t.hintedLeaf(r, dir, keys[j])
			}
		}
	}
	var words [batchLanes]uint64
	for j, n := range nodes {
		if n != nil {
			words[j] = *(*uint64)(unsafe.Pointer(&n.sequence))
		}
	}
	for j, n := range nodes {
		if n != nil {
			if value, found, ok := readHinted(n, words[j], keys[j]); ok {
				dst[j] = Result[uint64]{value, found}
				continue
			}
		}
		dst[j].Value, dst[j].OK = t.get(keys[j])
	}
}

// upgradeHinted takes the leaf key's hint names exclusively, by a
// compare-and-swap at the latch word it read, after searching it: the
// writers' counterpart of getHinted. Its reads load after the latch word,
// by address, and the compare-and-swap succeeds only if no thread changed or
// held the leaf since, so they read the leaf as it is held. ok is false if
// no hint settles the key, or the leaf changed meanwhile.
func (t *latchedTree) upgradeHinted(key uint64) (leaf *wordNode, leafID uint32, version uint64, i int, ok bool) {
	if !seqlockPlainStores {
		return nil, 0, 0, 0, false
	}
	r, d := plainPointer(&t.routes), plainPointer(&t.blocks)
	if r == nil || d == nil {
		return nil, 0, 0, 0, false
	}
	n, id := t.hintedLeafID(r, *d, key)
	if n == nil {
		return nil, 0, 0, 0, false
	}
	word := *(*uint64)(unsafe.Pointer(&n.sequence))
	if word&(latchExclusive|latchSharedMask) != 0 {
		return nil, 0, 0, 0, false
	}
	m := after(n, word)
	if m.n == 0 || !holds(*m.prefixWord(), m.high, key) {
		return nil, 0, 0, 0, false
	}
	i = m.exactEntryRacing(key)
	if !n.tryLockAt(word) {
		return nil, 0, 0, 0, false
	}
	return n, id, word, i, true
}

// splitAtParentHint makes room for key in leaf, full and held from version,
// through the parent its hint names, without a descent: a split or a move
// to a sibling needs only the parent while it has room. The parent is taken
// at the version read before its children were, so it still holds the leaf
// where they said; a stale hint, a parent without room or one that changed
// meanwhile reports false, with nothing changed and the leaf still held.
// Otherwise everything is let go.
func (t *latchedTree) splitAtParentHint(leaf *wordNode, leafID uint32, version, key, value uint64) bool {
	parentID := leaf.directory
	if parentID == 0 || parentID&wordLeafID != 0 {
		return false
	}
	parent := t.node(parentID)
	if parent == nil {
		return false
	}
	parentVersion, ok := parent.tryReadVersion()
	if !ok {
		return false
	}
	count := loadCountRacing(parent)
	if count < 2 || count >= nodeSize {
		return false
	}
	edge := -1
	for e := 0; e < count; e++ {
		if loadChildRacing(parent, e) == leafID {
			edge = e
			break
		}
	}
	if edge < 0 {
		return false
	}
	var path treePath
	path.levels[0] = pathLevel{n: parent, id: parentID, version: parentVersion, edge: edge, count: count}
	path.depth = 1
	if t.redistribute(&path, leaf, key, value) {
		leaf.unlockExclusive(version)
		return true
	}
	return t.split(&path, leaf, leafID, version, key, value)
}

// getOrdered is get's optimistic descent with plain loads only
// (leafOrdered), and a read of its leaf as a hinted one is read.
func (t *latchedTree) getOrdered(key uint64) (value uint64, found, ok bool) {
	n, _, word, ok := t.leafOrdered(key)
	if !ok {
		return 0, false, false
	}
	return readHinted(n, word, key)
}

// leafOrdered finds key's leaf by an optimistic descent with plain loads
// only, ordered as lock coupling requires them: each node's words load
// after its latch word, by address; its child's latch word loads before its
// closing check, whose address depends on it; and the root's latch word
// before the root pointer is checked again. It returns the leaf and the
// latch word read, which held while the leaf's parent routed key to it, or
// false where a check fails. It fills hints as it passes leaf parents.
func (t *latchedTree) leafOrdered(key uint64) (leaf *wordNode, leafID uint32, word uint64, ok bool) {
	d := plainPointer(&t.blocks)
	if d == nil {
		return nil, 0, 0, false
	}
	dir := *d
	id := *(*uint32)(unsafe.Pointer(&t.root))
	n := nodeIn(dir, id)
	if n == nil {
		return nil, 0, 0, false
	}
	word = *(*uint64)(unsafe.Pointer(&n.sequence))
	if *after((*uint32)(unsafe.Pointer(&t.root)), word) != id {
		return nil, 0, 0, false
	}
	routes := plainPointer(&t.routes)
	if routes == nil || routes.root != id {
		// A tree built by appends alone made no table for this root.
		routes = t.refreshRoutes(dir)
	}
	var g *wordNode
	edge := 0
	for depth := 0; depth <= maxDepth; depth++ {
		if id&wordLeafID != 0 {
			return n, id, word, true
		}
		if word&latchExclusive != 0 {
			return nil, 0, 0, false
		}
		m := after(n, word)
		count := int(m.n)
		if count == 0 || count > nodeSize {
			return nil, 0, 0, false
		}
		if depth == 0 && routes != nil && count >= 2*routes.rootCount {
			// The root doubled its children: the hints get finer.
			routes = t.refreshRoutes(dir)
		}
		i, acc, proven := routeWindows(m, key, count)
		if !proven {
			return nil, 0, 0, false
		}
		child := m.inner().children[i&(nodeSize-1)]
		c := nodeIn(dir, child)
		if c == nil {
			return nil, 0, 0, false
		}
		childWord := *after((*uint64)(unsafe.Pointer(&c.sequence)), acc)
		closing := *after((*uint64)(unsafe.Pointer(&n.sequence)), acc^uint64(child)^uint64(count)^childWord)
		if closing&^latchSharedMask != word&^latchSharedMask {
			return nil, 0, 0, false
		}
		if child&wordLeafID != 0 {
			// The leaf's parent fills the key's hint cell, as the
			// descent does; hints are checked where used.
			if routes != nil {
				if cell := routes.cell(key); cell != nil {
					routes.fill(dir, cell, key, m, g, edge)
				}
			}
		}
		g, edge = m, i
		n, id, word = c, child, childWord
	}
	return nil, 0, 0, false
}

// upgradeOrdered is upgradeHinted through leafOrdered: it searches key's
// leaf after its latch word, by address, and takes the leaf by a
// compare-and-swap at that word, which settles the search.
func (t *latchedTree) upgradeOrdered(key uint64) (leaf *wordNode, leafID uint32, version uint64, i int, ok bool) {
	n, id, word, ok := t.leafOrdered(key)
	if !ok || word&(latchExclusive|latchSharedMask) != 0 {
		return nil, 0, 0, 0, false
	}
	i = after(n, word).exactEntryRacing(key)
	if !n.tryLockAt(word) {
		return nil, 0, 0, 0, false
	}
	return n, id, word, i, true
}
