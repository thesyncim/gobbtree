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

// ownerLeaf is key's leaf, by a descent with plain loads.
func (t *genericTree[K, V, O]) ownerLeaf(key K, code uint64) *gleaf[K, V] {
	h := *(**gheader)(unsafe.Pointer(&t.root))
	if prefixed[O, K]() {
		// A key without the shared prefix routes anywhere: it is in no
		// leaf, and a writer admits its key first.
		s := textView(&key)
		for !h.leaf {
			n := asInner[K](h)
			count := int(*(*int32)(unsafe.Pointer(&n.count)))
			p := int(*(*int32)(unsafe.Pointer(&n.prefix)))
			sum := summaryOf(s, p)
			sums := n.sums()
			lo := lowerSummary(sums, count, sum)
			i := max(lo-1, 0)
			if lo < count && sums[lo&(nodeSize-1)] == sum {
				// Summaries tie: the boxed separators settle it.
				i, _ = t.route(n, key, code, count, false)
			}
			h = *(**gheader)(unsafe.Pointer(&n.children[i&(nodeSize-1)]))
		}
		return asLeaf[K, V](h)
	}
	for !h.leaf {
		n := asInner[K](h)
		i, _ := t.route(n, key, code, int(*(*int32)(unsafe.Pointer(&n.count))), false)
		h = *(**gheader)(unsafe.Pointer(&n.children[i&(nodeSize-1)]))
	}
	return asLeaf[K, V](h)
}

// ownerFind is key's slot in leaf n and whether key is there: a text key
// that fits its windows by equality of its code, tail and length, as
// readTextLeaf settles it, and otherwise by the leaf's search.
func (t *genericTree[K, V, O]) ownerFind(n *gleaf[K, V], key K, code uint64) (int, bool) {
	if prefixed[O, K]() {
		count := owned(&n.count)
		p := owned(&n.prefix)
		s := textView(&key)
		if p <= 16 && len(s) <= p+16 && len(s) < 255 {
			if len(s) < p || (stringCode(s, 0)^*(*uint64)(unsafe.Pointer(&n.head[0])))&headMask(p) != 0 || p > 8 && (stringCode(s, 8)^*(*uint64)(unsafe.Pointer(&n.head[1])))&headMask(p-8) != 0 {
				return t.find(n, key, code)
			}
			c := stringCode(s, p)
			lo := lowerCode(&n.codes, count, c)
			if j := lo & (nodeSize - 1); lo < count && n.codes[j] == c {
				text := n.text()
				tail, ok := fastWindow(s, p+8)
				if !ok {
					tail = stringCode(s, p+8)
				}
				if text.tails[j] == tail && text.lens[j] == textLen(s) {
					return lo, true
				}
			} else {
				// No key of the code: absent, and lo is where it goes.
				return lo, false
			}
		}
	}
	return t.find(n, key, code)
}

func (t *genericTree[K, V, O]) ownerGet(key K) (value V, found bool) {
	if plainPointer(&t.root) == nil {
		return value, false
	}
	code := uint64(0)
	if !prefixed[O, K]() {
		code = t.code(key)
	}
	n := t.ownerLeaf(key, code)
	if i, ok := t.ownerFind(n, key, code); ok {
		return n.values[i], true
	}
	return value, false
}

func (t *genericTree[K, V, O]) ownerRead(key K, visit func(K, V)) bool {
	if plainPointer(&t.root) == nil {
		return false
	}
	code := t.code(key)
	n := t.ownerLeaf(key, code)
	i, ok := t.ownerFind(n, key, code)
	if ok {
		visit(n.keys[i], n.values[i])
	}
	return ok
}

func (t *genericTree[K, V, O]) ownerUpdate(key K, change func(K, V) V) (V, bool) {
	var zero V
	if plainPointer(&t.root) == nil {
		return zero, false
	}
	code := t.code(key)
	n := t.ownerLeaf(key, code)
	i, ok := t.ownerFind(n, key, code)
	if !ok {
		return zero, false
	}
	value := change(n.keys[i], n.values[i])
	n.values[i] = value
	return value, true
}

// ownerInsert is insert into an owner's tree: into key's leaf if it holds
// key or has room, else by the latched split.
func (t *genericTree[K, V, O]) ownerInsert(key K, value V, policy int, merge func(K, V, V) V) (V, bool) {
	if plainPointer(&t.root) == nil {
		t.start()
	}
	code := uint64(0)
	if !prefixed[O, K]() {
		code = t.code(key)
	}
	n := t.ownerLeaf(key, code)
	i, found := t.ownerFind(n, key, code)
	if !found && prefixed[O, K]() && t.sharedOrder(key) != 0 {
		// A new key without the shared prefix shortens it, which refits
		// the borders its route may have crossed: route it again.
		t.admit(key)
		n = t.ownerLeaf(key, code)
		i, found = t.ownerFind(n, key, code)
	} else if !found && prefixed[O, K]() && plainPointer(&t.shared) == nil {
		t.admit(key)
	}
	if found {
		old := n.values[i]
		switch policy {
		case insertReplace:
			n.values[i] = value
		case insertMerge:
			n.values[i] = merge(n.keys[i], old, value)
		}
		return old, true
	}
	if owned(&n.count) < nodeSize {
		t.insertEntry(n, i, ownKey[O](key), value)
		var zero V
		return zero, false
	}
	return t.insertSplitting(key, code, value, policy, merge)
}

// ownerRemove is remove from an owner's tree: from its leaf where the leaf
// keeps its minimum or is the root, else by the latched rebalancing.
func (t *genericTree[K, V, O]) ownerRemove(seek int, key K, condition func(K, V) bool) (K, V, bool) {
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
		code = t.code(key)
		n = t.ownerLeaf(key, code)
		i, ok = t.ownerFind(n, key, code)
	} else {
		n, _ = t.descend(key, 0, seek, false, optimistic, nil)
		i, ok = t.target(n, seek, key, 0)
	}
	if !ok || condition != nil && !condition(n.keys[i], n.values[i]) {
		return k, v, false
	}
	if owned(&n.count) > leafMinLen || plainPointer(&t.root) == &n.gheader {
		k, v = t.removeEntry(n, i)
		return k, v, true
	}
	for {
		if k, v, removed, done := t.removeRebalancing(seek, key, code, condition); done {
			return k, v, removed
		}
	}
}
