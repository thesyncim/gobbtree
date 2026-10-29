package gobbtree

import (
	"math"
	"strings"
	"unsafe"
)

// latchSet is the nodes an operation takes besides its leaf, upstream's
// HeldNodes, for the generic tree: internal nodes by a compare-and-swap at
// a version, leaves exclusively, all without waiting.
type latchSet struct {
	nodes    [5*maxDepth + 4]*gheader
	versions [5*maxDepth + 4]uint64
	len      int
}

func (s *latchSet) holds(h *gheader) bool {
	for i := 0; i < s.len; i++ {
		if s.nodes[i] == h {
			return true
		}
	}
	return false
}

func (s *latchSet) push(h *gheader, version uint64) {
	s.nodes[s.len], s.versions[s.len] = h, version
	s.len++
}

// takeAt takes h if it is still at version; false if it changed or is held.
func (s *latchSet) takeAt(h *gheader, version uint64) bool {
	if s.holds(h) {
		return true
	}
	if !h.latch.tryLockAt(version) {
		return false
	}
	s.push(h, version)
	return true
}

// takeInternal takes internal node h at the version it is at, unless held.
func (s *latchSet) takeInternal(h *gheader) bool {
	if s.holds(h) {
		return true
	}
	version, ok := h.latch.tryReadVersion()
	return ok && s.takeAt(h, version)
}

// takeLeaf latches leaf h exclusively, unless any goroutine holds it.
func (s *latchSet) takeLeaf(h *gheader) bool {
	if s.holds(h) {
		return true
	}
	version, ok := h.latch.tryLockExclusive()
	if !ok {
		return false
	}
	s.push(h, version)
	return true
}

func (s *latchSet) unlock() {
	for i := 0; i < s.len; i++ {
		s.nodes[i].latch.unlockExclusive(s.versions[i])
	}
	s.len = 0
}

// insert inserts key and value, or, where the tree holds key, does what
// policy says; it returns the stored value and whether key was there. As
// latchedTree.insert: the key's leaf, latched exclusively, takes it if it
// has room; otherwise the leaf splits.
func (t *genericTree[K, V, O]) insert(key K, value V, policy int, merge func(K, V, V) V) (V, bool) {
	if t.owner {
		return t.ownerInsert(key, value, policy, merge)
	}
	if t.root.Load() == nil {
		t.start()
	}
	if prefixed[O, K]() {
		t.admit(key)
	}
	code := t.code(key)
	if old, found, done := t.insertLatched(key, code, value, policy, merge); done {
		return old, found
	}
	return t.insertSplitting(key, code, value, policy, merge)
}

// admit makes the shared prefix one key begins with before the key goes in:
// the first key's sets it, and a key without it shortens it to what they
// share (D50). It only shortens, so a key that had it keeps it.
func (t *genericTree[K, V, O]) admit(key K) {
	s := textView(&key)
	for {
		cur := t.shared.Load()
		if cur == nil {
			// A copy: Bytes keys are the caller's to change.
			shared := strings.Clone(s[:min(len(s), 64)])
			if t.shared.CompareAndSwap(nil, &shared) {
				return
			}
			continue
		}
		p := commonPrefix(s, *cur)
		if p >= len(*cur) {
			return
		}
		t.shrinkShared(cur, p)
	}
}

// shrinkShared shortens the shared prefix cur to p bytes, as upstream's
// latched tree does: it takes the root and every internal node on the
// tree's borders, the nodes that take the shared prefix for a fence, without
// waiting, and refits them; another goroutine holding one makes it start
// over. It returns once the prefix is no longer cur.
func (t *genericTree[K, V, O]) shrinkShared(cur *string, p int) {
	var b backoff
	for t.shared.Load() == cur {
		var held latchSet
		root := t.root.Load()
		ok := false
		if root.leaf {
			ok = held.takeLeaf(root)
		} else {
			ok = held.takeInternal(root) && takeBorder(&held, asInner[K](root), false) && takeBorder(&held, asInner[K](root), true)
		}
		if ok && t.root.Load() == root && t.shared.Load() == cur {
			shorter := (*cur)[:p]
			t.shared.Store(&shorter)
			for i := 0; i < held.len; i++ {
				if n := held.nodes[i]; !n.leaf {
					t.refitInner(asInner[K](n))
				}
			}
			// The leaves on the borders take the shared prefix for a fence.
			if root.leaf {
				t.fitLeaf(asLeaf[K, V](root), nil, nil)
			} else {
				for _, right := range [2]bool{false, true} {
					parent, edge := borderParent(asInner[K](root), right)
					low, high := leafFences(parent, edge)
					t.fitLeaf(asLeaf[K, V](parent.children[edge&(nodeSize-1)].Load()), low, high)
				}
			}
			held.unlock()
			return
		}
		held.unlock()
		b.spin()
	}
}

