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
// lookup needs only equality: key, if it has the leaf's prefix and fits its
// windows, is there exactly where a slot's code, tail and length are its.
// A word-sized value feeds the closing check's address; a wider one is
// ordered before it by a load fence.
func (t *genericTree[K, V, O]) readTextLeaf(n *gleaf[K, V], word uint64, key K) (value V, found, ok bool) {
	if word&latchExclusive != 0 {
		return value, false, false
	}
	count := int(*(*int32)(unsafe.Pointer(&n.count)))
	p := int(*(*int32)(unsafe.Pointer(&n.prefix)))
	if count < 0 || count > nodeSize || p > 16 {
		return value, false, false
	}
	s := textView(&key)
	h0 := *(*uint64)(unsafe.Pointer(&n.head[0]))
	h1 := *(*uint64)(unsafe.Pointer(&n.head[1]))
	acc := uint64(count) ^ uint64(p)<<8 ^ h0 ^ h1<<1
	// Key has the leaf's prefix where its first p bytes are the head's.
	shares := sharesHead(s, p, h0, h1)
	if shares {
		code, length := stringCode(s, p), textLen(s)
		tail, fast := fastWindow(s, p+8)
		if !fast {
			tail = stringCode(s, p+8)
		}
		if len(s) > p+16 || length == 255 {
			// Too long to settle by windows.
			return value, false, false
		}
		lo := lowerCode(&n.codes, count, code) & (nodeSize - 1)
		c := n.codes[lo]
		text := n.text()
		slotTail, slotLen := text.tails[lo], text.lens[lo]
		acc ^= uint64(lo)<<40 ^ c ^ slotTail<<3 ^ uint64(slotLen)<<48
		// A slot past the count holds the padding, a code a key may share.
		if lo < count && c == code {
			if slotTail != tail || slotLen != length {
				// Another key of the same code: key may follow it.
				return value, false, false
			}
			found = true
			value = n.values[lo]
			if unsafe.Sizeof(value) == 8 {
				acc ^= *(*uint64)(unsafe.Pointer(&value))
			} else {
				loadFence()
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

// leafLatched is key's leaf latched in mode, shared or exclusive, and the
// version held: through the ordered descent and a compare-and-swap at the
// latch word it read, as the word tree's upgradeOrdered takes its leaf, or
// else by a latching descent. A swap that succeeds finds the leaf as the
// descent's checks vouched for it, so the leaf is still key's.
func (t *genericTree[K, V, O]) leafLatched(key K, code uint64, seek int, mode int) (*gleaf[K, V], uint64) {
	if optimisticText[O, K]() && seek == seekKey {
		if n, word, ok := t.textLeafOrdered(key); ok {
			switch {
			case mode == exclusive && word&(latchExclusive|latchSharedMask) == 0 && n.latch.tryLockAt(word):
				return n, word
			case mode == shared && word&latchExclusive == 0 && n.latch.CompareAndSwap(word, word+latchSharedUnit):
				return n, 0
			}
		}
	}
	return t.descend(key, code, seek, false, mode, nil)
}
