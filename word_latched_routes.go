package gobbtree

import (
	"math/bits"
	"slices"
)

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
	hashed    bool
	bits      uint8
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

// bucket is key's hint slot, or nil outside the table's domain. A table
// over a domain no wider than itself indexes regions directly; a sparser
// one hashes them.
func (r *latchedRoutes) bucket(key uint64) *uint32 {
	region := (key - r.base) >> r.shift
	if r.hashed {
		return &r.ids[region*0x9e3779b97f4a7c15>>(64-r.bits)]
	}
	if region < uint64(len(r.ids)) {
		return &r.ids[region]
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

// fillLeaf names leaf id in key's bucket, and in a direct table the
// buckets beside it, while its fences hold them whole; false if they don't
// hold key's.
func (r *latchedRoutes) fillLeaf(id uint32, key, low, high uint64) bool {
	slot := r.bucket(key)
	region := (key - r.base) >> r.shift
	if slot == nil || !r.within(region, low, high) {
		return false
	}
	set(slot, id)
	if r.hashed {
		return true
	}
	for i, n := region, 0; i > 0 && n < 4 && r.within(i-1, low, high); i, n = i-1, n+1 {
		set(&r.ids[i-1], id)
	}
	for i, n := region+1, 0; i < uint64(len(r.ids)) && n < 4 && r.within(i, low, high); i, n = i+1, n+1 {
		set(&r.ids[i], id)
	}
	return true
}

// within reports that fences low and high hold region i whole.
func (r *latchedRoutes) within(i, low, high uint64) bool {
	start := r.base + i<<r.shift
	end := start + (uint64(1)<<r.shift - 1)
	return start >= low && end >= start && (high == 0 || end < high)
}

// fill names node id, a leaf parent with fences low and high, in key's
// bucket if they hold its region whole.
func (r *latchedRoutes) fill(id uint32, key, low, high uint64) {
	if slot := r.bucket(key); slot != nil && r.within((key-r.base)>>r.shift, low, high) {
		set(slot, id)
	}
}

// set names id in a slot, writing only a change: every core reads the
// table, so a store of what is there already costs each of them its line.
func set(slot *uint32, id uint32) {
	if *slot != id {
		*slot = id
	}
}

// refreshRoutes makes a hint table for the current root when there is none,
// or the root changed or doubled its children since. Its regions are about
// a 32nd of a leaf's span, the median gap between separators of a few
// leaf parents, so most fit in one leaf whatever the keys' distribution;
// about 32 regions a leaf fit in the table. A domain, the root's
// separators' span, that holds no more regions than that is indexed
// directly; a sparse one, such as clustered keys', is hashed. The tree needs
// internal nodes above its leaf parents for hints to skip any.
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
	span := sampleLeafSpan(dir, root, count)
	if span == 0 {
		return nil
	}
	parents := count << (4 * (level - 2))
	tableBits := min(max(bits.Len(uint(parents*20*32-1)), 8), 22)
	shift := max(bits.Len64(span)-5, 0)
	width := bits.Len64(lo ^ hi)
	r := &latchedRoutes{root: rootID, rootCount: count, shift: uint8(shift)}
	if width-shift <= tableBits {
		// Few enough regions to index directly.
		r.base = lo &^ (uint64(1)<<width - 1)
		r.ids = make([]uint32, 1<<max(width-shift, 0))
	} else {
		// Keys fill a hashed table's regions, so it takes fewer, wider
		// ones, about six a leaf, in 32 slots a leaf, to collide less.
		r.shift = uint8(max(bits.Len64(span)-3, 0))
		r.hashed, r.bits = true, uint8(tableBits)
		r.ids = make([]uint32, 1<<r.bits)
	}
	t.routes.Store(r)
	return r
}

// sampleLeafSpan estimates a leaf's span: the median gap between
// neighboring separators of leaf parents under a few of the root's
// children. Its reads race writers; a heuristic needs no check.
func sampleLeafSpan(dir []*wordNode, root *wordNode, count int) uint64 {
	var gaps []uint64
	for j := 0; j < count && len(gaps) < 256; j += max(count/8, 1) {
		n := nodeIn(dir, root.inner().children[j&(nodeSize-1)])
		for depth := 0; n != nil && !n.leaf && depth < maxDepth; depth++ {
			child := n.inner().children[0]
			if child&wordLeafID != 0 {
				for i := 1; i+1 < min(int(n.n), nodeSize); i++ {
					if gap := n.keys[i+1] - n.keys[i]; n.keys[i+1] > n.keys[i] {
						gaps = append(gaps, gap)
					}
				}
				break
			}
			n = nodeIn(dir, child)
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	slices.Sort(gaps)
	return gaps[len(gaps)/2]
}