// takeBorder takes the nodes below n, held, along its first children, or
// its last when right: internal nodes and the leaf they end at.
func takeBorder[K any](held *latchSet, n *ginner[K], right bool) bool {
	for {
		i := 0
		if right {
			i = int(n.count.Load()) - 1
		}
		c := n.children[max(i, 0)&(nodeSize-1)].Load()
		if c == nil {
			return false
		}
		if c.leaf {
			return held.takeLeaf(c)
		}
		if !held.takeInternal(c) {
			return false
		}
		n = asInner[K](c)
	}
}

// borderParent is the internal node above the tree's first leaf, or its
// last when right, below n, and the leaf's edge in it.
func borderParent[K any](n *ginner[K], right bool) (*ginner[K], int) {
	for {
		i := 0
		if right {
			i = int(n.count.Load()) - 1
		}
		c := n.children[i&(nodeSize-1)].Load()
		if c.leaf {
			return n, i
		}
		n = asInner[K](c)
	}
}

func (t *genericTree[K, V, O]) insertLatched(key K, code uint64, value V, policy int, merge func(K, V, V) V) (old V, found, done bool) {
	n, version := t.leafLatched(key, code, seekKey, exclusive)
	defer n.latch.unlockExclusive(version)
	return t.insertInto(n, key, code, value, policy, merge)
}

// insertInto inserts into leaf n, held exclusively, if it holds key, of
// code code, or has room; done is false when the leaf is full and must split.
func (t *genericTree[K, V, O]) insertInto(n *gleaf[K, V], key K, code uint64, value V, policy int, merge func(K, V, V) V) (old V, found, done bool) {
	i, ok := t.find(n, key, code)
	if ok {
		old = n.values[i]
		switch policy {
		case insertReplace:
			n.values[i] = value
		case insertMerge:
			n.values[i] = merge(t.key(n, i), old, value)
		}
		return old, true, true
	}
	if int(n.count.Load()) == nodeSize {
		return old, false, false
	}
	t.insertEntry(n, i, key, value)
	return old, false, true
}

// insertIntoReleasing is insertInto that lets go of n if merge panics.
func (t *genericTree[K, V, O]) insertIntoReleasing(n *gleaf[K, V], version uint64, key K, code uint64, value V, policy int, merge func(K, V, V) V) (old V, found, done bool) {
	finished := false
	defer func() {
		if !finished {
			n.latch.unlockExclusive(version)
		}
	}()
	old, found, done = t.insertInto(n, key, code, value, policy, merge)
	finished = true
	return old, found, done
}

// insertSplitting is insert where the leaf was full: it searches again,
// keeping its path, and splits, until no other goroutine holds a node the
// split needs.
func (t *genericTree[K, V, O]) insertSplitting(key K, code uint64, value V, policy int, merge func(K, V, V) V) (V, bool) {
	for {
		var path gpath[K]
		n, version := t.descend(key, code, seekKey, false, exclusive, &path)
		old, found, done := t.insertIntoReleasing(n, version, key, code, value, policy, merge)
		if done {
			n.latch.unlockExclusive(version)
			return old, found
		}
		if t.split(&path, n, version, key, code, value) {
			return old, false
		}
		n.latch.unlockExclusive(version)
		// Another goroutine holds a node the split needs: wait a moment,
		// as upstream's Interleaving::wait(1).
		spinWait()
	}
}

// split splits leaf, full and held, and inserts key and value, as
// latchedTree.split: the lowest node on the path with room takes the
// separator, every node below it splits, and without one the root splits.
// It takes those path nodes at the versions the descent read; false, with
// nothing changed, if one changed or is held. Otherwise it lets go of
// everything, leaf included.
func (t *genericTree[K, V, O]) split(path *gpath[K], leaf *gleaf[K, V], leafVersion uint64, key K, code uint64, value V) bool {
	top := path.room()
	var held latchSet
	for level := top; level < path.depth; level++ {
		if !held.takeAt(&path.levels[level].n.gheader, path.levels[level].version) {
			held.unlock()
			return false
		}
	}
	index, _ := t.find(leaf, key, code)
	var fresh latchSet
	t.splitHeld(path, top, leaf, index, key, value, &fresh)
	held.unlock()
	leaf.latch.unlockExclusive(leafVersion)
	fresh.unlock()
	return true
}

