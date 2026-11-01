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
// [lo, hi), whose keys the caller compares holding it, with key's hash tag
// where its leaves keep them. Only key itself is read: no stored key, whose
// bytes an annexed tree's leaf may move. A code that ties with a
// separator's, which only the separator's key settles, or a node that
// changes, leaves it codesUnsettled, for the latched descent.
//
// The internal nodes are descendCodes's, which counts their separators
// inline and hands back only where a node's prefix needs key's code at it.
// Each node's version is read by an acquire load and its words plainly,
// after it, and only the closing check, which every word read feeds, by
// address dependency, so that a node's lines are fetched together.
func (t *genericTree[K, V, O]) leafByCodes(key K, code uint64) (n *gleaf[K, V], lo, hi int, tag uint8, state int) {
	if !seqlockPlainStores {
		return nil, 0, 0, 0, codesUnsettled
	}
	if t.sharesBytes() && t.sharedOrderOf(key) != 0 {
		return nil, 0, 0, 0, codesAbsent
	}
	h, word, ok := t.leafOfCodes(key, code)
	if !ok {
		return nil, 0, 0, 0, codesUnsettled
	}
	// Key lies between the leaf's fences, so begins with its prefix (D26):
	// its candidates are the keys whose codes are its own, and of several,
	// as upstream reads them, those whose tags are its.
	m := asLeaf[K, V](h)
	count := int(*(*int32)(unsafe.Pointer(&m.count)))
	p := int(*(*int32)(unsafe.Pointer(&m.prefix)))
	if count < 0 || count > nodeSize || p < 0 || p > customPrefixMax {
		return nil, 0, 0, 0, codesUnsettled
	}
	lo, hi = leafCodeRange(h, count, narrowed(t.codeOf(key, code, p), t.inline.leafWidth))
	acc := uint64(lo) ^ uint64(hi)<<8 ^ uint64(count)<<16 ^ uint64(p)<<24
	if hi-lo > 1 && t.tagged() {
		tag = t.inline.hashTag(key)
		seen := false
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
			return nil, 0, 0, 0, codesAbsent
		}
		return nil, 0, 0, 0, codesUnsettled
	}
	m.latch.lockShared()
	if m.latch.Load()&^latchSharedMask != word&^latchSharedMask {
		m.latch.unlockShared()
		return nil, 0, 0, 0, codesUnsettled
	}
	return m, lo, hi, tag, codesHeld
}

// leafOfCodes is custom key key's leaf by leafByCodes's racing descent, and
// the latch word it read the leaf at; false where the descent can't settle.
func (t *genericTree[K, V, O]) leafOfCodes(key K, code uint64) (*gheader, uint64, bool) {
	root := (**gheader)(unsafe.Pointer(&t.root))
	h := *root
	if h == nil {
		return nil, 0, false
	}
	word := h.latch.Load()
	if *after(root, word) != h {
		return nil, 0, false
	}
	narrow, width := t.narrowInner(), t.inline.innerWidth
	cp, c, depth := 0, code, 0
	var resumed *gheader
	for {
		var reached int
		h, word, depth, reached = descendCodes(h, word, depth, c, cp, narrow, width)
		if reached == codesAtLeaf {
			return h, word, true
		}
		// A node of another prefix takes key's code at it, once: a prefix
		// that moves again is a writer's, for the latched descent.
		if reached != codesAtPrefix || h == resumed {
			return nil, 0, false
		}
		q := int(*(*int32)(unsafe.Pointer(&h.prefix)))
		if q < 0 || q > customPrefixMax {
			return nil, 0, false
		}
		resumed, cp, c = h, q, t.codeOf(key, code, q)
	}
}

// writeLeafByCodes is custom key key's leaf, found by leafByCodes's racing
// descent and latched exclusively, and the version it holds it from: as
// upstream's writes descend as its lookups do, recording no path, and latch
// the leaf they reach (D37). False, holding nothing, where the descent
// can't settle, or the leaf changed since it read it.
func (t *genericTree[K, V, O]) writeLeafByCodes(key K, code uint64) (*gleaf[K, V], uint64, bool) {
	if !seqlockPlainStores || t.sharesBytes() && t.sharedOrderOf(key) != 0 {
		return nil, 0, false
	}
	h, word, ok := t.leafOfCodes(key, code)
	if !ok {
		return nil, 0, false
	}
	n := asLeaf[K, V](h)
	version := n.latch.lockExclusive()
	if version != word&^latchSharedMask {
		n.latch.unlockExclusive(version)
		return nil, 0, false
	}
	return n, version, true
}

// settleHeld is the candidate of [lo, hi), the keys of leaf n whose codes
// are custom key key's, that is key, and whether one is; n is latched, and
// tag is key's hash tag where its leaves keep them and the candidates are
// several. A lone candidate is compared.
func (t *genericTree[K, V, O]) settleHeld(n *gleaf[K, V], key K, lo, hi int, tag uint8) (int, bool) {
	equal := equalThrough(int(n.prefix.Load()), t.inline.leafWidth)
	tagged := hi-lo > 1 && t.tagged()
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
