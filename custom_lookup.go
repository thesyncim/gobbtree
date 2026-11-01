package gobbtree

import "unsafe"

// What a racing read of a custom key's leaf settled (leafByCodes): nothing,
// the key absent, its leaf held with candidates to compare, or its leaf
// held with its entry found.
const (
	codesUnsettled = iota
	codesAbsent
	codesHeld
	codesFound
)

// leafByCodes looks custom key key up as upstream's latched tree does a key
// that doesn't order by its own bytes (D57): the descent reads the internal
// nodes racing, by their codes, and the leaf too, at the version it was at.
// A key without the shared prefix, or one the leaf's codes and hash tags
// rule out, is absent, codesAbsent, once that version, unchanged after the
// read, confirms it: a miss writes no latch. Otherwise the leaf is latched
// shared, codesHeld, still at that version, and its candidates for key are
// [lo, hi), whose keys the caller compares holding it, with key's hash tag
// where its leaves keep them. A node that changes leaves it
// codesUnsettled, for the latched descent.
//
// The internal nodes are descendCodes's, which counts their separators
// inline and hands back where a node's prefix needs key's code at it, or
// where a separator's code ties with key's: its key settles the route, as
// the general descent's route settles it, an annexed tree's compared
// holding its leaf, whose records may move.
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
	// The leaf its hint names, where it settles key; else the descent's.
	if h := t.hintedLeaf(key, code); h != nil {
		if n, lo, hi, tag, state = t.leafCandidates(key, code, h, h.latch.Load(), true); state != codesUnsettled {
			return n, lo, hi, tag, state
		}
	}
	h, word, ok := t.leafOfCodes(key, code)
	if !ok {
		return nil, 0, 0, 0, codesUnsettled
	}
	t.noteLeaf(key, code, h)
	return t.leafCandidates(key, code, h, word, false)
}

// leafCandidates is leafByCodes's read of leaf h, at latch word word: the
// leaf key's descent reached, or, where hinted, one a hint named, which
// settles key only where key has its prefix and its code lies strictly
// between the leaf's first and last, or where key is found there: a key
// whose code is the leaf's first or last may be the leaf's, or a
// neighbor's.
func (t *genericTree[K, V, O]) leafCandidates(key K, code uint64, h *gheader, word uint64, hinted bool) (n *gleaf[K, V], lo, hi int, tag uint8, state int) {
	if word&latchExclusive != 0 {
		return nil, 0, 0, 0, codesUnsettled
	}
	// Key lies between a reached leaf's fences, so begins with its prefix
	// (D26): its candidates are the keys whose codes are its own, and of
	// several, as upstream reads them, those whose tags are its.
	m := asLeaf[K, V](h)
	count := int(*(*int32)(unsafe.Pointer(&m.count)))
	p := int(*(*int32)(unsafe.Pointer(&m.prefix)))
	if count < 0 || count > nodeSize || p < 0 || p > customPrefixMax {
		return nil, 0, 0, 0, codesUnsettled
	}
	acc := uint64(count)<<16 ^ uint64(p)<<24
	if hinted && (p <= 8 && !hasShortPrefix(h, code, p) || p > 8 && !t.hasLeafPrefix(m, key, code, p)) {
		return nil, 0, 0, 0, codesUnsettled
	}
	lo, hi = leafCodeRange(h, count, narrowed(t.codeOf(key, code, p), t.inline.leafWidth))
	edge := hinted && (lo == 0 || hi >= count)
	if edge && lo == hi {
		return nil, 0, 0, 0, codesUnsettled
	}
	acc ^= uint64(lo) ^ uint64(hi)<<8
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
		if !edge && *after((*uint64)(unsafe.Pointer(&h.latch)), acc)&^latchSharedMask == word&^latchSharedMask {
			return nil, 0, 0, 0, codesAbsent
		}
		return nil, 0, 0, 0, codesUnsettled
	}
	m.latch.lockShared()
	if m.latch.Load()&^latchSharedMask != word&^latchSharedMask {
		m.latch.unlockShared()
		return nil, 0, 0, 0, codesUnsettled
	}
	if edge {
		// Found there, key is the leaf's; else the descent settles it.
		if i, ok := t.settleHeld(m, key, lo, hi, tag); ok {
			return m, i, i + 1, tag, codesFound
		}
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
		switch reached {
		case codesAtLeaf:
			return h, word, true
		case codesAtPrefix:
			// A node of another prefix takes key's code at it, once: a
			// prefix that moves again is a writer's, for the latched
			// descent.
			if h == resumed {
				return nil, 0, false
			}
			q := int(*(*int32)(unsafe.Pointer(&h.prefix)))
			if q < 0 || q > customPrefixMax {
				return nil, 0, false
			}
			resumed, cp, c = h, q, t.codeOf(key, code, q)
		case codesTied:
			// The separators settle a tie, as the general descent's route
			// settles it, the node checked unchanged after: the descent
			// goes on from its child, not from the root.
			n := asInner[K](h)
			i, ok := t.route(n, key, c, int(n.count.Load()), false)
			if !ok {
				return nil, 0, false
			}
			child := n.children[i&(nodeSize-1)].Load()
			if child == nil {
				return nil, 0, false
			}
			childWord := child.latch.Load()
			if !h.latch.isUnchanged(word &^ latchSharedMask) {
				return nil, 0, false
			}
			h, word, depth = child, childWord, depth+1
		default:
			return nil, 0, false
		}
	}
}