// room is the lowest level of the path with room for a child, from which
// the split's separator stops rising, or 0, where the root splits too.
func (p *gpath[K]) room() int {
	for level := p.depth - 1; level >= 0; level-- {
		if p.levels[level].count < nodeSize {
			return level
		}
	}
	return 0
}

// splitHeld is split's change, made with the path from top down and leaf
// held, or in an owner's tree, where no other goroutine reads them: leaf
// splits and takes key and value at index, and the separator rises to the
// path's level top, splitting the full nodes below it, or the root. The
// nodes it makes are latched in fresh.
func (t *genericTree[K, V, O]) splitHeld(path *gpath[K], top int, leaf *gleaf[K, V], index int, key K, value V, fresh *latchSet) {
	right := newGLeaf[K, V](prefixed[O, K]())
	fresh.push(&right.gheader, 0)
	// A key past the leaf's last leaves it nearly full, as an ascending
	// run refills the new leaf.
	minimum := leafMinLen
	if index == nodeSize {
		minimum = leafMinLen / 2
	}
	cut, left := ofSplit(nodeSize, index, minimum)
	t.moveLeafEntries(right, 0, leaf, cut, nodeSize-cut)
	// The separator is the right half's first key, before the new one goes
	// in, which ofSplit leaves to the left where they would tie; the halves
	// take what the separator shares with the leaf's fences.
	separator := t.separator(right)
	t.setHead(right)
	if prefixed[O, K]() {
		var low, high *K
		if path.depth > 0 {
			level := &path.levels[path.depth-1]
			low, high = leafFences(level.n, level.edge)
		}
		t.growLeaf(leaf, t.fencePrefix(low, separator))
		t.growLeaf(right, t.fencePrefix(separator, high))
	}
	if left {
		t.insertEntry(leaf, index, key, value)
	} else {
		t.insertEntry(right, index-cut, key, value)
	}
	carried := &right.gheader
	for level := path.depth - 1; level >= top && carried != nil; level-- {
		n, edge := path.levels[level].n, path.levels[level].edge+1
		if int(n.count.Load()) < nodeSize {
			t.insertChild(n, edge, separator, carried)
			carried = nil
			break
		}
		split := newGInner[K]()
		fresh.push(&split.gheader, 0)
		high := n.high.Load()
		cut, left := ofSplit(nodeSize, edge, innerMinLen)
		t.moveChildEntries(split, 0, n, cut, nodeSize-cut)
		if left {
			t.insertChild(n, edge, separator, carried)
		} else {
			t.insertChild(split, edge-cut, separator, carried)
		}
		separator, carried = split.seps[0].Load(), &split.gheader
		// The halves' fences: the separator between them, and n's.
		n.high.Store(separator)
		split.high.Store(high)
		t.refitInner(n)
		t.refitInner(split)
	}
	if carried != nil {
		// The root split: a new root takes the old one and its new sibling.
		root := newGInner[K]()
		fresh.push(&root.gheader, 0)
		t.insertChild(root, 0, nil, t.root.Load())
		t.insertChild(root, 1, separator, carried)
		// Its fences are the tree's borders: it takes the shared prefix.
		t.refitInner(root)
		t.root.Store(&root.gheader)
	}
}

// Node changes, made with the node held exclusively. Racing readers of
// internal nodes read only words that can't tear, atomically; readers of
// leaves read them latched, or racing where a version check covers them.

