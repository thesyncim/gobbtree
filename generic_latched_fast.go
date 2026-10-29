package gobbtree

import "unsafe"

// The generic tree's text lookups, a tight loop of the general descent's
// checks, as the word tree's leafOrdered and readHinted are
// (word_latched_fast.go): a node's latch word is an acquire load, which
// orders its plain loads after it while letting the core issue them at
// once; every word a route or a leaf's answer depends on feeds the address
// of the node's closing check, which therefore loads after them. Anything
// unusual, a summary or code tie, a long leaf prefix, a held latch, a
// version that moved, sends the lookup the general way. Only plain-store
// builds take them.

// textLeafOrdered finds text key's leaf by an optimistic descent with plain
// loads, returning it and the latch word read, which held while its parent
// routed key to it; false where it can't. A key without the shared prefix
// is in no leaf, so whichever one its route reaches settles it absent.
func (t *genericTree[K, V, O]) textLeafOrdered(key K) (*gleaf[K, V], uint64, bool) {
	s := textView(&key)
	root := (**gheader)(unsafe.Pointer(&t.root))
	h := *root
	if h == nil {
		return nil, 0, false
	}
	word := h.latch.Load()
	if *after(root, word) != h {
		return nil, 0, false
	}
	for depth := 0; depth < maxDepth; depth++ {
		if word&latchExclusive != 0 {
			return nil, 0, false
		}
		if h.leaf {
			return asLeaf[K, V](h), word, true
		}
		n := asInner[K](h)
		count := int(*(*int32)(unsafe.Pointer(&n.count)))
		if count < 1 || count > nodeSize {
			return nil, 0, false
		}
		p := int(*(*int32)(unsafe.Pointer(&n.prefix)))
		sum := summaryOf(s, p)
		sums := n.sums()
		i := max(lowerSummary(sums, count, sum)-1, 0)
		// The summaries that bracket the route prove it in the snapshot
		// the closing check vouches for; one equal to key's is a tie.
		below, next := sums[i&(nodeSize-1)], sums[(i+1)&(nodeSize-1)]
		if i > 0 && below >= sum || i+1 < count && next <= sum {
			return nil, 0, false
		}
		acc := uint64(i) ^ uint64(count) ^ uint64(p)<<8 ^ uint64(below)<<16 ^ uint64(next)<<32
		child := *after((**gheader)(unsafe.Pointer(&n.children[i&(nodeSize-1)])), acc)
		if child == nil {
			return nil, 0, false
		}
		childWord := child.latch.Load()
		closing := *after((*uint64)(unsafe.Pointer(&h.latch)), acc^childWord^uint64(uintptr(unsafe.Pointer(child))))
		if closing&^latchSharedMask != word&^latchSharedMask {
			return nil, 0, false
		}
		h, word = child, childWord
	}
	return nil, 0, false
}

// readTextLeaf settles text key in leaf n, whose latch word the caller
// read: its value and whether key is there, or false where it can't. A
// word-sized value feeds the closing check's address; a wider one is
// ordered before it by a load fence.
func (t *genericTree[K, V, O]) readTextLeaf(n *gleaf[K, V], word uint64, key K) (value V, found, ok bool) {
	if word&latchExclusive != 0 {
		return value, false, false
	}
	m := n
	count := int(*(*int32)(unsafe.Pointer(&m.count)))
	p := int(*(*int32)(unsafe.Pointer(&m.prefix)))
	if count < 0 || count > nodeSize || p > 16 {
		return value, false, false
	}
	s := textView(&key)
	h0 := *(*uint64)(unsafe.Pointer(&m.head[0]))
	h1 := *(*uint64)(unsafe.Pointer(&m.head[1]))
	acc := uint64(count) ^ uint64(p)<<8 ^ h0 ^ h1<<1
	if p == 0 || headOrder(s, p, h0, h1) == 0 {
		k := probeText(s, p)
		lo := lowerCode(&m.codes, count, k.code)
		c0, c1 := m.codes[lo&(nodeSize-1)], m.codes[(lo+1)&(nodeSize-1)]
		acc ^= uint64(lo)<<40 ^ c0 ^ c1<<2
		if lo < count && c0 == k.code {
			if lo+1 < count && c1 == k.code {
				return value, false, false
			}
			text := m.text()
			tail, length := text.tails[lo&(nodeSize-1)], text.lens[lo&(nodeSize-1)]
			acc ^= tail<<3 ^ uint64(length)<<48
			order, settled := slotOrder(m, lo, &k, p)
			if !settled {
				return value, false, false
			}
			if order == 0 {
				found = true
				value = m.values[lo&(nodeSize-1)]
				if unsafe.Sizeof(value) == 8 {
					acc ^= *(*uint64)(unsafe.Pointer(&value))
				} else {
					loadFence()
				}
			}
		}
	}
	closing := *after((*uint64)(unsafe.Pointer(&n.latch)), acc)
	if closing&^latchSharedMask != word&^latchSharedMask {
		return value, false, false
	}
	if !found {
		var zero V
		return zero, false, true
	}
	return value, true, true
}