// writeLeafByCodes is custom key key's leaf, found by leafByCodes's racing
// descent and latched exclusively, and the version it holds it from: as
// upstream's writes descend as its lookups do, recording no path, and latch
// the leaf they reach (D37). False, holding nothing, where the descent
// can't settle, or the leaf changed since it read it.
func (t *genericTree[K, V, O]) writeLeafByCodes(key K, code uint64) (*gleaf[K, V], uint64, bool) {
	n, version, _, _, _, ok := t.writeLeafRange(key, code)
	return n, version, ok
}

// writeLeafRange is writeLeafByCodes that, where a hint named the leaf,
// hands on [lo, hi), the keys whose codes are key's, which it read racing
// at the version it then latched the leaf at, so still the leaf's: ranged.
func (t *genericTree[K, V, O]) writeLeafRange(key K, code uint64) (n *gleaf[K, V], version uint64, lo, hi int, ranged, ok bool) {
	if !seqlockPlainStores || t.sharesBytes() && t.sharedOrderOf(key) != 0 {
		return nil, 0, 0, 0, false, false
	}
	h, word, lo, hi, ranged := t.hintedWriteLeaf(key, code)
	if !ranged {
		if h, word, ok = t.leafOfCodes(key, code); !ok {
			return nil, 0, 0, 0, false, false
		}
		t.noteLeaf(key, code, h)
	}
	n = asLeaf[K, V](h)
	if version = n.latch.lockExclusive(); version != word&^latchSharedMask {
		n.latch.unlockExclusive(version)
		return nil, 0, 0, 0, false, false
	}
	return n, version, lo, hi, ranged, true
}

// hintedWriteLeaf is the leaf key's hint names and the latch word it read
// it at, where read racing at that word the leaf settles key as a lookup's
// does (leafCandidates), its first and last codes strictly about key's:
// the writer that latches it at that word holds key's leaf.
func (t *genericTree[K, V, O]) hintedWriteLeaf(key K, code uint64) (h *gheader, word uint64, lo, hi int, ok bool) {
	if h = t.hintedLeaf(key, code); h == nil {
		return nil, 0, 0, 0, false
	}
	if word = h.latch.Load(); word&latchExclusive != 0 {
		return nil, 0, 0, 0, false
	}
	m := asLeaf[K, V](h)
	count := int(*(*int32)(unsafe.Pointer(&m.count)))
	p := int(*(*int32)(unsafe.Pointer(&m.prefix)))
	if count < 2 || count > nodeSize || p < 0 || p > customPrefixMax {
		return nil, 0, 0, 0, false
	}
	if p <= 8 && !hasShortPrefix(h, code, p) || p > 8 && !t.hasLeafPrefix(m, key, code, p) {
		return nil, 0, 0, 0, false
	}
	lo, hi = leafCodeRange(h, count, narrowed(t.codeOf(key, code, p), t.inline.leafWidth))
	return h, word, lo, hi, lo > 0 && hi < count
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
