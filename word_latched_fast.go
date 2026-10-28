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

// hintedLeaf is the leaf key's hint names, with its latch word, or nil.
func (t *latchedTree) hintedLeaf(r *latchedRoutes, dir []*wordNode, key uint64) *wordNode {
	c := r.cell(key)
	if c == nil {
		return nil
	}
	id := r.leafFor(c, key)
	if id == 0 {
		return nil
	}
	return nodeIn(dir, id)
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
func (t *latchedTree) upgradeHinted(key uint64) (leaf *wordNode, version uint64, i int, ok bool) {
	if !seqlockPlainStores {
		return nil, 0, 0, false
	}
	r, d := plainPointer(&t.routes), plainPointer(&t.blocks)
	if r == nil || d == nil {
		return nil, 0, 0, false
	}
	n := t.hintedLeaf(r, *d, key)
	if n == nil {
		return nil, 0, 0, false
	}
	word := *(*uint64)(unsafe.Pointer(&n.sequence))
	if word&(latchExclusive|latchSharedMask) != 0 {
		return nil, 0, 0, false
	}
	m := after(n, word)
	if m.n == 0 || !holds(*m.prefixWord(), m.high, key) {
		return nil, 0, 0, false
	}
	i = m.exactEntry(key)
	if !n.tryLockAt(word) {
		return nil, 0, 0, false
	}
	return n, word, i, true
}
