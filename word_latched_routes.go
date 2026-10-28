package gobbtree

import "math/bits"

// Route hints let a descent skip the levels above a leaf: a table of buckets
// of the key space, each naming the leaf whose fences hold the whole bucket,
// or failing that the leaf's parent. A hint is never trusted.
// The descent reads the named node's version, checks that the node holds
// children and that its fences still hold the key, and goes on by lock
// coupling from there, so a stale hint costs only a descent from the root.
//
// A node's fences are the lower bound of every key under it (an internal
// node's first separator) and a high fence, the lower bound of the next
// node's keys, 0 on the tree's right border. Separators move whole between nodes
// but never change value (upstream's "fence values move but never change"),
// so a node's fences change only when it splits, merges or trades a child
// with a sibling, while it is held. Its version vouches for them as for
// everything else in it; a freed node holds no children.
//
// Lookups fill the hints as they pass leaf parents; only plain-store builds
// keep them, as only their readers route by windows.
type latchedRoutes struct {
	root      uint32
	rootCount int
	base      uint64
	shift     uint8
	ids       []uint32
}

// A node's fences: low, the lower bound of its keys, in the prefix word
// (an internal node's first separator, which its windows share), and high.
func lowFence(n *wordNode) uint64  { return *n.prefixWord() }
func highFence(n *wordNode) uint64 { return n.high }

func setHighFence(n *wordNode, high uint64) {
	if seqlockPlainStores {
		n.high = high
	}
}

// setLeafFences sets a leaf's fences; an internal node's low fence is its
// first separator, which refitWindows keeps.
func setLeafFences(n *wordNode, low, high uint64) {
	if seqlockPlainStores {
		*n.prefixWord() = low
		n.high = high
	}
}

// holds reports that fences low and high hold key.
func holds(low, high, key uint64) bool { return low <= key && (high == 0 || key < high) }

// bucket is key's hint slot, or nil outside the table's domain.
func (r *latchedRoutes) bucket(key uint64) *uint32 {
	if index := (key - r.base) >> r.shift; index < uint64(len(r.ids)) {
		return &r.ids[index]
	}
	return nil
}

// jump starts a descent at the leaf parent a hint names: its version and the
// words that prove it holds key, or false if it doesn't, is held, or freed.
func jump(dir []*wordNode, id uint32, key uint64) (n *wordNode, version, acc uint64, ok bool) {
	n = nodeIn(dir, id)
	if n == nil {
		return nil, 0, 0, false
	}
	version, ok = n.tryReadVersion()
	if !ok {
		return nil, 0, 0, false
	}
	count := uint64(n.n)
	low, high := lowFence(n), highFence(n)
	return n, version, count ^ low ^ high, count != 0 && holds(low, high, key)
}

// fillLeaf names leaf id in key's bucket, and the buckets beside it, while
// its fences hold them whole; false if they don't hold key's.
func (r *latchedRoutes) fillLeaf(id uint32, key, low, high uint64) bool {
	index := (key - r.base) >> r.shift
	if index >= uint64(len(r.ids)) || !r.within(index, low, high) {
		return false
	}
	r.ids[index] = id
	for i, n := index, 0; i > 0 && n < 4 && r.within(i-1, low, high); i, n = i-1, n+1 {
		r.ids[i-1] = id
	}
	for i, n := index+1, 0; i < uint64(len(r.ids)) && n < 4 && r.within(i, low, high); i, n = i+1, n+1 {
		r.ids[i] = id
	}
	return true
}

// within reports that fences low and high hold bucket i whole.
func (r *latchedRoutes) within(i, low, high uint64) bool {
	start := r.base + i<<r.shift
	end := start + (uint64(1)<<r.shift - 1)
	return start >= low && end >= start && (high == 0 || end < high)
}

// fill names node id, a leaf parent with fences low and high, in key's
// bucket if they hold it whole.
func (r *latchedRoutes) fill(id uint32, key, low, high uint64) {
	index := (key - r.base) >> r.shift
	if index >= uint64(len(r.ids)) || !r.within(index, low, high) {
		return
	}
	r.ids[index] = id
}

// refreshRoutes makes a hint table for the current root when there is none,
// or the root changed or doubled its children since: about four buckets
// for every leaf, from the root's separators. The tree
// needs internal nodes above its leaf parents for hints to skip any.
func (t *latchedTree) refreshRoutes(dir []*wordNode) *latchedRoutes {
	rootID := t.root.Load()
	root := nodeIn(dir, rootID)
	if root == nil || rootID&wordLeafID != 0 {
		return nil
	}
	version, ok := root.tryReadVersion()
	if !ok {
		return nil
	}
	count, level := int(root.n), int(root.level)
	if count < 2 || count > nodeSize {
		return nil
	}
	lo, hi := root.keys[1], root.keys[count-1]
	loadFence()
	if !root.isUnchanged(version) || t.root.Load() != rootID {
		return nil
	}
	if current := t.routes.Load(); current != nil && current.root == rootID && current.rootCount*2 > count {
		return current
	}
	if level < 2 {
		// No level to skip: an empty table, kept until the root changes.
		r := &latchedRoutes{root: rootID, rootCount: count}
		t.routes.Store(r)
		return r
	}
	parents := count << (4 * (level - 2))
	buckets := min(max(bits.Len(uint(parents*320-1)), 8), 22)
	width := bits.Len64(lo ^ hi)
	base := lo &^ (uint64(1)<<width - 1)
	if width == 64 {
		base = 0
	}
	shift := max(width-buckets, 0)
	r := &latchedRoutes{root: rootID, rootCount: count, base: base, shift: uint8(shift), ids: make([]uint32, 1<<min(buckets, width))}
	t.routes.Store(r)
	return r
}