// insertEntry inserts an entry at i into leaf n, which has room, a keyed
// leaf's key owned, a text leaf's copied into its buffer. The key lies
// between the leaf's fences, so begins with its prefix; an empty leaf, the
// root, takes the tree's shared prefix, its fences' (D50).
func (t *genericTree[K, V, O]) insertEntry(n *gleaf[K, V], i int, key K, value V) {
	p := 0
	count := int(n.count.Load())
	if prefixed[O, K]() {
		if count == 0 {
			n.setPrefix(textView(&key), len(t.sharedText()))
		}
		p = int(n.prefix.Load())
	}
	copy(n.codes[i+1:count+1], n.codes[i:count])
	copy(n.values[i+1:count+1], n.values[i:count])
	if prefixed[O, K]() {
		// The key's bytes go in first: making room repacks the refs, which
		// must be the leaf's count, unshifted.
		s, c := textView(&key), n.text()
		ref := t.placeKey(n, s)
		copy(c.slots[i+1:count+1], c.slots[i:count])
		n.codes[i] = stringCode(s, p)
		c.slots[i] = textSlot{stringCode(s, p+8), ref}
	} else {
		keys := n.keyed()
		copy(keys[i+1:count+1], keys[i:count])
		keys[i], n.codes[i] = ownKey[O](key), windowKey[O](key, p)
	}
	n.values[i] = value
	n.count.Store(int32(count + 1))
}

// reserveKeys makes room in text leaf n's buffer for extra more bytes.
func (t *genericTree[K, V, O]) reserveKeys(n *gleaf[K, V], extra int) {
	if c := n.text(); extra > int(c.size-c.used) {
		t.growKeys(n, extra)
	}
}

// growKeys makes room in text leaf n's buffer for extra more bytes, with
// as much room again: a reusable buffer that then fits them packs its live
// keys in place; otherwise n takes a new buffer, of its live keys in slot
// order, and the old keeps its bytes for the keys handed out over them.
func (t *genericTree[K, V, O]) growKeys(n *gleaf[K, V], extra int) {
	c := n.text()
	count, live := int(n.count.Load()), extra
	for i := 0; i < count; i++ {
		live += int(uint32(c.slots[i].ref))
	}
	c.hole = 0
	if 2*live <= int(c.size) && t.reusable(n) {
		t.packKeys(n)
		return
	}
	size := max(2*live, 64)
	if uint64(size) > math.MaxUint32 {
		panic("gobbtree: a leaf's keys passed 4 GiB")
	}
	buf, used := keyBuffer(size), 0
	for i := 0; i < count; i++ {
		s := keyAt(c.buf, c.slots[i].ref)
		copy(buf[used:], s)
		c.slots[i].ref = keyRef(used, len(s))
		used += len(s)
	}
	c.buf, c.used, c.size = unsafe.Pointer(unsafe.SliceData(buf)), uint32(used), uint32(cap(buf))
	// A new generation where keys of the last were handed out.
	if t.handed.Load() >= t.epoch {
		t.epoch++
	}
	c.epoch = t.epoch
}

// packKeys moves text leaf n's live keys to the front of its buffer, in
// the order they lie there, so that each moves down, over bytes no key
// reads.
func (t *genericTree[K, V, O]) packKeys(n *gleaf[K, V]) {
	c := n.text()
	count := int(n.count.Load())
	var order [nodeSize]uint8
	for i := 0; i < count; i++ {
		j := i
		for ; j > 0 && c.slots[order[j-1]].ref>>32 > c.slots[i].ref>>32; j-- {
			order[j] = order[j-1]
		}
		order[j] = uint8(i)
	}
	used := 0
	for _, i := range order[:count] {
		ref := c.slots[i].ref
		length := int(uint32(ref))
		if length == 0 {
			continue
		}
		if from := int(ref >> 32); from != used {
			copy(unsafe.Slice((*byte)(unsafe.Add(c.buf, used)), length), unsafe.Slice((*byte)(unsafe.Add(c.buf, from)), length))
		}
		c.slots[i].ref = keyRef(used, length)
		used += length
	}
	c.used = uint32(used)
}

// keyBuffer is a buffer of at least size bytes for a leaf's keys, its
// whole capacity, which holds no pointers.
func keyBuffer(size int) []byte {
	b := make([]byte, 0, size)
	return b[:cap(b)]
}

// placeKey writes text s into text leaf n's buffer and returns its ref: in
// the hole a removal left, where the buffer may be reused and s fits it,
// otherwise past the bytes written, room made for it.
func (t *genericTree[K, V, O]) placeKey(n *gleaf[K, V], s string) uint64 {
	c := n.text()
	if hole := c.hole; hole != 0 && len(s) <= int(uint32(hole)) && t.reusable(n) {
		c.hole = 0
		if len(s) == 0 {
			return keyRef(0, 0)
		}
		copy(unsafe.Slice((*byte)(unsafe.Add(c.buf, hole>>32)), len(s)), s)
		return keyRef(int(hole>>32), len(s))
	}
	t.reserveKeys(n, len(s))
	return t.appendKey(n, s)
}

