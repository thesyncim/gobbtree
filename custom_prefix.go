package gobbtree

import (
	"cmp"
	"math/bits"
	"unsafe"
)

// Custom keys' nodes keep prefixes as text nodes do (D26, D50): a node's
// prefix is what its fences share, the tree's shared prefix standing in for
// one missing on its borders, and its codes are its keys' windows past it,
// so they tell keys apart where they differ.
//
// A custom key has no bytes the tree reads, only windows, and an annexed
// tree's keys read theirs from where their leaf keeps them, which moves
// while the leaf is held. So a keyed leaf's head is a copy of its first key
// with that key's first 64 bytes as windows, taken as the head is set,
// while the leaf is held, and a node's prefix and codes are read from its
// fences' and separators' copies, never from their keys. Prefixes stop at
// customPrefixMax, so every window a node keeps lies in those 64 bytes: a
// prefix shorter than the fences share is always correct, its windows
// only tie more.

const (
	headWindows     = 8
	customPrefixMax = 8*headWindows - 8
)

// headCopy is a keyed leaf's head: its first key, and the first 64 bytes
// of that key's encoding as windows, zero past its end. It never changes;
// a new head, or the same key moved, is a new copy.
type headCopy[K any] struct {
	key K
	win [headWindows]uint64
}

// fence is a node's bound, a separator beside it in its ancestors, as its
// prefix is fit to it: a text key, or a custom key's head copy. The zero
// fence is none, on the tree's border, where the shared prefix stands in.
type fence[K any] struct {
	key  *K
	head *headCopy[K]
}

// fenceOfKey is text key k as a fence.
func fenceOfKey[K any](k K) fence[K] { return fence[K]{key: &k} }

// A custom key's code at a node's prefix p is its window there, upstream's
// inline key (D26), but for a prefix of fewer than 8 bytes, where it is its
// first window, at 0, past the prefix's bytes: what they share leaves the
// rest of that window to tell keys apart, as a code at 0 did, and a lookup,
// which takes its key's first window once, takes none for nodes whose
// prefixes are that short, as random keys' are below the borders. A prefix
// of 8 bytes or more strips them all.

// windowIn is the code at prefix p, at most customPrefixMax, of the
// encoding whose first 64 bytes w holds.
func windowIn(w *[headWindows]uint64, p int) uint64 {
	if p < 8 {
		return w[0] << (8 * uint(p&7))
	}
	j, r := (p>>3)&(headWindows-1), uint(p&7)*8
	if r == 0 {
		return w[j]
	}
	return w[j]<<r | w[(j+1)&(headWindows-1)]>>(64-r)
}

// codeOf is custom key key's code at prefix p, given w0, its window at 0.
func (t *genericTree[K, V, O]) codeOf(key K, w0 uint64, p int) uint64 {
	if p < 8 {
		return w0 << (8 * uint(p&7))
	}
	return windowKey[O](key, p)
}

// textWindows is s's first 64 bytes as windows, zero past its end.
func textWindows(s string) (w [headWindows]uint64) {
	for j := range w {
		w[j] = stringCode(s, 8*j)
	}
	return w
}

// commonWindows is how many leading bytes the encodings a and b hold share,
// at most limit.
func commonWindows(a, b *[headWindows]uint64, limit int) int {
	for j := range a {
		if x := a[j] ^ b[j]; x != 0 {
			return min(8*j+bits.LeadingZeros64(x)/8, limit)
		}
	}
	return limit
}

// keyWindows is custom key k's first 64 bytes as windows.
func (t *genericTree[K, V, O]) keyWindows(k K) (w [headWindows]uint64) {
	for j := range w {
		w[j] = windowKey[O](k, 8*j)
	}
	return w
}

// headOf is keyed leaf n's head copy, nil for the leftmost leaf.
func headOf[K any, V any](n *gleaf[K, V]) *headCopy[K] {
	return (*keyedLeaf[K, V])(unsafe.Pointer(n)).head.Load()
}

// fencePrefixOf is fencePrefix for custom keys: what fences low and high,
// either nil for none, share, by their copies' windows.
func (t *genericTree[K, V, O]) fencePrefixOf(low, high *headCopy[K]) int {
	shared := t.sharedText()
	if low == nil && high == nil {
		return len(shared)
	}
	limit := customPrefixMax
	var s [headWindows]uint64
	a, b := &s, &s
	if low == nil || high == nil {
		s, limit = textWindows(shared), len(shared)
	}
	if low != nil {
		a = &low.win
	}
	if high != nil {
		b = &high.win
	}
	return commonWindows(a, b, limit)
}

// sharedLenOf is how many of the shared prefix's bytes custom key key's
// encoding begins with, by its windows.
func (t *genericTree[K, V, O]) sharedLenOf(key K, shared string) int {
	for off := 0; off < len(shared); off += 8 {
		m := headMask(len(shared) - off)
		if x := (windowKey[O](key, off) ^ stringCode(shared, off)) & m; x != 0 {
			return off + bits.LeadingZeros64(x)/8
		}
	}
	return len(shared)
}

