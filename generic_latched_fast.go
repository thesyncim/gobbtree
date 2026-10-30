package gobbtree

import (
	"strings"
	"unsafe"
)

// The generic tree's fast lookups, a tight loop of the general descent's
// checks: an integer node's latch word is loaded plainly and feeds the
// address of every load of the node after it, which the core then issues
// in order without holding later lookups' loads, as an acquire would; a
// text node's is an acquire. Every word a route or a leaf's answer depends
// on feeds the address of the node's closing check, which therefore loads
// after them; a child's load waits only for the route. Anything
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
		// The child is read once the route is known: the reads that only
		// bracket it join the closing check's dependency, not the child's.
		child := *(**gheader)(unsafe.Pointer(&n.children[i&(nodeSize-1)]))
		if child == nil {
			return nil, 0, false
		}
		childWord := child.latch.Load()
		acc := uint64(i) ^ uint64(count) ^ uint64(p)<<8 ^ uint64(below)<<16 ^ uint64(next)<<32
		closing := *after((*uint64)(unsafe.Pointer(&h.latch)), acc^childWord^uint64(uintptr(unsafe.Pointer(child))))
		if closing&^latchSharedMask != word&^latchSharedMask {
			return nil, 0, false
		}
		h, word = child, childWord
	}
	return nil, 0, false
}