// appendKey writes text s past the bytes text leaf n's buffer holds, which
// has room for it, and returns its ref.
func (t *genericTree[K, V, O]) appendKey(n *gleaf[K, V], s string) uint64 {
	if len(s) == 0 {
		return keyRef(0, 0)
	}
	c := n.text()
	copy(unsafe.Slice((*byte)(unsafe.Add(c.buf, c.used)), len(s)), s)
	ref := keyRef(int(c.used), len(s))
	c.used += uint32(len(s))
	return ref
}

// removeEntry removes leaf n's entry i, clearing the slot it frees so that
// the collector can take what it held. An emptied leaf has no prefix: no key
// is left to share one; a text leaf lets go of its buffer, which the key
// removed may still read. handOut is false where the caller drops the key
// returned: an owner's reusable buffer then keeps its bytes for the next.
func (t *genericTree[K, V, O]) removeEntry(n *gleaf[K, V], i int, handOut bool) (K, V) {
	count := int(n.count.Load())
	var key K
	if handOut || !prefixed[O, K]() {
		key = t.key(n, i)
	} else if t.reusable(n) {
		n.text().hole = n.text().slots[i&(nodeSize-1)].ref
	}
	value := n.values[i]
	copy(n.codes[i:count-1], n.codes[i+1:count])
	copy(n.values[i:count-1], n.values[i+1:count])
	var zv V
	n.codes[count-1], n.values[count-1] = codePad, zv
	if prefixed[O, K]() {
		c := n.text()
		copy(c.slots[i:count-1], c.slots[i+1:count])
		c.slots[count-1] = textSlot{}
		if count == 1 {
			c.buf, c.used, c.size, c.hole = nil, 0, 0, 0
		}
	} else {
		keys := n.keyed()
		copy(keys[i:count-1], keys[i+1:count])
		var zk K
		keys[count-1] = zk
	}
	n.count.Store(int32(count - 1))
	if count == 1 {
		n.setPrefix("", 0)
	}
	return key, value
}

// moveLeafEntries moves count entries of src from from to dst at at,
// shifting dst's entries up and src's down; a text leaf's keys are copied
// into dst's buffer, room made for them first. Text leaves' windows are
// left to their refits, at the fences the move gives them.
func (t *genericTree[K, V, O]) moveLeafEntries(dst *gleaf[K, V], at int, src *gleaf[K, V], from, count int) {
	srcCount, dstCount := int(src.count.Load()), int(dst.count.Load())
	if prefixed[O, K]() && dstCount == 0 {
		// A split's new half: its keys keep src's prefix and windows.
		t.splitLeafEntries(dst, src, from, count)
		return
	}
	if prefixed[O, K]() {
		sc, dc := src.text(), dst.text()
		moved := 0
		for j := from; j < from+count; j++ {
			moved += int(uint32(sc.slots[j].ref))
		}
		t.reserveKeys(dst, moved)
		copy(dc.slots[at+count:dstCount+count], dc.slots[at:dstCount])
		for j := 0; j < count; j++ {
			dc.slots[at+j].ref = t.appendKey(dst, keyAt(sc.buf, sc.slots[from+j].ref))
		}
		copy(sc.slots[from:srcCount-count], sc.slots[from+count:srcCount])
		clear(sc.slots[srcCount-count : srcCount])
	} else {
		dk, sk := dst.keyed(), src.keyed()
		copy(dk[at+count:dstCount+count], dk[at:dstCount])
		copy(dk[at:at+count], sk[from:from+count])
		copy(sk[from:srcCount-count], sk[from+count:srcCount])
		clear(sk[srcCount-count : srcCount])
	}
	copy(dst.codes[at+count:dstCount+count], dst.codes[at:dstCount])
	copy(dst.values[at+count:dstCount+count], dst.values[at:dstCount])
	copy(dst.codes[at:at+count], src.codes[from:from+count])
	copy(dst.values[at:at+count], src.values[from:from+count])
	copy(src.codes[from:srcCount-count], src.codes[from+count:srcCount])
	copy(src.values[from:srcCount-count], src.values[from+count:srcCount])
	for j := srcCount - count; j < srcCount; j++ {
		src.codes[j] = codePad
	}
	clear(src.values[srcCount-count : srcCount])
	dst.count.Store(int32(dstCount + count))
	src.count.Store(int32(srcCount - count))
}