// sharedOrderOf is sharedOrder for custom keys: 0 where key's encoding
// begins with the shared prefix, else as key sorts against every key; by
// the key's ComparePrefix, where it has one.
func (t *genericTree[K, V, O]) sharedOrderOf(key K) int {
	shared := t.sharedText()
	if f := t.inline.comparePrefix; f != nil {
		return f(key, unsafe.Slice(unsafe.StringData(shared), len(shared)))
	}
	for off := 0; off < len(shared); off += 8 {
		m := headMask(len(shared) - off)
		if w, s := windowKey[O](key, off)&m, stringCode(shared, off)&m; w != s {
			return cmp.Compare(w, s)
		}
	}
	return 0
}

// leafOrderOf is leafOrder for custom keys: key's first p bytes against
// leaf n's prefix, its first 16 from its head windows and the rest from its
// first key, which n, latched, holds. w0 is key's window at 0.
func (t *genericTree[K, V, O]) leafOrderOf(n *gleaf[K, V], key K, w0 uint64, p int) int {
	for off := 0; off < p; off += 8 {
		m := headMask(p - off)
		var h uint64
		if off < 16 {
			h = n.head[(off>>3)&1].Load()
		} else {
			h = windowKey[O](t.key(n, 0), off) & m
		}
		w := w0
		if off > 0 {
			w = windowKey[O](key, off)
		}
		if w &= m; w != h {
			return cmp.Compare(w, h)
		}
	}
	return 0
}

// setPrefixOf makes h's prefix p bytes of the encoding whose windows at 0
// and 8 are w0 and w1, as setPrefix does a text key's.
func (h *gheader) setPrefixOf(w0, w1 uint64, p int) {
	h.prefix.Store(int32(p))
	h1 := uint64(0)
	if p > 8 {
		h1 = w1 & headMask(p-8)
	}
	h.head[0].Store(w0 & headMask(p))
	h.head[1].Store(h1)
}

// refitLeafOf is refitLeaf for custom keys: leaf n, held, takes prefix p,
// which its keys share, and its codes become their windows past it.
func (t *genericTree[K, V, O]) refitLeafOf(n *gleaf[K, V], p int) {
	count := int(n.count.Load())
	keys := n.keyed()
	if count == 0 {
		n.setPrefixOf(0, 0, 0)
		return
	}
	if q := int(n.prefix.Load()); q <= p && p < 8 && t.inline.leafWidth == 8 {
		// A prefix that grows below 8 bytes, as a split's halves' do: each
		// code, its key's first window past the prefix, shifts past the
		// bytes the prefix takes, as upstream's windows move
		// (05-fence-key.md), and only the first key's window, for the head,
		// is read.
		for i := 0; i < count; i++ {
			n.codes[i] <<= 8 * uint(p-q)
		}
		n.setPrefixOf(windowKey[O](keys[0], 0), 0, p)
		return
	}
	n.setPrefixOf(windowKey[O](keys[0], 0), windowKey[O](keys[0], 8), p)
	for i := 0; i < count; i++ {
		w0 := uint64(0)
		if p < 8 {
			w0 = windowKey[O](keys[i], 0)
		}
		n.codes[i] = narrowed(t.codeOf(keys[i], w0, p), t.inline.leafWidth)
	}
}

// headFence is leaf n's head, just set from its first key sep, as a fence.
func (t *genericTree[K, V, O]) headFence(sep K, n *gleaf[K, V]) fence[K] {
	if windowed[O, K]() {
		return fence[K]{head: headOf(n)}
	}
	return fenceOfKey(sep)
}

// windowed reports custom keys whose nodes keep prefixes by their windows:
// custom keys the tree doesn't read as text, as it does Bytes, whose custom
// order TreeOf takes.
func windowed[O keyOrder[K], K any]() bool {
	var o O
	var k K
	if unsafe.Sizeof(o) == 0 {
		return false
	}
	if unsafe.Sizeof(k) != unsafe.Sizeof(Bytes(nil)) {
		return true
	}
	_, ok := any(k).(Bytes)
	return !ok
}

// sharesBytes reports a shared prefix of at least a byte, or none set yet,
// which a custom key checks against: random keys share none.
func (t *genericTree[K, V, O]) sharesBytes() bool {
	s := t.shared.Load()
	return s == nil || len(*s) > 0
}

// narrowInner reports custom keys whose internal nodes keep windows of
// fewer than 8 bytes, as 4-byte summaries, the text nodes' layout.
func (t *genericTree[K, V, O]) narrowInner() bool {
	return windowed[O, K]() && t.inline.innerWidth < 8
}

// innerWidth is how many bytes of each window a custom key's internal
// nodes keep.
func (t *genericTree[K, V, O]) innerWidth() int {
	if t.narrowInner() {
		return t.inline.innerWidth
	}
	return 8
}

// compareTied orders keys a and b, whose codes tie, so that their first
// equal bytes are equal: by a custom key's CompareFrom where it has one.
func (t *genericTree[K, V, O]) compareTied(a, b K, equal int) int {
	if windowed[O, K]() {
		if f := t.inline.compareFrom; f != nil {
			return f(a, b, equal)
		}
	}
	return t.compare(a, b)
}

// tagged reports custom keys whose leaves keep each key's hash tag.
func (t *genericTree[K, V, O]) tagged() bool { return windowed[O, K]() && t.inline.hashTag != nil }
