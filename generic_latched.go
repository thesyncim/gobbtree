package gobbtree

import (
	"cmp"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// genericTree is the Map engine for keys or values that don't fit a word,
// upstream's latched tree (tree/latched/) over nodes of any key and value
// type, as latchedTree is for words:
//
//   - Descent is optimistic through the internal nodes, by lock coupling:
//     each node's version is checked after its child's is read. Leaves are
//     latched, shared to read and exclusive to change.
//   - A split, or a removal that rebalances, takes without waiting every
//     node it changes, at the versions its descent read, and starts over if
//     another goroutine holds one.
//   - A racing reader reads only words that can't tear: counts, separator
//     codes and pointers. Keys and values are read with their leaf latched,
//     as upstream reads what isn't Copy. A separator is a boxed key, never
//     changed once boxed, so a racing reader compares it whole.
//   - Freed nodes hold nothing and keep their latch, whose version their
//     release bumps; the garbage collector keeps one alive while a stale
//     pointer reaches it, so a reader that does fails its check.
type genericTree[K any, V any, O keyOrder[K]] struct {
	root atomic.Pointer[gheader]
	// shared is what every text key begins with, upstream's shared prefix
	// (D50), at most 64 bytes, nil before the first key: a query without it
	// sorts before or after every key and reads no node, and nodes on the
	// tree's borders take it for the fence they lack.
	shared   atomic.Pointer[string]
	starting sync.Mutex
	// owner marks a Tree's tree (generic_latched_owner.go).
	owner bool
	// An owner's text leaves reuse their buffers' bytes, a removed key's for
	// the next to go in and a full buffer's for its live keys, while no key
	// read over them is out: epoch is the generation of buffers the writer
	// makes, and handed the last whose keys were handed out, which leaves
	// it and every buffer before it to grow by appending.
	epoch  uint32
	handed atomic.Uint32
}

// A node's header. Its count is read racing; whether it is a leaf never
// changes.
//
// Text keys skip what a node's keys share: prefix bytes every key of a leaf,
// or every separator of an internal node past its own, begins with, the
// first 16 of which head holds as windows. Codes are windows past them, so
// they tell a node's keys apart where the keys differ.
type gheader struct {
	latch  nodeLatch
	count  atomic.Int32
	prefix atomic.Int32
	head   [2]atomic.Uint64
	leaf   bool
}

// A leaf: entries in key order, with each key's code, its window past the
// prefix, which narrows a search before keys are compared. Code slots past
// the count hold codePad, as lowerCode's vector kernels count every slot.
// Its keys follow it in one allocation: a keyed leaf's as they are, a text
// leaf's as bytes.
type gleaf[K any, V any] struct {
	gheader
	codes  [nodeSize]uint64
	values [nodeSize]V
}

// A keyed leaf keeps its keys, which aren't text, as they are.
type keyedLeaf[K any, V any] struct {
	gleaf[K, V]
	keys [nodeSize]K
}

// A text leaf also keeps, for each key, its window past the code's (its
// slot's tail) and its length, upstream's inline key and hash tag (D14,
// D36): a key no longer than the prefix and 16 bytes is then compared
// within the leaf, without reading its bytes.
//
// It keeps its keys' bytes in a buffer of its own, which holds no pointer
// for the collector to scan, each key's at the offset and length its slot's
// ref gives: the leaf holds one pointer where it held one a key. Bytes are
// written once, as the keys the tree hands out read them: a key the leaf
// drops keeps its bytes, and a leaf that fills its buffer takes a new one
// of its live keys, which leaves the old to whatever still reads it.
type textLeaf[K any, V any] struct {
	gleaf[K, V]
	textColumns
}

type textColumns struct {
	slots [nodeSize]textSlot
	// buf holds size bytes, the first used of them written. In an owner's
	// tree, hole is a removed key's ref, bytes the next key may take while
	// the buffer's epoch is past the tree's handed one.
	buf        unsafe.Pointer
	used, size uint32
	epoch      uint32
	hole       uint64
}

// A text slot: a key's tail, and its ref, which a lookup reads together.
type textSlot struct {
	tail, ref uint64
}

// keyRef is the ref of a key of length bytes at offset in its leaf's buffer.
func keyRef(offset, length int) uint64 { return uint64(offset)<<32 | uint64(length) }

// refLen is the length of the key ref refers to.
func refLen(ref uint64) int { return int(uint32(ref)) }

// keyAt is the text ref gives in buf. An empty key reads no byte, so that
// it points nowhere, not past the buffer's end.
func keyAt(buf unsafe.Pointer, ref uint64) string {
	length := int(uint32(ref))
	if length == 0 {
		return ""
	}
	return unsafe.String((*byte)(unsafe.Add(buf, ref>>32)), length)
}

// keyed is keyed leaf n's keys.
func (n *gleaf[K, V]) keyed() *[nodeSize]K { return &(*keyedLeaf[K, V])(unsafe.Pointer(n)).keys }

// key is leaf n's key i, n held or owned, to hand out. A text key reads
// its leaf's buffer, which it keeps alive as long as it lives, and in an
// owner's tree keeps from reuse.
func (t *genericTree[K, V, O]) key(n *gleaf[K, V], i int) K {
	if prefixed[O, K]() {
		if t.owner {
			t.handOut()
		}
		return keyOfText[K](t.keyText(n, i))
	}
	return n.keyed()[i&(nodeSize-1)]
}

// handOut marks the buffers an owner's tree has made as read by a key
// handed out, which their bytes must then keep. Readers may race to mark,
// atomically, but only while no writer runs.
func (t *genericTree[K, V, O]) handOut() {
	if e := t.epoch; t.handed.Load() != e {
		t.handed.Store(e)
	}
}

// reusable reports whether text leaf n's buffer may have its bytes
// written again: an owner's, made since the last key was handed out.
func (t *genericTree[K, V, O]) reusable(n *gleaf[K, V]) bool {
	return t.owner && n.text().epoch > t.handed.Load()
}

// keyText is text leaf n's key i as text, n held or owned.
func (t *genericTree[K, V, O]) keyText(n *gleaf[K, V], i int) string {
	c := n.text()
	return keyAt(c.buf, c.slots[i&(nodeSize-1)].ref)
}

// separator is leaf n's first key, boxed for a separator above it: a text
// key copied, so that the separator keeps no leaf's buffer alive.
func (t *genericTree[K, V, O]) separator(n *gleaf[K, V]) *K {
	if prefixed[O, K]() {
		return box(keyOfText[K](strings.Clone(t.keyText(n, 0))))
	}
	return box(n.keyed()[0])
}

// text is text leaf n's columns.
func (n *gleaf[K, V]) text() *textColumns {
	return (*textColumns)(unsafe.Add(unsafe.Pointer(n), unsafe.Offsetof(textLeaf[K, V]{}.textColumns)))
}

// An internal node: child i holds keys at least separator i, whose code,
// its window at offset 0, routes without reading the key. Code 0 is 0 and
// slots past the count hold codePad. Separator 0 is the node's own, nil on
// the tree's left border; high is the separator above the node, nil on its
// right border. Separators are boxed keys, never changed once boxed.
//
// A text node routes as upstream's do (D26, D43, D50): its prefix is what its
// fences share, a missing fence taken as the tree's shared prefix, so every
// key under it begins with the prefix and a descent skips it unchecked; and
// in place of codes it keeps 4-byte summaries, the top half of each
// separator's window past the prefix, all 32 in the codes' first 128 bytes.
// Summaries that tie are settled by comparing the boxed separators. Upstream
// keeps no separator keys and latches the leaf whose head is one to compare
// it; a boxed key, which the collector keeps whole, needs no latch.
type ginner[K any] struct {
	gheader
	codes    [nodeSize]atomic.Uint64
	children [nodeSize]atomic.Pointer[gheader]
	seps     [nodeSize]atomic.Pointer[K]
	high     atomic.Pointer[K]
}

// sums is text node n's summaries, the first half of its codes.
func (n *ginner[K]) sums() *[nodeSize]uint32 { return (*[nodeSize]uint32)(unsafe.Pointer(&n.codes)) }

// loadSum and storeSum read and write summary i, racing where a version
// check covers it.
func loadSum(sums *[nodeSize]uint32, i int) uint32 {
	if seqlockPlainStores {
		return sums[i&(nodeSize-1)]
	}
	return atomic.LoadUint32(&sums[i&(nodeSize-1)])
}

func storeSum(sums *[nodeSize]uint32, i int, v uint32) {
	if seqlockPlainStores {
		sums[i&(nodeSize-1)] = v
		return
	}
	atomic.StoreUint32(&sums[i&(nodeSize-1)], v)
}

// summaryOf is text s's summary at prefix p.
func summaryOf(s string, p int) uint32 { return uint32(stringCode(s, p) >> 32) }

const codePad = ^uint64(0)

func asLeaf[K any, V any](h *gheader) *gleaf[K, V] { return (*gleaf[K, V])(unsafe.Pointer(h)) }
func asInner[K any](h *gheader) *ginner[K]         { return (*ginner[K])(unsafe.Pointer(h)) }

// freed marks a node a split or merge let go of: it holds nothing, so a
// stale route to it starts over, and its version moves when it is let go.
func (h *gheader) freed() { h.count.Store(0) }

// newGLeaf and newGInner make a node latched exclusively, at version 0,
// until the operation making it lets go.
func newGLeaf[K any, V any](text bool) *gleaf[K, V] {
	var n *gleaf[K, V]
	if text {
		n = &new(textLeaf[K, V]).gleaf
	} else {
		n = &new(keyedLeaf[K, V]).gleaf
	}
	n.leaf = true
	for i := range n.codes {
		n.codes[i] = codePad
	}
	n.latch.Store(latchExclusive)
	return n
}

func newGInner[K any]() *ginner[K] {
	n := &ginner[K]{}
	for i := range n.codes {
		n.codes[i].Store(codePad)
	}
	n.latch.Store(latchExclusive)
	return n
}

// Which entry a descent seeks: a key, or an end of the tree.
const (
	seekKey = iota
	seekFirst
	seekLast
)

// exactCodes reports keys whose codes order them exactly: built-in word
// keys in their natural order. Their equal codes are equal keys.
func exactCodes[O keyOrder[K], K any]() bool { return wordKeyKind[O, K]() != 0 }

// codeRange is where codes, count of them in order, equal code: [lo, hi).
// It reads them racing, by plain loads where a version check covers them.
func codeRange(codes *[nodeSize]atomic.Uint64, count int, code uint64) (lo, hi int) {
	if seqlockPlainStores {
		plain := (*[nodeSize]uint64)(unsafe.Pointer(codes))
		lo, hi = lowerCode(plain, count, code), count
		if code != ^uint64(0) {
			hi = lowerCode(plain, count, code+1)
		}
		return lo, hi
	}
	lo, hi = 0, count
	for lo < hi {
		mid := int(uint(lo+hi)>>1) & (nodeSize - 1)
		if codes[mid].Load() < code {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	end := count
	for a := lo; a < end; {
		mid := int(uint(a+end)>>1) & (nodeSize - 1)
		if codes[mid].Load() <= code {
			a = mid + 1
		} else {
			end = mid
		}
	}
	return lo, end
}

// tieRange is codeRange over a leaf's plain codes: one search finds the
// first code at least code, and the codes equal to it, rare past a prefix,
// follow.
func tieRange(codes *[nodeSize]uint64, count int, code uint64) (lo, hi int) {
	lo = lowerCode(codes, count, code)
	hi = lo
	for hi < count && codes[hi&(nodeSize-1)] == code {
		hi++
	}
	return lo, hi
}

// route is the child of internal node n, of count children, whose keys
// hold key, read racing: the last whose separator is at most key, or below
// it when strict. Codes, or a text node's summaries, settle it unless they
// tie; then boxed separators, never changed, are compared whole. ok is
// false where a racing read finds no separator.
func (t *genericTree[K, V, O]) route(n *ginner[K], key K, code uint64, count int, strict bool) (int, bool) {
	var lo, hi int
	if prefixed[O, K]() {
		// Every key under n begins with its prefix (D26): no check.
		s := summaryOf(textView(&key), int(n.prefix.Load()))
		sums := n.sums()
		if seqlockPlainStores {
			lo = lowerSummary(sums, count, s)
		} else {
			lo = lowerSummaryAtomic(sums, count, s)
		}
		hi = lo
		for hi < count && loadSum(sums, hi) == s {
			hi++
		}
	} else if seqlockPlainStores {
		lo, hi = tieRange((*[nodeSize]uint64)(unsafe.Pointer(&n.codes)), count, code)
	} else {
		lo, hi = codeRange(&n.codes, count, code)
	}
	if lo == hi || exactCodes[O, K]() {
		if strict || lo == hi {
			return max(lo-1, 0), true
		}
		return hi - 1, true
	}
	// Separator 0 bounds the node from below, so it is never above key.
	lo = max(lo, 1)
	for lo < hi {
		mid := int(uint(lo+hi)>>1) & (nodeSize - 1)
		sep := n.seps[mid].Load()
		if sep == nil {
			return 0, false
		}
		order := t.compare(*sep, key)
		if order > 0 || strict && order == 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo - 1, true
}

// lowerSummaryAtomic is lowerSummary by atomic loads, for builds whose
// writers store atomically.
func lowerSummaryAtomic(sums *[nodeSize]uint32, count int, s uint32) int {
	lo, hi := 0, count
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if loadSum(sums, mid) < s {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// A descent's path: each internal node, its version, the edge taken, its
// count, and the separators beside the edge, its subtree's fences.
type gpathLevel[K any] struct {
	n         *ginner[K]
	version   uint64
	edge      int
	count     int
	low, high *K
}

type gpath[K any] struct {
	levels [maxDepth]gpathLevel[K]
	depth  int
}

// highFence is the lowest key past the leaf the path leads to, or nil.
func (p *gpath[K]) highFence() *K {
	for level := p.depth - 1; level >= 0; level-- {
		if l := &p.levels[level]; l.edge+1 < l.count {
			return l.high
		}
	}
	return nil
}

// lowFence is the lowest key of the leaf the path leads to, or nil for the
// first leaf.
func (p *gpath[K]) lowFence() *K {
	for level := p.depth - 1; level >= 0; level-- {
		if l := &p.levels[level]; l.edge > 0 {
			return l.low
		}
	}
	return nil
}

func latchHeader(h *gheader, mode int) uint64 {
	switch mode {
	case shared:
		h.latch.lockShared()
		return 0
	case exclusive:
		return h.latch.lockExclusive()
	}
	return h.latch.readVersion()
}

func unlatchHeader(h *gheader, mode int, version uint64) {
	switch mode {
	case shared:
		h.latch.unlockShared()
	case exclusive:
		h.latch.unlockExclusive(version)
	}
}

func (t *genericTree[K, V, O]) compare(a, b K) int { return compareKeys[O](a, b) }

// descend finds the leaf that seek's entry belongs in and latches it in
// mode, by optimistic lock coupling through the internal nodes; strict
// seeks the leaf of keys below key, whose code is code. path, if not nil,
// keeps the internal nodes. The tree has a root.
func (t *genericTree[K, V, O]) descend(key K, code uint64, seek int, strict bool, mode int, path *gpath[K]) (*gleaf[K, V], uint64) {
	if prefixed[O, K]() && seek == seekKey {
		// A key without the shared prefix sorts before or after every key:
		// its leaf is the first or the last. Writers admit theirs first.
		switch t.sharedOrder(key) {
		case -1:
			seek = seekFirst
		case 1:
			seek = seekLast
		}
	}
restart:
	for {
		if path != nil {
			path.depth = 0
		}
		h := t.root.Load()
		if h.leaf {
			version := latchHeader(h, mode)
			if t.root.Load() != h {
				unlatchHeader(h, mode, version)
				continue
			}
			return asLeaf[K, V](h), version
		}
		n := asInner[K](h)
		v := h.latch.readVersion()
		if t.root.Load() != h {
			continue
		}
		for depth := 0; depth < maxDepth; depth++ {
			count := int(n.count.Load())
			if count == 0 || count > nodeSize {
				continue restart
			}
			var i int
			switch seek {
			case seekFirst:
				i = 0
			case seekLast:
				i = count - 1
			default:
				var ok bool
				if i, ok = t.route(n, key, code, count, strict); !ok {
					continue restart
				}
			}
			child := n.children[i].Load()
			var low, high *K
			if path != nil {
				if i > 0 {
					low = n.seps[i].Load()
				}
				if i+1 < count {
					high = n.seps[i+1].Load()
				}
			}
			if child == nil || !h.latch.isUnchanged(v) {
				continue restart
			}
			if path != nil {
				path.levels[path.depth] = gpathLevel[K]{n: n, version: v, edge: i, count: count, low: low, high: high}
				path.depth++
			}
			if child.leaf {
				version := latchHeader(child, mode)
				if !h.latch.isUnchanged(v) {
					unlatchHeader(child, mode, version)
					continue restart
				}
				return asLeaf[K, V](child), version
			}
			cv := child.latch.readVersion()
			if !h.latch.isUnchanged(v) {
				continue restart
			}
			h, n, v = child, asInner[K](child), cv
		}
	}
}

func (t *genericTree[K, V, O]) code(key K) uint64 { return windowKey[O](key, 0) }

// prefixed reports keys whose nodes keep prefixes: built-in text, whose
// bytes are compared directly, with no user code.
func prefixed[O keyOrder[K], K any]() bool { return directText[O, K]() }

func prefixOf(s string, p int) string { return s[:min(len(s), p)] }

// headOrder orders s's first p bytes, 0 < p <= 16, against a prefix whose
// windows at 0 and 8, cut to p bytes, are h0 and h1.
func headOrder(s string, p int, h0, h1 uint64) int {
	w := stringCode(s, 0) & headMask(p)
	if w == h0 && p > 8 {
		w, h0 = stringCode(s, 8)&headMask(p-8), h1
	}
	switch {
	case w < h0:
		return -1
	case w > h0:
		return 1
	case len(s) < p:
		// s is the prefix's own beginning, or matches its NULs with padding.
		return -1
	}
	return 0
}

// headMask keeps a window's first p bytes, all eight from 8 up. It doesn't
// branch: a shift past the word gives 0, and a leaf's prefix length, which
// varies from leaf to leaf, would mispredict.
func headMask(p int) uint64 { return ^(^uint64(0) >> (8 * uint(p))) }

// headWindow is text s's window at 8, by fastWindow, which where it
// declines has no bytes to read: stringCode's there too.
func headWindow(s string) uint64 {
	w, _ := fastWindow(s, 8)
	return w
}

// headDiff is nonzero where windows w0 and w1, at 0 and 8, differ in their
// first p bytes, at most 16, from h0 and h1, the windows of a prefix cut to
// p bytes: a text key of p bytes or more begins with the prefix where it is
// 0. It compares both windows rather than branch on p, and it inlines.
func headDiff(w0, w1 uint64, p int, h0, h1 uint64) uint64 {
	return (w0^h0)&headMask(p) | (w1^h1)&headMask(max(p-8, 0))
}

// setPrefix makes h's prefix the first p bytes of key.
func (h *gheader) setPrefix(key string, p int) {
	h.prefix.Store(int32(p))
	h0, h1 := stringCode(key, 0)&headMask(p), uint64(0)
	if p > 8 {
		h1 = stringCode(key, 8) & headMask(p-8)
	}
	h.head[0].Store(h0)
	h.head[1].Store(h1)
}

// commonPrefix is what text keys a and b begin with, at most 65535 bytes.
func commonPrefix(a, b string) int {
	end, p := min(len(a), len(b), 65535), 0
	for p+8 <= end && stringCode(a, p) == stringCode(b, p) {
		p += 8
	}
	for p < end && a[p] == b[p] {
		p++
	}
	return p
}

// A leaf's prefix is what its fences share (D26): the separators beside it
// in its ancestors, the tree's shared prefix standing in for one missing on
// its borders (D50). Every key a route takes to the leaf lies between them,
// so an insert never moves it; a split, merge or borrow does, and the leaves
// it changes refit to their new fences.

// leafFences is the fences of internal node n's child edge: the separator
// it hangs from, and the next, or n's own where it is the first or last.
func leafFences[K any](n *ginner[K], edge int) (low, high *K) {
	low = n.seps[edge&(nodeSize-1)].Load()
	if edge+1 < int(n.count.Load()) {
		return low, n.seps[(edge+1)&(nodeSize-1)].Load()
	}
	return low, n.high.Load()
}

// fitChild is fitLeaf for leaf n, internal node parent's child edge.
func (t *genericTree[K, V, O]) fitChild(n *gleaf[K, V], parent *ginner[K], edge int) {
	low, high := leafFences(parent, edge)
	t.fitLeaf(n, low, high)
}

// fitLeaf refits text leaf n, held, to its fences low and high, from its
// keys. An emptied leaf, only ever the root, keeps no prefix.
func (t *genericTree[K, V, O]) fitLeaf(n *gleaf[K, V], low, high *K) {
	if !prefixed[O, K]() {
		return
	}
	if n.count.Load() == 0 {
		n.setPrefix("", 0)
		return
	}
	t.refitLeaf(n, t.fencePrefix(low, high))
}

// refitLeaf sets text leaf n's prefix to p, which its keys share, and
// recomputes its windows from its keys. n is held.
func (t *genericTree[K, V, O]) refitLeaf(n *gleaf[K, V], p int) {
	count := int(n.count.Load())
	n.setPrefix(t.keyText(n, 0), p)
	for i := 0; i < count; i++ {
		s := t.keyText(n, i)
		n.codes[i], n.text().slots[i].tail = stringCode(s, p), stringCode(s, p+8)
	}
}

// growLeaf moves text leaf n, whose windows are its keys' at its prefix q,
// to prefix p, at least q, which its keys share, as a split's halves do:
// each window shifts past the bytes the prefix now takes, the columns
// holding every byte of a key that fits them, and only keys too long for
// them, and the first for the head, are read.
func (t *genericTree[K, V, O]) growLeaf(n *gleaf[K, V], p int) {
	q := int(n.prefix.Load())
	if p <= q {
		if p < q {
			t.refitLeaf(n, p)
		}
		return
	}
	count, d := int(n.count.Load()), p-q
	c := n.text()
	for i := 0; i < count; i++ {
		code, tail := n.codes[i], c.slots[i].tail
		switch {
		case refLen(c.slots[i].ref) > q+16:
			s := t.keyText(n, i)
			code, tail = stringCode(s, p), stringCode(s, p+8)
		case d >= 8:
			code, tail = tail<<(8*(d-8)), 0
		default:
			code, tail = code<<(8*d)|tail>>(64-8*d), tail<<(8*d)
		}
		n.codes[i], c.slots[i].tail = code, tail
	}
	n.setPrefix(t.keyText(n, 0), p)
}

// textProbe is a text key as a leaf at prefix p compares it: its code and
// tail windows there, and its length.
type textProbe struct {
	code, tail uint64
	len        int
	settles    bool
}

func probeText(s string, p int) textProbe {
	tail, ok := fastWindow(s, p+8)
	if !ok {
		tail = stringCode(s, p+8)
	}
	return textProbe{stringCode(s, p), tail, len(s), len(s) <= p+16}
}

// slotOrder orders text leaf n's key at slot i, whose code equals k's, at
// prefix p, against k's key, within the leaf: by tails, then by lengths
// where a key fits its windows, a longer key extending the other. settled
// is false where neither key fits, and their bytes must be compared.
func slotOrder[K any, V any](n *gleaf[K, V], i int, k *textProbe, p int) (order int, settled bool) {
	slot := &n.text().slots[i&(nodeSize-1)]
	if slot.tail != k.tail {
		if slot.tail < k.tail {
			return -1, true
		}
		return 1, true
	}
	l := refLen(slot.ref)
	fits := l <= p+16
	switch {
	case fits && k.settles:
		return cmp.Compare(l, k.len), true
	case k.settles:
		return 1, true
	case fits:
		return -1, true
	}
	return 0, false
}

// refitInner sets internal node n's prefix, for text keys what its fences
// share, and recomputes its codes or summaries from its separators, padding
// the slots past its count. n is held.
func (t *genericTree[K, V, O]) refitInner(n *ginner[K]) {
	count := int(n.count.Load())
	if !prefixed[O, K]() {
		for i := 0; i < nodeSize; i++ {
			code := codePad
			switch {
			case i == 0 && count > 0:
				code = 0
			case i < count:
				code = windowKey[O](*n.seps[i].Load(), 0)
			}
			n.codes[i].Store(code)
		}
		return
	}
	p := t.fencePrefix(n.seps[0].Load(), n.high.Load())
	n.prefix.Store(int32(p))
	sums := n.sums()
	for i := 0; i < nodeSize; i++ {
		sum := ^uint32(0)
		switch {
		case i == 0 && count > 0:
			sum = 0
		case i < count:
			sum = summaryOf(textView(n.seps[i].Load()), p)
		}
		storeSum(sums, i, sum)
	}
}

// sharedText is the tree's shared prefix, empty before its first key.
func (t *genericTree[K, V, O]) sharedText() string {
	if s := t.shared.Load(); s != nil {
		return *s
	}
	return ""
}

// fencePrefix is what every key between fences low and high begins with:
// what they share, a missing one taken as the shared prefix (D50).
func (t *genericTree[K, V, O]) fencePrefix(low, high *K) int {
	shared := t.sharedText()
	if low == nil && high == nil {
		return len(shared)
	}
	a, b := shared, shared
	if low != nil {
		a = textView(low)
	}
	if high != nil {
		b = textView(high)
	}
	return commonPrefix(a, b)
}

// sharedOrder orders text key against the shared prefix: 0 where key begins
// with it, else as key sorts against every key, which do.
func (t *genericTree[K, V, O]) sharedOrder(key K) int {
	shared, s := t.sharedText(), textView(&key)
	if len(s) >= len(shared) && s[:len(shared)] == shared {
		return 0
	}
	if strings.Compare(prefixOf(s, len(shared)), shared) < 0 {
		return -1
	}
	return 1
}

// leafOrder orders key's first p bytes against leaf n's prefix of p bytes;
// n is latched, or p is at most 16.
func (t *genericTree[K, V, O]) leafOrder(n *gleaf[K, V], key K, p int) int {
	if p <= 16 {
		return headOrder(textView(&key), p, n.head[0].Load(), n.head[1].Load())
	}
	return strings.Compare(prefixOf(textView(&key), p), t.keyText(n, 0)[:p])
}

// find is key's slot in leaf n, latched, and whether key is there.
func (t *genericTree[K, V, O]) find(n *gleaf[K, V], key K, code uint64) (int, bool) {
	count := int(n.count.Load())
	if prefixed[O, K]() {
		p := int(n.prefix.Load())
		if p > 0 {
			switch order := t.leafOrder(n, key, p); {
			case order < 0:
				return 0, false
			case order > 0:
				return count, false
			}
		}
		k := probeText(textView(&key), p)
		lo, hi := tieRange(&n.codes, count, k.code)
		order := 1
		for lo < hi {
			mid := int(uint(lo+hi)>>1) & (nodeSize - 1)
			o, settled := slotOrder(n, mid, &k, p)
			if !settled {
				o = t.compare(keyOfText[K](t.keyText(n, mid)), key)
			}
			if o < 0 {
				lo = mid + 1
			} else {
				hi, order = mid, o
			}
		}
		return lo, lo < count && order == 0
	}
	lo := lowerCode(&n.codes, count, code)
	if exactCodes[O, K]() {
		return lo, lo < count && n.codes[lo&(nodeSize-1)] == code
	}
	hi := count
	if code != ^uint64(0) {
		hi = lowerCode(&n.codes, count, code+1)
	}
	for lo < hi {
		mid := int(uint(lo+hi)>>1) & (nodeSize - 1)
		if t.compare(t.key(n, mid), key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < count && t.compare(t.key(n, lo), key) == 0
}

// start makes the tree's root leaf, once.
func (t *genericTree[K, V, O]) start() {
	t.starting.Lock()
	defer t.starting.Unlock()
	if t.root.Load() == nil {
		n := newGLeaf[K, V](prefixed[O, K]())
		n.latch.unlockExclusive(0)
		t.root.Store(&n.gheader)
	}
}

// optimisticLeaves reports trees whose lookups read a leaf without
// latching it, checking its version after: exact codes settle the search
// without comparing keys, so no user code runs on a racing read. A value is
// copied racing, perhaps torn, and returned only once the check passes;
// until then nothing reads through it, and each of its words is a pointer
// or not as its type says, which is all the collector needs.
func optimisticLeaves[O keyOrder[K], K any]() bool {
	return seqlockPlainStores && exactCodes[O, K]()
}

// optimisticText reports trees whose lookups read a text leaf without
// latching it: its codes leave one candidate, whose key header and value
// are copied racing and used only once the leaf's version holds; a string's
// bytes never change, so comparing them then reads nothing racing.
func optimisticText[O keyOrder[K], K any]() bool {
	return seqlockPlainStores && prefixed[O, K]()
}

// getText is getOptimistic for text keys; done is false where codes leave
// several candidates, a prefix too long for the head, or writers kept
// changing the leaf.
func (t *genericTree[K, V, O]) getText(key K, code uint64) (value V, found, done bool) {
	for try := 0; try < 4; try++ {
		n, version := t.descend(key, code, seekKey, false, optimistic, nil)
		count := min(int(n.count.Load()), nodeSize)
		p := int(n.prefix.Load())
		if p > 16 {
			return value, false, false
		}
		// The slot that holds key, if any: settled within the leaf, or a
		// candidate whose bytes are compared once the leaf's version holds.
		var buf unsafe.Pointer
		var ref uint64
		slot, settled := -1, false
		if p == 0 || headOrder(textView(&key), p, n.head[0].Load(), n.head[1].Load()) == 0 {
			k := probeText(textView(&key), p)
			lo, hi := tieRange(&n.codes, count, k.code)
			for i := lo; i < hi; i++ {
				order, ok := slotOrder(n, i, &k, p)
				if ok && order != 0 {
					continue
				}
				if !ok && i+1 < hi {
					// Long keys that tie past their windows: a racing
					// read compares only one, so the leaf is latched.
					return value, false, false
				}
				slot, settled = i&(nodeSize-1), ok
				value = n.values[slot]
				if !ok {
					// The candidate's bytes are read once the version
					// holds: a racing buffer and ref may not match.
					buf, ref = n.text().buf, n.text().slots[slot].ref
				}
				break
			}
		}
		loadFence()
		if !n.latch.isUnchanged(version) {
			continue
		}
		if slot >= 0 && (settled || keyAt(buf, ref) == textView(&key)) {
			return value, true, true
		}
		var zero V
		return zero, false, true
	}
	return value, false, false
}

// get is key's value. With optimistic leaves it is read without latching
// the leaf, a few times while writers change it, before it latches shared.
func (t *genericTree[K, V, O]) get(key K) (value V, found bool) {
	if t.owner {
		return t.ownerGet(key)
	}
	if optimisticText[O, K]() {
		// The ordered lookup needs no code; a nil root fails it.
		if n, word, ok := t.textLeafOrdered(key); ok {
			if value, found, ok := t.readTextLeaf(n, word, key); ok {
				return value, found
			}
		}
	}
	if t.root.Load() == nil {
		return value, false
	}
	code := t.code(key)
	if optimisticText[O, K]() {
		if value, found, done := t.getText(key, code); done {
			return value, found
		}
	}
	if optimisticLeaves[O, K]() {
		for try := 0; try < 4; try++ {
			n, version := t.descend(key, code, seekKey, false, optimistic, nil)
			count := min(int(n.count.Load()), nodeSize)
			i := lowerCode(&n.codes, count, code) & (nodeSize - 1)
			found = i < count && n.codes[i] == code
			value = n.values[i]
			loadFence()
			if n.latch.isUnchanged(version) {
				if !found {
					var zero V
					return zero, false
				}
				return value, true
			}
		}
	}
	n, _ := t.leafLatched(key, code, seekKey, shared)
	if i, ok := t.find(n, key, code); ok {
		value, found = n.values[i], true
	} else {
		var zero V
		value, found = zero, false
	}
	n.latch.unlockShared()
	return value, found
}

// read runs visit on key's entry, its stored key and value, with its leaf
// latched shared.
func (t *genericTree[K, V, O]) read(key K, visit func(K, V)) bool {
	if t.owner {
		return t.ownerRead(key, visit)
	}
	if t.root.Load() == nil {
		return false
	}
	code := t.code(key)
	n, _ := t.leafLatched(key, code, seekKey, shared)
	defer n.latch.unlockShared()
	i, ok := t.find(n, key, code)
	if ok {
		visit(t.key(n, i), n.values[i])
	}
	return ok
}

// update stores change's result as key's value, with its leaf latched
// exclusively; a panicking change stores nothing.
func (t *genericTree[K, V, O]) update(key K, change func(K, V) V) (V, bool) {
	if t.owner {
		return t.ownerUpdate(key, change)
	}
	var zero V
	if t.root.Load() == nil {
		return zero, false
	}
	code := t.code(key)
	n, version := t.leafLatched(key, code, seekKey, exclusive)
	defer n.latch.unlockExclusive(version)
	i, ok := t.find(n, key, code)
	if !ok {
		return zero, false
	}
	value := change(t.key(n, i), n.values[i])
	n.values[i] = value
	return value, true
}

// isEmpty reports that the tree held no entry at some moment during the
// call: its root is a leaf without entries.
func (t *genericTree[K, V, O]) isEmpty() bool {
	for {
		h := t.root.Load()
		if h == nil {
			return true
		}
		if !h.leaf {
			return false
		}
		version := h.latch.readVersion()
		empty := h.count.Load() == 0
		if h.latch.isUnchanged(version) && t.root.Load() == h {
			return empty
		}
	}
}

// clear drops every entry. As upstream's, it is the owner's.
func (t *genericTree[K, V, O]) clear() {
	t.root.Store(nil)
	t.shared.Store(nil)
}