// splitLeafEntries is moveLeafEntries into dst, empty, for text leaves: the
// entries move with their windows, their keys into a buffer of dst's own,
// and dst takes src's prefix, which both grow to their fences' after.
func (t *genericTree[K, V, O]) splitLeafEntries(dst, src *gleaf[K, V], from, count int) {
	srcCount := int(src.count.Load())
	sc, dc := src.text(), dst.text()
	moved := 0
	for j := from; j < from+count; j++ {
		moved += int(uint32(sc.slots[j].ref))
	}
	t.reserveKeys(dst, moved)
	for j := 0; j < count; j++ {
		dc.slots[j] = textSlot{sc.slots[from+j].tail, t.appendKey(dst, keyAt(sc.buf, sc.slots[from+j].ref))}
	}
	copy(dst.codes[:count], src.codes[from:from+count])
	copy(dst.values[:count], src.values[from:from+count])
	copy(src.codes[from:srcCount-count], src.codes[from+count:srcCount])
	copy(src.values[from:srcCount-count], src.values[from+count:srcCount])
	copy(sc.slots[from:srcCount-count], sc.slots[from+count:srcCount])
	for j := srcCount - count; j < srcCount; j++ {
		src.codes[j] = codePad
	}
	clear(sc.slots[srcCount-count : srcCount])
	clear(src.values[srcCount-count : srcCount])
	dst.prefix.Store(src.prefix.Load())
	dst.head[0].Store(src.head[0].Load())
	dst.head[1].Store(src.head[1].Load())
	dst.count.Store(int32(count))
	src.count.Store(int32(srcCount - count))
}

// insertChild inserts child, whose keys are at least separator, at i in
// internal node n, which has room.
func (t *genericTree[K, V, O]) insertChild(n *ginner[K], i int, separator *K, child *gheader) {
	count := int(n.count.Load())
	for j := count; j > i; j-- {
		n.seps[j].Store(n.seps[j-1].Load())
		n.children[j].Store(n.children[j-1].Load())
	}
	n.seps[i].Store(separator)
	n.children[i].Store(child)
	n.count.Store(int32(count + 1))
	// A separator between n's fences moves neither (D26): the codes or
	// summaries shift, and only the new one is read from its key.
	if prefixed[O, K]() {
		sums := n.sums()
		for j := count; j > i; j-- {
			storeSum(sums, j, loadSum(sums, j-1))
		}
		sum := uint32(0)
		if i > 0 {
			sum = summaryOf(textView(separator), int(n.prefix.Load()))
		}
		storeSum(sums, i, sum)
		return
	}
	for j := count; j > i; j-- {
		n.codes[j].Store(n.codes[j-1].Load())
	}
	code := uint64(0)
	if i > 0 {
		code = windowKey[O](*separator, 0)
	}
	n.codes[i].Store(code)
}

// removeChild removes internal node n's child i.
func (t *genericTree[K, V, O]) removeChild(n *ginner[K], i int) {
	count := int(n.count.Load())
	for j := i; j < count-1; j++ {
		n.seps[j].Store(n.seps[j+1].Load())
		n.children[j].Store(n.children[j+1].Load())
	}
	n.seps[count-1].Store(nil)
	n.children[count-1].Store(nil)
	n.count.Store(int32(count - 1))
	t.refitInner(n)
}

// moveChildEntries moves count children of src from from, with their
// separators, to dst at at, as moveLeafEntries moves entries.
func (t *genericTree[K, V, O]) moveChildEntries(dst *ginner[K], at int, src *ginner[K], from, count int) {
	srcCount, dstCount := int(src.count.Load()), int(dst.count.Load())
	for j := dstCount - 1; j >= at; j-- {
		dst.seps[j+count].Store(dst.seps[j].Load())
		dst.children[j+count].Store(dst.children[j].Load())
	}
	for j := 0; j < count; j++ {
		dst.seps[at+j].Store(src.seps[from+j].Load())
		dst.children[at+j].Store(src.children[from+j].Load())
	}
	for j := from; j+count < srcCount; j++ {
		src.seps[j].Store(src.seps[j+count].Load())
		src.children[j].Store(src.children[j+count].Load())
	}
	for j := srcCount - count; j < srcCount; j++ {
		src.seps[j].Store(nil)
		src.children[j].Store(nil)
	}
	dst.count.Store(int32(dstCount + count))
	src.count.Store(int32(srcCount - count))
	t.refitInner(dst)
	t.refitInner(src)
}
