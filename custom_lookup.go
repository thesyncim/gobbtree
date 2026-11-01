package gobbtree

import "unsafe"

// What a racing read of a custom key's leaf settled (leafByCodes).
const (
	codesUnsettled = iota
	codesAbsent
	codesHeld
)

// leafByCodes looks custom key key up as upstream's latched tree does a key
// that doesn't order by its own bytes (D57): the descent reads the internal
// nodes racing, by their codes, and the leaf too, at the version it was at.
// A key without the shared prefix, or one the leaf's codes and hash tags
// rule out, is absent, codesAbsent, once that version, unchanged after the
// read, confirms it: a miss writes no latch. Otherwise the leaf is latched
// shared, codesHeld, still at that version, and its candidates for key are
// [lo, hi), whose keys the caller compares holding it. Only key itself is
// read: no stored key, whose bytes an annexed tree's leaf may move. A code
// that ties with a separator's, which only the separator's key settles, or
// a node that changes, leaves it codesUnsettled, for the latched descent.
//
// It reads as textLeafOrdered does: each node's version by an acquire load
// and its words plainly, after it, and only the closing check, which every
// word read feeds, by address dependency, so that a node's lines are
// fetched together.
func (t *genericTree[K, V, O]) leafByCodes(key K, code uint64) (n *gleaf[K, V], lo, hi, state int) {
	if !seqlockPlainStores {
		return nil, 0, 0, codesUnsettled
	}
	if t.sharesBytes() && t.sharedOrderOf(key) != 0 {
		return nil, 0, 0, codesAbsent
	}
	root := (**gheader)(unsafe.Pointer(&t.root))
	h := *root
	if h == nil {
		return nil, 0, 0, codesUnsettled
	}
	word := h.latch.Load()
	if *after(root, word) != h {
		return nil, 0, 0, codesUnsettled
	}
	cp, c := 0, code
	narrow, width := t.narrowInner(), t.inline.innerWidth
	for depth := 0; ; depth++ {
		if word&latchExclusive != 0 || depth == maxDepth {
			return nil, 0, 0, codesUnsettled
		}
		if h.leaf {
			break
		}
		r := asInner[K](h)
		count := int(*(*int32)(unsafe.Pointer(&r.count)))
		q := int(*(*int32)(unsafe.Pointer(&r.prefix)))
		if count < 1 || count > nodeSize || q < 0 || q > customPrefixMax {
			return nil, 0, 0, codesUnsettled
		}
		if q != cp {
			cp, c = q, t.codeOf(key, code, q)
		}
		// The last child whose separator is at most key: the codes that
		// bracket the route prove it in the snapshot the closing check
		// vouches for, and one equal to key's is a tie, which only the
		// separator's key settles.
		var i int
		var below, next uint64
		if narrow {
			s := uint32(narrowed(c, width))
			sums := r.sums()
			i = max(lowerSummary(sums, count, s)-1, 0)
			b, x := sums[i&(nodeSize-1)], sums[(i+1)&(nodeSize-1)]
			if i > 0 && b >= s || i+1 < count && x <= s {
				return nil, 0, 0, codesUnsettled
			}
			below, next = uint64(b), uint64(x)
		} else {
			codes := (*[nodeSize]uint64)(unsafe.Pointer(&r.codes))
			i = max(lowerCode(codes, count, c)-1, 0)
			below, next = codes[i&(nodeSize-1)], codes[(i+1)&(nodeSize-1)]
			if i > 0 && below >= c || i+1 < count && next <= c {
				return nil, 0, 0, codesUnsettled
			}
		}
		child := *(**gheader)(unsafe.Pointer(&r.children[i&(nodeSize-1)]))
		if child == nil {
			return nil, 0, 0, codesUnsettled
		}
		childWord := child.latch.Load()
		acc := uint64(i) ^ uint64(count) ^ uint64(q)<<8 ^ below<<16 ^ next<<32
		closing := *after((*uint64)(unsafe.Pointer(&h.latch)), acc^childWord^uint64(uintptr(unsafe.Pointer(child))))
		if closing&^latchSharedMask != word&^latchSharedMask {
			return nil, 0, 0, codesUnsettled
		}
		h, word = child, childWord
	}
	// Key lies between the leaf's fences, so begins with its prefix (D26):
	// its candidates are the keys whose codes are its own, and of those
	// whose tags are its.
	m := asLeaf[K, V](h)
	count := int(*(*int32)(unsafe.Pointer(&m.count)))
	p := int(*(*int32)(unsafe.Pointer(&m.prefix)))
	if count < 0 || count > nodeSize || p < 0 || p > customPrefixMax {
		return nil, 0, 0, codesUnsettled
	}
	lo, hi = tieRange(&m.codes, count, narrowed(t.codeOf(key, code, p), t.inline.leafWidth))
	acc := uint64(lo) ^ uint64(hi)<<8 ^ uint64(count)<<16 ^ uint64(p)<<24
	if lo < hi && t.tagged() {
		tag, seen := t.inline.hashTag(key), false
		for j := lo; j < hi; j++ {
			got := m.tags[j&(nodeSize-1)]
			seen = seen || got == tag
			acc ^= uint64(got) << 32
		}
		if !seen {
			hi = lo
		}
	}
	if lo == hi {
		if *after((*uint64)(unsafe.Pointer(&h.latch)), acc)&^latchSharedMask == word&^latchSharedMask {
			return nil, 0, 0, codesAbsent
		}
		return nil, 0, 0, codesUnsettled
	}
	m.latch.lockShared()
	if m.latch.Load()&^latchSharedMask != word&^latchSharedMask {
		m.latch.unlockShared()
		return nil, 0, 0, codesUnsettled
	}
	return m, lo, hi, codesHeld
}

// settleHeld is the candidate of [lo, hi), the keys of leaf n whose codes
// are custom key key's, that is key, and whether one is; n is latched.
func (t *genericTree[K, V, O]) settleHeld(n *gleaf[K, V], key K, lo, hi int) (int, bool) {
	equal := equalThrough(int(n.prefix.Load()), t.inline.leafWidth)
	tagged := t.tagged()
	var tag uint8
	if tagged {
		tag = t.inline.hashTag(key)
	}
	for i := lo; i < hi; i++ {
		if tagged && n.tags[i&(nodeSize-1)] != tag {
			continue
		}
		if t.compareTied(t.key(n, i), key, equal) == 0 {
			return i & (nodeSize - 1), true
		}
	}
	return 0, false
}