// getTextOrdered is get through descendTextOrdered: where its code is
// key's, the slot's tail and length settle key there, and its value is copied before the leaf's closing check, which the
// words read feed. false where it can't settle.
func (t *genericTree[K, V, O]) getTextOrdered(key K) (value V, found, ok bool) {
	s := textView(&key)
	h, word, lo, state, acc, ok := descendTextOrdered((**gheader)(unsafe.Pointer(&t.root)), s)
	if !ok {
		return value, false, false
	}
	n := asLeaf[K, V](h)
	if state == orderedCode {
		p := int(*(*int32)(unsafe.Pointer(&n.prefix)))
		tail, fast := fastWindow(s, p+8)
		if !fast {
			tail = stringCode(s, p+8)
		}
		slot := n.text().slots[lo&(nodeSize-1)]
		acc ^= slot.tail<<3 ^ slot.ref<<5
		if slot.tail != tail || refLen(slot.ref) != len(s) {
			// Another key of the same code: key may follow it.
			return value, false, false
		}
		found = true
		value = n.values[lo&(nodeSize-1)]
		if unsafe.Sizeof(value) == 8 {
			acc ^= *(*uint64)(unsafe.Pointer(&value))
		} else {
			loadFence()
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

// codeLeafOrdered is textLeafOrdered for keys that codes order exactly,
// by code, from the root at root: an internal node's codes are its
// separators', its first 0, and its route the last child whose separator
// is at most code, which a search of every slot feeds the closing check.
//
// It records in f, where not nil, the leaf's parent and the fences its
// route read, for a hint's fill.
func codeLeafOrdered(root **gheader, code uint64, f *hintFrame) (*gheader, uint64, bool) {
	h := *root
	if h == nil {
		return nil, 0, false
	}
	word := *(*uint64)(unsafe.Pointer(&h.latch))
	if *after(root, word) != h {
		return nil, 0, false
	}
	low, high := uint64(0), uint64(0)
	for depth := 0; depth < maxDepth; depth++ {
		if word&latchExclusive != 0 {
			return nil, 0, false
		}
		if h.leaf {
			return h, word, true
		}
		n := (*innerRoute)(unsafe.Pointer(after(h, word)))
		count := int(*(*int32)(unsafe.Pointer(&n.count)))
		if count < 1 || count > nodeSize {
			return nil, 0, false
		}
		codes := (*[nodeSize]uint64)(unsafe.Pointer(&n.codes))
		i := routeCode(codes, count, code)
		acc := uint64(i) ^ uint64(count)<<8
		if f != nil {
			f.push(n, i, low, high)
			if i > 0 {
				low = codes[i&(nodeSize-1)]
			}
			if i+1 < count {
				high = codes[(i+1)&(nodeSize-1)]
			}
		}
		child := *after((**gheader)(unsafe.Pointer(&n.children[i&(nodeSize-1)])), acc)
		if child == nil {
			return nil, 0, false
		}
		childWord := *(*uint64)(unsafe.Pointer(&child.latch))
		closing := *after((*uint64)(unsafe.Pointer(&h.latch)), acc^childWord^uint64(uintptr(unsafe.Pointer(child))))
		if closing&^latchSharedMask != word&^latchSharedMask {
			return nil, 0, false
		}
		h, word = child, childWord
	}
	return nil, 0, false
}

// readHintedCode settles code in leaf n, which its hint names: its value
// and whether it is there, or false where n doesn't hold it between its
// first and last keys, or changed meanwhile. Its latch word is loaded
// plainly, and feeds the addresses of the words it guards, which all feed
// the closing check's.
func (t *genericTree[K, V, O]) readHintedCode(n *gleaf[K, V], code uint64) (value V, found, ok bool) {
	return t.readHintedAt(n, *(*uint64)(unsafe.Pointer(&n.latch)), code)
}

// readHintedAt is readHintedCode at latch word word, which the caller
// loaded plainly.
func (t *genericTree[K, V, O]) readHintedAt(n *gleaf[K, V], word, code uint64) (value V, found, ok bool) {
	if word&latchExclusive != 0 {
		return value, false, false
	}
	m := after(n, word)
	count := int(*(*int32)(unsafe.Pointer(&m.count)))
	if count <= 0 || count > nodeSize {
		return value, false, false
	}
	// Code's slot by its tag; where none is its, or another code has it,
	// code is absent only between the leaf's first key and its last.
	lo := firstTag(&m.tags, count, code)
	acc := uint64(count) ^ uint64(lo)<<8
	var c uint64
	if lo < count {
		c = m.codes[lo&(nodeSize-1)]
		acc ^= c
	}
	if lo == count || c != code {
		first, high := bounds(&m.gheader)
		if !owns(count, first, high, code) {
			return value, false, false
		}
		acc ^= first<<1 ^ high<<2
		if lo < count {
			lo = lowerCode(&m.codes, count, code)
			c = m.codes[lo&(nodeSize-1)]
			acc ^= uint64(lo)<<16 ^ c<<3
		}
	}
	if lo < count && c == code {
		found = true
		value = m.values[lo&(nodeSize-1)]
		if unsafe.Sizeof(value) == 8 {
			acc ^= *(*uint64)(unsafe.Pointer(&value))
		} else {
			loadFence()
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

// getMany8 looks up eight integer keys at once, as upstream's
// get_many_copied: each stage runs for every key
// before the next, so their misses overlap, from the hint cells to the
// leaves' directory slots, their latch words and their entries. Each
// result is consistent on its own; a key its hint doesn't settle is looked
// up alone.
func (t *genericTree[K, V, O]) getMany8(keys *[batchLanes]K, dst *[batchLanes]Result[V]) {
	var codes [batchLanes]uint64
	var ids [batchLanes]uint32
	var leaves [batchLanes]*gheader
	for j := range keys {
		codes[j] = numericKey[O](keys[j])
	}
	if r := plainPointer(&t.hints); r != nil && (t.owner || seqlockPlainStores) {
		for j, code := range codes {
			ids[j] = r.hintID(code)
		}
		for j, id := range ids {
			if id != 0 {
				leaves[j] = t.leaves.leaf(id)
			}
		}
	}
	if t.owner {
		for j, h := range leaves {
			if h != nil {
				if i, found, settled := settleCode(h, codes[j], false); settled {
					var value V
					if found {
						value = asLeaf[K, V](h).values[i&(nodeSize-1)]
					}
					dst[j] = Result[V]{value, found}
					continue
				}
			}
			dst[j].Value, dst[j].OK = t.ownerGet(keys[j])
		}
		return
	}
	var words [batchLanes]uint64
	for j, h := range leaves {
		if h != nil {
			words[j] = *(*uint64)(unsafe.Pointer(&h.latch))
		}
	}
	for j, h := range leaves {
		if h != nil {
			if value, found, ok := t.readHintedAt(asLeaf[K, V](h), words[j], codes[j]); ok {
				dst[j] = Result[V]{value, found}
				continue
			}
		}
		dst[j].Value, dst[j].OK = t.get(keys[j])
	}
}

// pushCode puts code, a key its codes hold whole, and value last in leaf n,
// of count entries and room: nothing shifts.
func (n *gleaf[K, V]) pushCode(count int, code uint64, value V) {
	i := count & (nodeSize - 1)
	n.codes[i], n.tags[i], n.values[i] = code, uint8(code), value
	n.count.Store(int32(count + 1))
}

// appendTail is insert of key, past every key the tree holds, into its
// rightmost leaf, as appends make the next key: the leaf t.tail names,
// where it still has no high fence, and so is the rightmost, and holds a
// last key below code. With room, key goes in last, in an owner's tree in
// place, in a Map by a compare-and-swap at the latch word its reads
// followed, as hinted writes take theirs: done. Without, full is the leaf,
// for the caller to split at its end, unsearched. Neither where it can't.
//
// pastTail tells the keys for it: code past the last of the tail, read
// racing, for appendTail to settle. Inserts check it only where the hints
// place no leaf, as for an append past the table's domain, which they
// fail at once: on a large tree, a replace that checked the tail first,
// or called appendTail, took longer.
func (t *genericTree[K, V, O]) pastTail(code uint64) bool {
	h := plainPointer(&t.tail)
	if h == nil {
		return false
	}
	count := int(*(*int32)(unsafe.Pointer(&h.count)))
	return count > 0 && (*leafRoute)(unsafe.Pointer(h)).codes[(count-1)&(nodeSize-1)] < code
}

func (t *genericTree[K, V, O]) appendTail(key K, code uint64, value V) (done bool, full *gleaf[K, V]) {
	h := plainPointer(&t.tail)
	if h == nil {
		return false, nil
	}
	word := *(*uint64)(unsafe.Pointer(&h.latch))
	if !t.owner && word&(latchExclusive|latchSharedMask) != 0 {
		return false, nil
	}
	m := after(h, word)
	count := int(*(*int32)(unsafe.Pointer(&m.count)))
	if _, high := bounds(m); high != 0 || count <= 0 || count > nodeSize || (*leafRoute)(unsafe.Pointer(m)).codes[(count-1)&(nodeSize-1)] >= code {
		return false, nil
	}
	n := asLeaf[K, V](h)
	if count == nodeSize {
		return false, n
	}
	if !t.owner && !h.latch.tryLockAt(word) {
		return false, nil
	}
	if t.wholeCodes() {
		n.pushCode(count, code, value)
	} else {
		t.insertEntry(n, count, key, value)
	}
	if !t.owner {
		h.latch.unlockExclusive(word)
	}
	return true, nil
}

// seekHinted searches the leaf code's hint names, with plain loads after
// its latch word, which it returns, unheld: code's slot and whether it is there, false where
// the leaf is held, or code lies outside it or at either of its ends
// without being there. A compare-and-swap at the word, which succeeds only
// where nothing changed the leaf since, makes the search hold.
func (t *genericTree[K, V, O]) seekHinted(code uint64) (n *gleaf[K, V], word uint64, count, i int, found, ok bool) {
	r := plainPointer(&t.hints)
	if r == nil {
		return nil, 0, 0, 0, false, false
	}
	h := r.hintedLeaf(&t.leaves, code)
	if h == nil {
		return nil, 0, 0, 0, false, false
	}
	n = asLeaf[K, V](h)
	word = *(*uint64)(unsafe.Pointer(&n.latch))
	if word&(latchExclusive|latchSharedMask) != 0 {
		return nil, 0, 0, 0, false, false
	}
	m := after(n, word)
	count = int(*(*int32)(unsafe.Pointer(&m.count)))
	if count <= 0 || count > nodeSize {
		return nil, 0, 0, 0, false, false
	}
	i = firstTag(&m.tags, count, code)
	if i < count && m.codes[i&(nodeSize-1)] == code {
		return n, word, count, i, true, true
	}
	if first, high := bounds(&m.gheader); !owns(count, first, high, code) {
		return nil, 0, 0, 0, false, false
	}
	i = lowerCode(&m.codes, count, code)
	return n, word, count, i, i < count && m.codes[i&(nodeSize-1)] == code, true
}

// insertHinted is insert through key's hint, for a policy that runs no
// user code: the leaf is searched, then taken at the word the search read,
// and changed where the search found key, or room for it. done is false
// where it can't settle; full, where the leaf it settled in is full, for
// the caller to split it.
func (t *genericTree[K, V, O]) insertHinted(key K, code uint64, value V, policy int) (old V, found, done, full bool) {
	// seekHinted, written out: a write's calls before its store cost it.
	r := plainPointer(&t.hints)
	if r == nil {
		return old, false, false, false
	}
	h := r.hintedLeaf(&t.leaves, code)
	if h == nil {
		return old, false, false, false
	}
	n := asLeaf[K, V](h)
	word := *(*uint64)(unsafe.Pointer(&n.latch))
	if word&(latchExclusive|latchSharedMask) != 0 {
		return old, false, false, false
	}
	m := after(n, word)
	count := int(*(*int32)(unsafe.Pointer(&m.count)))
	if count <= 0 || count > nodeSize {
		return old, false, false, false
	}
	i := firstTag(&m.tags, count, code)
	found = i < count && m.codes[i&(nodeSize-1)] == code
	if !found {
		if first, high := bounds(&m.gheader); !owns(count, first, high, code) {
			return old, false, false, false
		}
		i = lowerCode(&m.codes, count, code)
		found = i < count && m.codes[i&(nodeSize-1)] == code
	}
	if !found && count == nodeSize {
		return old, false, false, true
	}
	if !n.latch.tryLockAt(word) {
		return old, false, false, false
	}
	i &= nodeSize - 1
	if found {
		old = n.values[i]
		if policy == insertReplace {
			n.values[i] = value
		}
	} else {
		t.insertEntry(n, i, key, value)
	}
	n.latch.unlockExclusive(word)
	return old, found, true, false
}

// removeHinted is remove of key through its hint: the leaf is searched,
// then taken at the word the search read, where it holds key and isn't
// left underfull. Its first key goes too: exact codes separate leaves
// without it.
func (t *genericTree[K, V, O]) removeHinted(code uint64) (k K, v V, done bool) {
	n, word, count, i, found, ok := t.seekHinted(code)
	if !ok || !found || count <= leafMinLen || i == 0 && !t.whole || !n.latch.tryLockAt(word) {
		return k, v, false
	}
	k, v = t.removeEntry(n, i&(nodeSize-1), true)
	n.latch.unlockExclusive(word)
	return k, v, true
}

// hintedLatched is code's leaf latched in mode through its hint, and the
// version held: the leaf is taken by a compare-and-swap at the latch word
// read, and kept where it holds code between its first and last keys.
func (t *genericTree[K, V, O]) hintedLatched(code uint64, mode int) (*gleaf[K, V], uint64, bool) {
	r := plainPointer(&t.hints)
	if r == nil {
		return nil, 0, false
	}
	h := r.hintedLeaf(&t.leaves, code)
	if h == nil {
		return nil, 0, false
	}
	n := asLeaf[K, V](h)
	word := *(*uint64)(unsafe.Pointer(&n.latch))
	version := uint64(0)
	switch {
	case mode == exclusive && word&(latchExclusive|latchSharedMask) == 0 && n.latch.tryLockAt(word):
		version = word
	case mode == shared && word&latchExclusive == 0 && n.latch.CompareAndSwap(word, word+latchSharedUnit):
	default:
		return nil, 0, false
	}
	if first, high := bounds(&n.gheader); owns(int(n.count.Load()), first, high, code) {
		return n, version, true
	}
	unlatchHeader(&n.gheader, mode, version)
	return nil, 0, false
}

// readCodeLeaf settles key in leaf n, whose latch word the caller read, for
// keys that codes order exactly: key is
// in leaf n exactly where its code is, the slot every code below it
// counts to, which feeds the closing check.
func (t *genericTree[K, V, O]) readCodeLeaf(n *gleaf[K, V], word, code uint64) (value V, found, ok bool) {
	if word&latchExclusive != 0 {
		return value, false, false
	}
	m := after(n, word)
	count := int(*(*int32)(unsafe.Pointer(&m.count)))
	if count < 0 || count > nodeSize {
		return value, false, false
	}
	// Code's slot by its tag, all of which feed the check: where none is
	// its, code is absent; where another code has it, the codes settle it.
	lo := firstTag(&m.tags, count, code)
	acc := uint64(count) ^ uint64(lo)<<8
	var c uint64
	if lo < count {
		// A miss whose tag no slot has reads no code.
		c = m.codes[lo&(nodeSize-1)]
		acc ^= c
		if c != code {
			lo = lowerCode(&m.codes, count, code)
			c = m.codes[lo&(nodeSize-1)]
			acc ^= uint64(lo)<<16 ^ c<<3
		}
	}
	if lo < count && c == code {
		found = true
		value = m.values[lo&(nodeSize-1)]
		if unsafe.Sizeof(value) == 8 {
			acc ^= *(*uint64)(unsafe.Pointer(&value))
		} else {
			loadFence()
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

// seekText finds text key's slot in leaf n, whose latch word the caller
// loaded, with plain loads after it, by windows alone: the slot and whether
// key is there, false where the windows don't settle it, the leaf is
// empty, or key lacks its prefix. No key's bytes are read: a compare-and-
// swap at the word, which holds only where nothing changed the leaf since,
// makes the search hold, as seekHinted's does.
func (t *genericTree[K, V, O]) seekText(n *gleaf[K, V], word uint64, key K) (i int, found, ok bool) {
	n = after(n, word)
	count := int(*(*int32)(unsafe.Pointer(&n.count)))
	p := int(*(*int32)(unsafe.Pointer(&n.prefix)))
	if count <= 0 || count > nodeSize || p > 16 {
		return 0, false, false
	}
	s := textView(&key)
	h0 := *(*uint64)(unsafe.Pointer(&n.head[0]))
	h1 := *(*uint64)(unsafe.Pointer(&n.head[1]))
	if len(s) < p || headDiff(stringCode(s, 0), headWindow(s), p, h0, h1) != 0 {
		return 0, false, false
	}
	k := probeText(s, p)
	if !k.settles {
		return 0, false, false
	}
	lo, hi := tieRange(&n.codes, count, k.code)
	for i = lo; i < hi; i++ {
		// A probe that fits its windows settles every slot's order.
		if order, _ := slotOrder(n, i, &k, p); order >= 0 {
			return i, order == 0, true
		}
	}
	return hi, false, true
}

// seekTextOrdered is key's leaf by descendTextOrdered, its latch word and
// key's slot there, and whether key is there: settled by windows, as
// seekText settles it, false where they don't settle it or key lacks the
// leaf's prefix. The slot's tail and length settle a code of key's; a
// compare-and-swap at the word makes the search hold.
func (t *genericTree[K, V, O]) seekTextOrdered(key K) (n *gleaf[K, V], word uint64, i int, found, ok bool) {
	s := textView(&key)
	h, word, lo, state, _, ok := descendTextOrdered((**gheader)(unsafe.Pointer(&t.root)), s)
	if !ok || state == orderedOutside || word&(latchExclusive|latchSharedMask) != 0 {
		return nil, 0, 0, false, false
	}
	n = asLeaf[K, V](h)
	if state == orderedAbsent {
		return n, word, lo, false, true
	}
	p := int(*(*int32)(unsafe.Pointer(&n.prefix)))
	tail, fast := fastWindow(s, p+8)
	if !fast {
		tail = stringCode(s, p+8)
	}
	if slot := n.text().slots[lo&(nodeSize-1)]; slot.tail == tail && refLen(slot.ref) == len(s) {
		return n, word, lo, true, true
	}
	// Another key of key's code: the windows order them.
	i, found, ok = t.seekText(n, word, key)
	return n, word, i, found, ok
}

// insertText is insert of text key through the ordered descent, for a
// policy that runs no user code: its leaf is searched by windows, then
// taken at the word the descent read, and changed where the search found
// key, or room for it. done is false where it can't settle; full, where
// the leaf it settled in is full, for the caller to split it.
func (t *genericTree[K, V, O]) insertText(key K, value V, policy int) (old V, found, done, full bool) {
	n, word, i, found, ok := t.seekTextOrdered(key)
	if !ok {
		return old, false, false, false
	}
	if !found {
		// A key the tree holds has the shared prefix; a new one goes in
		// here only with it, as admit would find it.
		if shared := t.shared.Load(); shared == nil || !strings.HasPrefix(textView(&key), *shared) {
			return old, false, false, false
		}
		if int(*(*int32)(unsafe.Pointer(&n.count))) == nodeSize {
			return old, false, false, true
		}
	}
	if !n.latch.tryLockAt(word) {
		return old, false, false, false
	}
	i &= nodeSize - 1
	if found {
		old = n.values[i]
		if policy == insertReplace {
			n.values[i] = value
		}
	} else {
		t.insertEntry(n, i, key, value)
	}
	n.latch.unlockExclusive(word)
	return old, found, true, false
}

// removeText is remove of text key through the ordered descent: the leaf is
// searched by windows, then taken at the word the descent read, where it
// holds key, not its first, which may be a separator, and isn't left
// underfull.
func (t *genericTree[K, V, O]) removeText(key K) (k K, v V, done bool) {
	n, word, i, found, ok := t.seekTextOrdered(key)
	if !ok || !found || i == 0 || int(*(*int32)(unsafe.Pointer(&n.count))) <= leafMinLen || !n.latch.tryLockAt(word) {
		return k, v, false
	}
	k, v = t.removeEntry(n, i&(nodeSize-1), true)
	n.latch.unlockExclusive(word)
	return k, v, true
}

// leafLatched is key's leaf latched in mode, shared or exclusive, and the
// version held: through the ordered descent and a compare-and-swap at the
// latch word it read, or
// else by a latching descent. A swap that succeeds finds the leaf as the
// descent's checks vouched for it, so the leaf is still key's.
func (t *genericTree[K, V, O]) leafLatched(key K, code uint64, seek int, mode int) (*gleaf[K, V], uint64) {
	if seqlockPlainStores && t.wordCodes() && seek == seekKey {
		if n, version, ok := t.hintedLatched(code, mode); ok {
			return n, version
		}
		var f hintFrame
		if h, word, ok := codeLeafOrdered((**gheader)(unsafe.Pointer(&t.root)), code, &f); ok {
			currentHints(&t.hints, &t.making, &t.reshaped, plainPointer(&t.root)).fillFrom(&f, code)
			n := asLeaf[K, V](h)
			switch {
			case mode == exclusive && word&(latchExclusive|latchSharedMask) == 0 && n.latch.tryLockAt(word):
				return n, word
			case mode == shared && word&latchExclusive == 0 && n.latch.CompareAndSwap(word, word+latchSharedUnit):
				return n, 0
			}
		}
	}
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
