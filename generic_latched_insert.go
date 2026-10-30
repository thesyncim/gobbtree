package gobbtree

import (
	"math"
	"strings"
	"unsafe"
)

// What an insert does where the tree holds the key already, upstream's
// InsertPolicy: replace the value (insert), store merge's result
// (update_or_insert), or keep it (get_or_insert).
const (
	insertReplace = iota
	insertMerge
	insertKeep
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
// upstream's insert: the key's leaf, latched exclusively, takes it if it
// has room; otherwise the leaf splits.
func (t *genericTree[K, V, O]) insert(key K, value V, policy int, merge func(K, V, V) V) (V, bool) {
	if t.owner {
		return t.ownerInsert(key, value, policy, merge)
	}
	if t.root.Load() == nil {
		t.start()
	}
	// A leaf the fast paths find full splits at once: they have searched
	// it, and the latched path would search it again to find it full.
	if prefixed[O, K]() {
		t.admit(key)
		if seqlockPlainStores && policy != insertMerge {
			old, found, done, full := t.insertText(key, value, policy)
			if done {
				return old, found
			}
			if full {
				return t.insertSplitting(key, t.code(key), value, policy, merge)
			}
		}
	}
	if seqlockPlainStores && t.wordCodes() && policy != insertMerge {
		if done, full := t.appendTail(key, numericKey[O](key), value); done || full != nil {
			if full != nil {
				return t.insertSplitting(key, t.code(key), value, policy, merge)
			}
			var zero V
			return zero, false
		}
		old, found, done, full := t.insertHinted(key, numericKey[O](key), value, policy)
		if done {
			return old, found
		}
		if full {
			return t.insertSplitting(key, t.code(key), value, policy, merge)
		}
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
			// The borders take the shared prefix for a fence: the root, the
			// internal nodes and the leaves along them.
			if root.leaf {
				t.fitLeaf(asLeaf[K, V](root), nil, nil)
			} else {
				t.refitInner(asInner[K](root), nil)
				for _, right := range [2]bool{false, true} {
					t.refitBorder(asInner[K](root), right)
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

// refitBorder refits the nodes below root, held, along its first children,
// or its last when right, leaf included, to their fences: on the left
// border each one's high is the separator right of it, or its parent's.
func (t *genericTree[K, V, O]) refitBorder(root *ginner[K], right bool) {
	n, high := root, (*K)(nil)
	for {
		i := 0
		if right {
			i = int(n.count.Load()) - 1
		}
		if !right && i+1 < int(n.count.Load()) {
			if k, ok := t.separatorKey(n, i+1); ok {
				high = &k
			}
		}
		c := n.children[i&(nodeSize-1)].Load()
		if c.leaf {
			leaf := asLeaf[K, V](c)
			t.fitLeaf(leaf, t.leafLow(leaf), high)
			return
		}
		n = asInner[K](c)
		t.refitInner(n, high)
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
// upstream's split: the lowest node on the path with room takes the
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
	// The halves refit to their fences, the highest held at or above top.
	if !path.takeHighHolder(top, &held) {
		held.unlock()
		return false
	}
	index, _ := t.find(leaf, key, code)
	if !t.redistribute(path, leaf, index, key, value, &held) {
		var fresh latchSet
		t.splitHeld(path, top, leaf, index, key, value, &fresh)
		fresh.unlock()
	}
	held.unlock()
	leaf.latch.unlockExclusive(leafVersion)
	return true
}

// redistributionSlack is the room a full leaf's sibling must have to take
// some of its entries instead of the leaf splitting.
const redistributionSlack = 8

// redistribute makes room in leaf, full and held, for key and value at
// index, B*-tree fashion: half the room of a sibling under the same parent
// with redistributionSlack or more takes as many of the leaf's entries,
// and the separator between them moves with them; key goes into whichever
// leaf now holds its place. Leaves stay fuller than splits leave them,
// about a sixth, which a tree's memory and lookups both feel. An insert at
// either end splits as it did: ordered inserts fill leaves densely. path's
// nodes from the parent down are held, and the fence holder above them; the
// sibling is taken without waiting, added to held. false, with nothing
// changed, where no sibling has the room or can be taken.
func (t *genericTree[K, V, O]) redistribute(path *gpath[K], leaf *gleaf[K, V], index int, key K, value V, held *latchSet) bool {
	// Text leaves split: moving their keys copies the keys' bytes and
	// refits both leaves' windows, which costs a build more than the fill
	// saves.
	if path.depth == 0 || index == 0 || index == nodeSize || prefixed[O, K]() {
		return false
	}
	level := path.depth - 1
	parent, edge := path.levels[level].n, path.levels[level].edge
	count := int(parent.count.Load())
	best, bestCount := -1, nodeSize-redistributionSlack+1
	for _, sib := range [2]int{edge - 1, edge + 1} {
		if sib < 0 || sib >= count {
			continue
		}
		if c := int(parent.children[sib].Load().count.Load()); c < bestCount {
			best, bestCount = sib, c
		}
	}
	if best < 0 || !held.takeLeaf(parent.children[best].Load()) {
		return false
	}
	sib := asLeaf[K, V](parent.children[best].Load())
	moved := (nodeSize - int(sib.count.Load())) / 2
	if best < edge {
		// The leaf's first entries go to the end of its left sibling; the
		// leaf's new first key is their separator.
		t.moveLeafEntries(sib, int(sib.count.Load()), leaf, 0, moved)
		separator := t.firstKey(leaf)
		t.setHead(leaf)
		t.replaceSeparator(parent, edge, separator)
		if t.wordCodes() {
			setHigh(&sib.gheader, t.code(separator))
		}
		t.fitLeaf(sib, t.leafLow(sib), &separator)
		t.fitLeaf(leaf, &separator, t.childHigh(path, level, edge))
		// A key below the leaf's new first, the separator, is the
		// sibling's.
		if index <= moved {
			i, _ := t.find(sib, key, t.code(key))
			t.insertEntry(sib, i, key, value)
		} else {
			t.insertEntry(leaf, index-moved, key, value)
		}
		return true
	}
	// The leaf's last entries go to the front of its right sibling, whose
	// new first key is their separator.
	t.moveLeafEntries(sib, 0, leaf, nodeSize-moved, moved)
	separator := t.firstKey(sib)
	t.setHead(sib)
	t.replaceSeparator(parent, best, separator)
	if t.wordCodes() {
		setHigh(&leaf.gheader, t.code(separator))
	}
	t.fitLeaf(leaf, t.leafLow(leaf), &separator)
	t.fitLeaf(sib, &separator, t.childHigh(path, level, best))
	if index <= nodeSize-moved {
		t.insertEntry(leaf, index, key, value)
	} else {
		i, _ := t.find(sib, key, t.code(key))
		t.insertEntry(sib, i, key, value)
	}
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
	right, version := t.newLeaf()
	fresh.push(&right.gheader, version)
	// A key past the leaf's last leaves it nearly full, as an ascending
	// run refills the new leaf.
	minimum := leafMinLen
	if index == nodeSize {
		minimum = leafMinLen / 2
	}
	cut, left := ofSplit(nodeSize, index, minimum)
	t.moveLeafEntries(right, 0, leaf, cut, nodeSize-cut)
	// The separator is the right half's first key, before the new one goes
	// in, which ofSplit leaves to the left where they would tie, and the
	// right half's head; the halves take what it shares with the leaf's
	// fences.
	separator := t.firstKey(right)
	t.setHead(right)
	if t.wordCodes() {
		// The halves' fences: the separator between them, and the leaf's.
		_, high := bounds(&leaf.gheader)
		setHigh(&right.gheader, high)
		setHigh(&leaf.gheader, right.codes[0])
		if high == 0 {
			t.tail.Store(&right.gheader)
		}
	}
	if prefixed[O, K]() {
		var high *K
		if path.depth > 0 {
			level := &path.levels[path.depth-1]
			high = t.childHigh(path, path.depth-1, level.edge)
		}
		t.growLeaf(leaf, t.fencePrefix(t.leafLow(leaf), &separator))
		t.growLeaf(right, t.fencePrefix(&separator, high))
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
		high := t.nodeHigh(path, level)
		cut, left := ofSplit(nodeSize, edge, innerMinLen)
		t.moveChildEntries(split, 0, n, cut, nodeSize-cut)
		if left {
			t.insertChild(n, edge, separator, carried)
		} else {
			t.insertChild(split, edge-cut, separator, carried)
		}
		// The halves' fences: the separator between them, the first key
		// below split, and n's.
		separator, _ = t.separatorKey(split, 0)
		carried = &split.gheader
		t.refitInner(n, &separator)
		t.refitInner(split, high)
	}
	if carried != nil {
		// The root split: a new root takes the old one and its new sibling.
		root := newGInner[K]()
		fresh.push(&root.gheader, 0)
		var none K
		t.insertChild(root, 0, none, t.root.Load())
		t.insertChild(root, 1, separator, carried)
		// Its fences are the tree's borders: it takes the shared prefix.
		t.refitInner(root, nil)
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
	if i < count {
		copy(n.codes[i+1:count+1], n.codes[i:count])
		copy(n.values[i+1:count+1], n.values[i:count])
	}
	if prefixed[O, K]() {
		// The key's bytes go in first: making room repacks the refs, which
		// must be the leaf's count, unshifted.
		s, c := textView(&key), n.text()
		ref := t.placeKey(n, s)
		copy(c.slots[i+1:count+1], c.slots[i:count])
		n.codes[i] = stringCode(s, p)
		c.slots[i] = textSlot{stringCode(s, p+8), ref}
	} else if t.wholeCodes() {
		n.codes[i] = numericKey[O](key)
		if i < count {
			copy(n.tags[i+1:count+1], n.tags[i:count])
		}
		n.tags[i] = uint8(n.codes[i])
	} else {
		keys := n.keyed()
		copy(keys[i+1:count+1], keys[i:count])
		keys[i] = ownKey[O](key)
		n.codes[i] = windowKey[O](key, p)
		if exactCodes[O, K]() {
			copy(n.tags[i+1:count+1], n.tags[i:count])
			n.tags[i] = uint8(n.codes[i])
		}
	}
	n.values[i] = value
	if t.wordCodes() && i == 0 {
		t.setFirst(n, count+1)
	}
	n.count.Store(int32(count + 1))
}

// reserveKeys makes room in text leaf n's buffer for extra more bytes, of
// keys more keys.
func (t *genericTree[K, V, O]) reserveKeys(n *gleaf[K, V], extra, keys int) {
	if c := n.text(); extra > int(c.size-c.used) {
		t.growKeys(n, extra, keys)
	}
}

// growKeys makes room in text leaf n's buffer for extra more bytes, with
// as much room again: a reusable buffer that then fits them packs its live
// keys in place; otherwise n takes a new buffer, of its live keys in slot
// order, and the old keeps its bytes for the keys handed out over them.
func (t *genericTree[K, V, O]) growKeys(n *gleaf[K, V], extra, keys int) {
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
	size := bufferFor(live, count+keys)
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

// bufferFor is the size of a buffer for keys of live bytes, count of
// them, in a leaf that may fill: room for a full leaf of keys their length
// on average, an eighth more, so that a leaf filling takes one buffer, not
// one a doubling, each copying every key.
func bufferFor(live, count int) int {
	return max(live*nodeSize/max(count, 1)*9/8, live+live/8, 64)
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
	t.reserveKeys(n, len(s), 1)
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
	if t.wordCodes() {
		copy(n.tags[i:count-1], n.tags[i+1:count])
		n.tags[count-1] = 0
		if i == 0 {
			t.setFirst(n, count-1)
		}
	}
	if prefixed[O, K]() {
		c := n.text()
		copy(c.slots[i:count-1], c.slots[i+1:count])
		c.slots[count-1] = textSlot{}
		if count == 1 {
			c.buf, c.used, c.size, c.hole = nil, 0, 0, 0
		}
	} else if !t.wholeCodes() {
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
		t.reserveKeys(dst, moved, count)
		copy(dc.slots[at+count:dstCount+count], dc.slots[at:dstCount])
		for j := 0; j < count; j++ {
			dc.slots[at+j].ref = t.appendKey(dst, keyAt(sc.buf, sc.slots[from+j].ref))
		}
		copy(sc.slots[from:srcCount-count], sc.slots[from+count:srcCount])
		clear(sc.slots[srcCount-count : srcCount])
	} else if !t.wholeCodes() {
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
	if t.wordCodes() {
		retag(dst, dstCount+count)
		retag(src, srcCount-count)
		t.setFirst(dst, dstCount+count)
		t.setFirst(src, srcCount-count)
	}
	dst.count.Store(int32(dstCount + count))
	src.count.Store(int32(srcCount - count))
}

// retag sets leaf n's tags from its first count codes, the rest to 0: a
// key may have the padding's code.
func retag[K any, V any](n *gleaf[K, V], count int) {
	for j := range n.tags {
		n.tags[j] = 0
		if j < count {
			n.tags[j] = uint8(n.codes[j])
		}
	}
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
	t.reserveKeys(dst, moved, count)
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
func (t *genericTree[K, V, O]) insertChild(n *ginner[K], i int, separator K, child *gheader) {
	count := int(n.count.Load())
	for j := count; j > i; j-- {
		n.children[j].Store(n.children[j-1].Load())
	}
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
			sum = summaryOf(textView(&separator), int(n.prefix.Load()))
		}
		storeSum(sums, i, sum)
		return
	}
	for j := count; j > i; j-- {
		n.codes[j].Store(n.codes[j-1].Load())
	}
	code := uint64(0)
	if i > 0 {
		code = windowKey[O](separator, 0)
	}
	n.codes[i].Store(code)
}

// removeChild removes internal node n's child i.
func (t *genericTree[K, V, O]) removeChild(n *ginner[K], i int) {
	count := int(n.count.Load())
	for j := i; j < count-1; j++ {
		n.children[j].Store(n.children[j+1].Load())
	}
	n.children[count-1].Store(nil)
	n.count.Store(int32(count - 1))
	// n's fences stay: the codes or summaries past i shift down.
	if prefixed[O, K]() {
		sums := n.sums()
		for j := i; j < count-1; j++ {
			storeSum(sums, j, loadSum(sums, j+1))
		}
		storeSum(sums, count-1, ^uint32(0))
		return
	}
	for j := i; j < count-1; j++ {
		n.codes[j].Store(n.codes[j+1].Load())
	}
	n.codes[count-1].Store(codePad)
}

// moveChildEntries moves count children of src from from to dst at at, as
// moveLeafEntries moves entries, leaving both nodes' codes to their refits.
func (t *genericTree[K, V, O]) moveChildEntries(dst *ginner[K], at int, src *ginner[K], from, count int) {
	srcCount, dstCount := int(src.count.Load()), int(dst.count.Load())
	for j := dstCount - 1; j >= at; j-- {
		dst.children[j+count].Store(dst.children[j].Load())
	}
	for j := 0; j < count; j++ {
		dst.children[at+j].Store(src.children[from+j].Load())
	}
	for j := from; j+count < srcCount; j++ {
		src.children[j].Store(src.children[j+count].Load())
	}
	for j := srcCount - count; j < srcCount; j++ {
		src.children[j].Store(nil)
	}
	dst.count.Store(int32(dstCount + count))
	src.count.Store(int32(srcCount - count))
}

// ofSplit is upstream's InsertionSide::of_split (tree/node/mod.rs): where a
// full node of capacity entries splits for an insertion at index. An append
// at either end leaves the arriving side twice the minimum, so ordered
// inserts fill nodes densely; an interior insertion divides the free space
// evenly. The left half keeps entries [0, cut), the right takes the rest,
// and the new entry goes left at index when index <= cut, else right at
// index - cut.
func ofSplit(capacity, index, minimum int) (cut int, left bool) {
	arriving := min(2*minimum, capacity/2)
	switch {
	case index == 0:
		cut = arriving - 1
	case index == capacity:
		cut = capacity - arriving
	default:
		cut = capacity / 2
	}
	return cut, index <= cut
}
