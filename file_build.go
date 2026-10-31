package gobbtree

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"unsafe"
)

// The tree a FileWriter builds before it lays the tree's pages out, as
// upstream's FileWriter builds one with its TreeBuilder
// (bbtree/src/tree/build.rs). Each page is a byte string of upstream's
// layout, changed only by ports of the node operations the builder runs:
// leaves filled left to right, a new leaf started past a full one, the
// right border topped up past the minimum from its full left siblings
// (bbtree/src/tree/cut.rs), and prefixes fit to fences as nodes close
// (bbtree/src/tree/prefix.rs). The bytes a file map copies are then
// upstream's, stale ones included: a key moved out of a leaf leaves its
// bytes in its slot, as upstream's slots do, and the root page keeps what
// its node held before the tree was raised.
//
// A page is named by its height, leaves at 0, and its index there, left
// to right; the root is at the tree's height. An edge slot holds its
// child's index one height down while the tree is built, where upstream's
// holds a pointer, and the file map's offset once laid out.

// diskKeyPlain and the others are how keys get inline keys, as upstream's
// KeyInliner resolves them: u64 and i64 under Auto, by their big-endian
// bytes, an i64's sign bit flipped; byte strings, VarBytes under
// WithInlineKey, by their bytes, zero past their end; every other key has
// none, and its internal nodes keep keys.
const (
	diskKeyPlain = iota
	diskKeyUint64
	diskKeyInt64
	diskKeyBytes
)

// diskMaxPrefix is the longest prefix a node records, upstream's
// InlineKeyNodeHeader::MAX_PREFIX_LEN.
const diskMaxPrefix = math.MaxUint16

// diskHeapPage is the size of the heap pages after a leaf, upstream's
// NodeConfig::HEAP_PAGE_SIZE, the default config's leaf page size.
const diskHeapPage = 1024

// diskKey is what a key's slot holds, at most 8 bytes: an ordered key's
// own bytes, or a byte string's arena reference (diskArena).
type diskKey [8]byte

// diskWindow is upstream's Window: an inline key, bytes prefix..prefix+8
// of a key's encoding, with the prefix length it is taken at.
type diskWindow struct {
	prefix int
	key    uint64
}

// movedBackTo is the same key's window at fill's shorter prefix length, as
// upstream's Window::moved_back_to: the bytes that move in are fill's
// first, and this window's follow them.
func (w diskWindow) movedBackTo(fill diskWindow) diskWindow {
	shift := max(w.prefix-fill.prefix, 0)
	key := w.key
	if shift >= 8 {
		key = fill.key
	} else if shift > 0 {
		key = fill.key&^(math.MaxUint64>>(8*shift)) | w.key>>(8*shift)
	}
	return diskWindow{fill.prefix, key}
}

// diskSeparator is upstream's Separator on its way between nodes: with
// inline keys its window alone, the key staying a leaf head (D49); without
// them the key.
type diskSeparator struct {
	window diskWindow
	key    diskKey
}

// diskArena keeps the bytes of a writer's byte-string keys and values, as
// upstream's ByteArena does: chunks that never move, each record its
// length, then its bytes. A slot holds a record's chunk and offset where
// upstream's ArenaBytes holds its address.
type diskArena struct {
	chunks [][]byte
	used   int
}

func (a *diskArena) push(b []byte) (uint64, error) {
	if uint64(len(b)) >= math.MaxUint32 {
		return 0, fmt.Errorf("%w: byte span exceeds uint32", ErrFileFormat)
	}
	size := 4 + len(b)
	if len(a.chunks) == 0 || size > len(a.chunks[len(a.chunks)-1])-a.used {
		// Upstream's chunks are 1 MiB; these grow to it, so that a small
		// map takes a small arena.
		a.chunks = append(a.chunks, make([]byte, max(size, 4096<<min(len(a.chunks), 8))))
		a.used = 0
	}
	c := len(a.chunks) - 1
	chunk := a.chunks[c]
	binary.NativeEndian.PutUint32(chunk[a.used:], uint32(len(b)))
	copy(chunk[a.used+4:], b)
	ref := uint64(c)<<32 | uint64(a.used)
	a.used += size
	return ref, nil
}

func (a *diskArena) bytes(ref uint64) []byte {
	chunk := a.chunks[ref>>32]
	off := int(uint32(ref))
	n := int(binary.NativeEndian.Uint32(chunk[off:]))
	return chunk[off+4 : off+4+n : off+4+n]
}

// diskChunkShift is log2 of the pages in a chunk of diskPages.
const diskChunkShift = 10

// diskPages holds the pages of one height of the tree, left to right, in
// chunks of 1 << diskChunkShift pages, the first of which doubles as it
// fills, so that a small tree takes a small one.
type diskPages struct {
	size, count int
	chunks      [][]byte
}

func (p *diskPages) page(i int) []byte {
	off := (i & (1<<diskChunkShift - 1)) * p.size
	return p.chunks[i>>diskChunkShift][off : off+p.size : off+p.size]
}

// add appends a page, zeroed, as upstream's allocator gives them
// (allocate_zeroed), and returns its index. Pages taken earlier may move.
func (p *diskPages) add() int {
	i := p.count
	c, at := i>>diskChunkShift, i&(1<<diskChunkShift-1)
	if c == len(p.chunks) {
		n := 1 << diskChunkShift
		if c == 0 {
			n = 4
		}
		p.chunks = append(p.chunks, make([]byte, n*p.size))
	} else if (at+1)*p.size > len(p.chunks[c]) {
		grown := make([]byte, min(2*len(p.chunks[c]), p.size<<diskChunkShift))
		copy(grown, p.chunks[c])
		p.chunks[c] = grown
	}
	p.count++
	clear(p.page(i))
	return i
}

// chunk is the bytes of the pages of chunk c, as many as it holds.
func (p *diskPages) chunk(c int) []byte {
	return p.chunks[c][:min(max(p.count-c<<diskChunkShift, 0), 1<<diskChunkShift)*p.size]
}

// diskStack is upstream's NodeStack: the node at each depth from the root
// down, by index at its height, and the edge or key it is at.
type diskStack struct {
	depth int
	node  [fileMaxDepth + 1]int
	index [fileMaxDepth + 1]int
}

// diskTree is the tree a FileWriter builds.
type diskTree struct {
	l                *diskLayout
	enc              int
	varKey, varValue bool
	// Whether nodes keep inline keys, and leaves hash tags (D18, D36).
	inline, tags bool
	// The inline widths of leaves and internal nodes.
	lw, iw int
	// The root's height, 0 while it is a leaf; levels[h] holds the pages
	// at each height below it.
	height int
	root   []byte
	levels []diskPages
	// The root area: what every key of the tree shares (D50), upstream's
	// SharedPrefix, its length and then its bytes. Bytes past the length
	// keep what they held, as upstream's shrink_to keeps them.
	shared [1 + 64]byte
	arena  diskArena
	length int
	stack  diskStack
}

func newDiskTree(l *diskLayout) *diskTree {
	t := &diskTree{l: l, varKey: l.key.variable, varValue: l.value.variable,
		lw: l.leaf.width, iw: l.internal.width, tags: l.leaf.tags >= 0}
	t.inline = t.lw > 0
	switch {
	case t.varKey:
		t.enc = diskKeyBytes
	case t.inline && l.key.kind == reflect.Int64:
		t.enc = diskKeyInt64
	case t.inline:
		t.enc = diskKeyUint64
	}
	return t
}

// The node headers, upstream's NodeHeader and InlineKeyNodeHeader: the
// number of keys, a u16; whether a leaf has a head, a byte; with inline
// keys, the prefix length, a u16 at 4.

func nodeLen(p []byte) int { return int(binary.NativeEndian.Uint16(p)) }

func (t *diskTree) prefixOf(p []byte) int {
	if !t.inline {
		return 0
	}
	return int(binary.NativeEndian.Uint16(p[4:]))
}

func (t *diskTree) setPrefix(p []byte, n int) {
	if t.inline {
		binary.NativeEndian.PutUint16(p[4:], uint16(n))
	}
}

// inlineRead is a slot's inline key, the bytes it keeps of a window and
// zeros after them, as upstream's InlineSlot::inline_key.
func inlineRead(p []byte, off, width int) uint64 {
	switch width {
	case 2:
		return uint64(binary.NativeEndian.Uint16(p[off:])) << 48
	case 4:
		return uint64(binary.NativeEndian.Uint32(p[off:])) << 32
	}
	return binary.NativeEndian.Uint64(p[off:])
}

// inlineWrite keeps a window's first width bytes in a slot, as upstream's
// InlineSlot::of_inline_key.
func inlineWrite(p []byte, off, width int, key uint64) {
	switch width {
	case 2:
		binary.NativeEndian.PutUint16(p[off:], uint16(key>>48))
	case 4:
		binary.NativeEndian.PutUint32(p[off:], uint32(key>>32))
	default:
		binary.NativeEndian.PutUint64(p[off:], key)
	}
}

func (t *diskTree) page(h, i int) []byte {
	if h == t.height {
		return t.root
	}
	return t.levels[h].page(i)
}

// last is the index of the rightmost page at height h.
func (t *diskTree) last(h int) int {
	if h == t.height {
		return 0
	}
	return t.levels[h].count - 1
}

func (t *diskTree) layoutAt(h int) (*diskNodeLayout, int) {
	if h == 0 {
		return &t.l.leaf, t.lw
	}
	return &t.l.internal, t.iw
}

func (t *diskTree) child(h, i, edge int) int {
	return int(binary.NativeEndian.Uint64(t.page(h, i)[t.l.internal.trailing+edge*8:]))
}

func (t *diskTree) setEdge(p []byte, edge, child int) {
	binary.NativeEndian.PutUint64(p[t.l.internal.trailing+edge*8:], uint64(child))
}

// setLen sets a node's length, giving the inline-key slots a shorter one
// leaves the greatest inline key, as upstream's set_len_refilling does;
// their hash tags stay stale (mark_vacant).
func (t *diskTree) setLen(p []byte, h, n int) {
	if nl, w := t.layoutAt(h); w > 0 {
		for j := n; j < nodeLen(p); j++ {
			inlineWrite(p, nl.inline+j*w, w, math.MaxUint64)
		}
	}
	binary.NativeEndian.PutUint16(p, uint16(n))
}

// initialize makes p an empty node of height h's kind, as upstream's
// NodePtr::initialize does, whatever p held: its header, no head, and
// every inline-key slot the greatest inline key; a leaf's hash tags 0
// (KeyColumns::initialize). The rest of p keeps what it held.
func (t *diskTree) initialize(p []byte, h int) {
	clear(p[:4])
	t.setPrefix(p, 0)
	nl, w := t.layoutAt(h)
	for j := range nl.capacity * w {
		p[nl.inline+j] = 0xff
	}
	if h == 0 && t.tags {
		clear(p[nl.tags : nl.tags+nl.capacity])
	}
}

// Keys and their encodings, as upstream's KeyInliner reads them.

func (t *diskTree) word(k *diskKey) uint64 {
	v := binary.NativeEndian.Uint64(k[:])
	if t.enc == diskKeyInt64 {
		v ^= 1 << 63
	}
	return v
}

func (t *diskTree) keyBytes(k *diskKey) []byte {
	return t.arena.bytes(binary.NativeEndian.Uint64(k[:]))
}

// bytesInlineKeyAt is a byte string's inline key at offset: its bytes
// offset..offset+8, big-endian, zero past its end.
func bytesInlineKeyAt(b []byte, offset int) uint64 {
	if offset >= 0 && offset+8 <= len(b) {
		return binary.BigEndian.Uint64(b[offset:])
	}
	var w uint64
	for i := range 8 {
		w <<= 8
		if offset+i < len(b) {
			w |= uint64(b[offset+i])
		}
	}
	return w
}

// inlineKeyAt is k's inline key at offset, upstream's inline_key_at.
func (t *diskTree) inlineKeyAt(k *diskKey, offset int) uint64 {
	if t.enc == diskKeyBytes {
		return bytesInlineKeyAt(t.keyBytes(k), offset)
	}
	if offset >= 8 {
		return 0
	}
	return t.word(k) << (8 * offset)
}

// window is k's window at prefix, for a node with that prefix length;
// none without inline keys.
func (t *diskTree) window(k *diskKey, prefix int) diskWindow {
	if !t.inline {
		return diskWindow{}
	}
	return diskWindow{prefix, t.inlineKeyAt(k, prefix)}
}

// hashTag is a byte string's length saturated at 255, its hash tag (D36).
func (t *diskTree) hashTag(k *diskKey) byte {
	return byte(min(len(t.keyBytes(k)), 255))
}

// commonPrefixLen is how many leading bytes the zero-padded encodings of a
// and b share, at most diskMaxPrefix, as upstream's common_prefix_len for
// keys with an InlineKey impl, which a writer's byte strings (ArenaBytes)
// and integers both are: "ab" and "ab\x00" share all of it.
func (t *diskTree) commonPrefixLen(a, b *diskKey) int {
	if t.enc != diskKeyBytes {
		x := t.word(a) ^ t.word(b)
		if x == 0 {
			return diskMaxPrefix
		}
		return bits.LeadingZeros64(x) / 8
	}
	x, y := t.keyBytes(a), t.keyBytes(b)
	n := min(len(x), len(y))
	i := 0
	for i < n && x[i] == y[i] {
		i++
	}
	if i == n {
		rest := x[n:]
		if len(y) > n {
			rest = y[n:]
		}
		j := 0
		for j < len(rest) && rest[j] == 0 {
			j++
		}
		if j == len(rest) {
			return diskMaxPrefix
		}
		i = n + j
	}
	return min(i, diskMaxPrefix)
}

// The shared prefix (D50, bbtree/src/tree/shared_prefix.rs).

// startSharedPrefix makes k's first bytes what every key shares, as
// upstream's SharedPrefix::of_key: an integer's 8, a byte string's first
// 64, zero-padded, as for any key with its own InlineKey impl.
func (t *diskTree) startSharedPrefix(k *diskKey) {
	if t.enc == diskKeyBytes {
		copy(t.shared[1:], t.keyBytes(k))
		t.shared[0] = 64
		return
	}
	binary.BigEndian.PutUint64(t.shared[1:], t.word(k))
	t.shared[0] = 8
}

// lenSharedWith is how many of the shared prefix's bytes k's zero-padded
// encoding shares, as upstream's len_shared_with.
func (t *diskTree) lenSharedWith(k *diskKey) int {
	n := int(t.shared[0])
	if n == 0 {
		return 0
	}
	if t.enc != diskKeyBytes {
		shared := binary.BigEndian.Uint64(t.shared[1:]) &^ (math.MaxUint64 >> (8 * n))
		return min(bits.LeadingZeros64(t.word(k)^shared)/8, n)
	}
	b, s := t.keyBytes(k), t.shared[1:1+n]
	i := 0
	for i < n && (i < len(b) && b[i] == s[i] || i >= len(b) && s[i] == 0) {
		i++
	}
	return i
}

// shrinkBordersTo shrinks what every key shares to its first n bytes, and
// the prefixes along both borders with it, filled from the shared bytes,
// as upstream's shrink_borders_to does.
func (t *diskTree) shrinkBordersTo(n int) {
	fill := diskWindow{n, bytesInlineKeyAt(t.shared[1:1+int(t.shared[0])], n)}
	t.shared[0] = byte(n)
	for _, last := range []bool{false, true} {
		h, i := t.height, 0
		for {
			t.movePrefixBackTo(h, i, fill)
			if h == 0 {
				break
			}
			edge := 0
			if last {
				edge = nodeLen(t.page(h, i))
			}
			h, i = h-1, t.child(h, i, edge)
		}
	}
}

// movePrefixBackTo moves a node's prefix back to fill's, every inline key
// with it, reading no key, as upstream's Child::move_prefix_back_to.
func (t *diskTree) movePrefixBackTo(h, i int, fill diskWindow) {
	p := t.page(h, i)
	prefix := t.prefixOf(p)
	if prefix <= fill.prefix {
		return
	}
	nl, w := t.layoutAt(h)
	for j := range nodeLen(p) {
		moved := diskWindow{prefix, inlineRead(p, nl.inline+j*w, w)}.movedBackTo(fill)
		inlineWrite(p, nl.inline+j*w, w, moved.key)
	}
	t.setPrefix(p, fill.prefix)
}

// Separators, as upstream's Internal reads them.

// separatorKey is separator j's key: with inline keys the head key of the
// leftmost leaf right of it (D49), without them the node's own.
func (t *diskTree) separatorKey(h, i, j int) (k diskKey) {
	if !t.inline {
		il := &t.l.internal
		copy(k[:t.l.key.size], t.page(h, i)[il.keys+j*il.keyStride:])
		return k
	}
	c := t.child(h, i, j+1)
	for hh := h - 1; hh > 0; hh-- {
		c = t.child(hh, c, 0)
	}
	copy(k[:], t.page(0, c)[t.l.leaf.headKey:])
	return k
}

// internalWindow is separator j's window, upstream's Internal::window: read
// from the key again where internal nodes keep fewer bytes of it than
// leaves do, as for byte strings (D43).
func (t *diskTree) internalWindow(h, i, j int) diskWindow {
	if !t.inline {
		return diskWindow{}
	}
	p := t.page(h, i)
	prefix := t.prefixOf(p)
	if t.iw < t.lw {
		k := t.separatorKey(h, i, j)
		return diskWindow{prefix, t.inlineKeyAt(&k, prefix)}
	}
	return diskWindow{prefix, inlineRead(p, t.l.internal.inline+j*t.iw, t.iw)}
}

// readSeparator and writeSeparator are upstream's SeparatorSlots read and
// write: a window from its slot, or a key.
func (t *diskTree) readSeparator(p []byte, j int) (s diskSeparator) {
	il := &t.l.internal
	if t.inline {
		s.window = diskWindow{t.prefixOf(p), inlineRead(p, il.inline+j*t.iw, t.iw)}
	} else {
		copy(s.key[:t.l.key.size], p[il.keys+j*il.keyStride:])
	}
	return s
}

func (t *diskTree) writeSeparator(p []byte, j int, s diskSeparator) {
	il := &t.l.internal
	if t.inline {
		inlineWrite(p, il.inline+j*t.iw, t.iw, s.window.key)
	} else {
		copy(p[il.keys+j*il.keyStride:][:t.l.key.size], s.key[:t.l.key.size])
	}
}

// shiftSlots moves slots from..n of size bytes each, stride apart, up by
// one, from the top, as upstream's Slots::insert does before it writes the
// slot it frees.
func shiftSlots(p []byte, off, stride, size, from, n int) {
	for j := n - 1; j >= from; j-- {
		copy(p[off+(j+1)*stride:][:size], p[off+j*stride:][:size])
	}
}

// insertSeparator inserts s at edge j of the internal node p, with child
// right of it, as upstream's insert_fit_unchecked: s moves back to the
// node's prefix length, filled from the separator beside the edge.
func (t *diskTree) insertSeparator(h, i, j int, s diskSeparator, child int) {
	p := t.page(h, i)
	n := nodeLen(p)
	if t.inline && s.window.prefix > t.prefixOf(p) && n > 0 {
		beside := j - 1
		if j == 0 {
			beside = j
		}
		s.window = s.window.movedBackTo(t.internalWindow(h, i, beside))
	}
	il := &t.l.internal
	if t.inline {
		shiftSlots(p, il.inline, t.iw, t.iw, j, n)
	} else {
		shiftSlots(p, il.keys, il.keyStride, t.l.key.size, j, n)
	}
	t.writeSeparator(p, j, s)
	shiftSlots(p, il.trailing, 8, 8, j+1, n+1)
	t.setEdge(p, j+1, child)
	binary.NativeEndian.PutUint16(p, uint16(n+1))
}

// Leaves.

func (t *diskTree) leafKey(p []byte, j int) (k diskKey) {
	nl := &t.l.leaf
	copy(k[:t.l.key.size], p[nl.keys+j*nl.keyStride:])
	return k
}

// writeLeafKey writes key j, its inline key and hash tag: upstream's
// KeySlots::write.
func (t *diskTree) writeLeafKey(p []byte, j int, k *diskKey, w diskWindow) {
	nl := &t.l.leaf
	if ks := t.l.key.size; ks == 8 {
		binary.NativeEndian.PutUint64(p[nl.keys+j*nl.keyStride:], binary.NativeEndian.Uint64(k[:]))
	} else {
		copy(p[nl.keys+j*nl.keyStride:][:ks], k[:ks])
	}
	if t.inline {
		inlineWrite(p, nl.inline+j*t.lw, t.lw, w.key)
	}
	if t.tags {
		p[nl.tags+j] = t.hashTag(k)
	}
}

// leafAppend appends a key and its value to a leaf with room, upstream's
// Leaf::insert_fit at its end.
func (t *diskTree) leafAppend(p []byte, k *diskKey, w diskWindow, v unsafe.Pointer) {
	nl := &t.l.leaf
	n := nodeLen(p)
	t.writeLeafKey(p, n, k, w)
	switch vs := t.l.value.size; vs {
	case 0:
	case 8:
		binary.NativeEndian.PutUint64(p[nl.trailing+(n+1)*nl.stride:], binary.NativeEndian.Uint64(unsafe.Slice((*byte)(v), 8)))
	default:
		copy(p[nl.trailing+(n+1)*nl.stride:][:vs], unsafe.Slice((*byte)(v), vs))
	}
	binary.NativeEndian.PutUint16(p, uint16(n+1))
}

// Building (bbtree/src/tree/build.rs).

// push appends an entry whose key is greater than every key so far, as
// upstream's TreeBuilder::extend does for each: into the rightmost leaf
// while it has room, or else into a new leaf, whose start closes the
// nodes it leaves behind. v points to the value's slot bytes.
func (t *diskTree) push(k *diskKey, v unsafe.Pointer) {
	if t.root == nil {
		t.root = make([]byte, t.l.rootSize)
		t.initialize(t.root, 0)
		if t.inline {
			t.startSharedPrefix(k)
			t.setPrefix(t.root, int(t.shared[0]))
		}
	} else if t.inline && t.shared[0] != 0 {
		if n := t.lenSharedWith(k); n < int(t.shared[0]) {
			t.shrinkBordersTo(n)
		}
	}
	t.length++
	leaf := t.root
	if t.height > 0 {
		leaf = t.levels[0].page(t.levels[0].count - 1)
	}
	if nodeLen(leaf) < t.l.leaf.capacity {
		t.leafAppend(leaf, k, t.window(k, int(t.shared[0])), v)
		return
	}
	t.startLeaf(k, v)
}

// startLeaf starts a new rightmost leaf past a full one, its head k's
// value, and with inline keys k (D17, D49), as upstream's start_leaf: the
// separator goes into the lowest node up the right border with room, past
// full ones that each start a new node beside them, or into a new root.
// Then the nodes the new leaf closed fit their prefixes to their fences,
// from the leaf up, as extend does.
func (t *diskTree) startLeaf(k *diskKey, v unsafe.Pointer) {
	shared := int(t.shared[0])
	top := 1
	for top <= t.height && nodeLen(t.page(top, t.last(top))) == t.l.internal.capacity {
		top++
	}
	// Upstream starts the leaf and the nodes above it before it raises the
	// tree; raising first puts the old root left of them, where upstream's
	// breadth-first layout finds it, and no page's bytes depend on the order.
	if top > t.height {
		t.raise()
	}
	li := t.levels[0].add()
	leaf := t.levels[0].page(li)
	t.initialize(leaf, 0)
	t.setPrefix(leaf, shared)
	nl := &t.l.leaf
	sep := diskSeparator{window: t.window(k, shared)}
	if t.inline {
		copy(leaf[nl.headKey:][:t.l.key.size], k[:t.l.key.size])
	} else {
		sep.key = *k
	}
	if vs := t.l.value.size; vs > 0 {
		copy(leaf[nl.trailing:][:vs], unsafe.Slice((*byte)(v), vs))
	}
	leaf[2] = 1
	child := li
	for h := 1; h < top; h++ {
		i := t.levels[h].add()
		p := t.levels[h].page(i)
		t.initialize(p, h)
		t.setEdge(p, 0, child)
		t.setPrefix(p, shared)
		child = i
	}
	n := t.last(top)
	edge := nodeLen(t.page(top, n))
	t.insertSeparator(top, n, edge, sep, child)
	if !t.inline {
		return
	}
	// The node stack start_leaf returns: down the right border to the node
	// that took the separator, at the edge left of it.
	s := &t.stack
	s.depth = t.height - top
	for d := 0; d <= s.depth; d++ {
		h := t.height - d
		s.node[d] = t.last(h)
		s.index[d] = nodeLen(t.page(h, s.node[d]))
	}
	s.index[s.depth] = edge
	t.refitBorderBelow(s, edge)
	t.refitChildToFences(s, edge)
}

// raise makes the tree a level taller, as upstream's RootPage::raise: the
// root node moves to a new page of its kind, the root's only child, and
// the root page becomes an empty internal node, keeping what else it
// held.
func (t *diskTree) raise() {
	h := t.height
	nl, _ := t.layoutAt(h)
	t.levels = append(t.levels, diskPages{size: nl.size})
	i := t.levels[h].add()
	copy(t.levels[h].page(i), t.root[:nl.size])
	if h == 0 && (t.varKey || t.varValue) {
		// Upstream's root page keeps the leaf's slots, which hold the
		// writer's pointers to its bytes (ArenaBytes); these hold zeros.
		t.zeroVarSlots(t.root)
	}
	t.height++
	t.initialize(t.root, t.height)
	t.setEdge(t.root, 0, i)
	t.setPrefix(t.root, int(t.shared[0]))
}

// zeroVarSlots zeros every slot of the leaf p whose type points beyond it,
// as upstream's write_leaf_bytes does before it writes their spans.
func (t *diskTree) zeroVarSlots(p []byte) {
	nl := &t.l.leaf
	for j := 0; j <= nl.capacity; j++ {
		if t.varKey && j < nl.capacity {
			clear(p[nl.keys+j*nl.keyStride:][:8])
		}
		if t.varValue {
			clear(p[nl.trailing+j*nl.stride:][:8])
		}
	}
	if t.varKey && nl.headKey >= 0 {
		clear(p[nl.headKey:][:8])
	}
}

// Fitting prefixes (bbtree/src/tree/prefix.rs).

// fences are the bottom node's fences, the nearest separators left and
// right of the node stack above it, as upstream's NodeStackMut::fences.
func (t *diskTree) fences(s *diskStack) (low, high diskKey, hasLow, hasHigh bool) {
	for d := s.depth - 1; d >= 0; d-- {
		h, i, j := t.height-d, s.node[d], s.index[d]
		if !hasLow && j > 0 {
			low, hasLow = t.separatorKey(h, i, j-1), true
		}
		if !hasHigh && j < nodeLen(t.page(h, i)) {
			high, hasHigh = t.separatorKey(h, i, j), true
		}
		if hasLow && hasHigh {
			break
		}
	}
	return
}

// refitBorderBelow fits the prefixes of the nodes below the bottom node's
// child at edge, down that child's right border, from the leaf up, as
// upstream's refit_border_below; the child itself is the caller's.
func (t *diskTree) refitBorderBelow(s *diskStack, edge int) {
	start := s.depth
	s.index[s.depth] = edge
	for h := t.height - s.depth; h > 0; h-- {
		c := t.child(h, s.node[s.depth], s.index[s.depth])
		s.depth++
		s.node[s.depth] = c
		s.index[s.depth] = nodeLen(t.page(h-1, c))
	}
	s.depth--
	for s.depth > start {
		t.refitChildToFences(s, s.index[s.depth])
		s.depth--
	}
}

// refitChildToFences fits the prefix of the bottom node's child at edge to
// what its fences share, as upstream's refit_children_to_fences: unless
// the child's own inline keys show they share no more.
func (t *diskTree) refitChildToFences(s *diskStack, edge int) {
	h, i := t.height-s.depth, s.node[s.depth]
	c := t.child(h, i, edge)
	if t.keysDifferPastPrefix(h-1, c) {
		return
	}
	p := t.page(h, i)
	n := nodeLen(p)
	var low, high diskKey
	var hasLow, hasHigh bool
	if edge == 0 || edge == n {
		low, high, hasLow, hasHigh = t.fences(s)
	}
	if edge > 0 {
		low, hasLow = t.separatorKey(h, i, edge-1), true
	}
	if edge < n {
		high, hasHigh = t.separatorKey(h, i, edge), true
	}
	prefix := int(t.shared[0])
	if hasLow && hasHigh {
		prefix = t.commonPrefixLen(&low, &high)
	}
	t.refitPrefix(h-1, c, max(prefix, t.prefixOf(p)))
}

// keysDifferPastPrefix is whether a node's first and last inline keys
// differ in their first byte, upstream's Child::keys_differ_past_prefix.
func (t *diskTree) keysDifferPastPrefix(h, i int) bool {
	p := t.page(h, i)
	n := nodeLen(p)
	nl, w := t.layoutAt(h)
	return n > 0 && (inlineRead(p, nl.inline, w)^inlineRead(p, nl.inline+(n-1)*w, w))>>56 != 0
}

// refitPrefix sets a node's prefix length to prefix, as far as its kind
// lets it grow, and every inline key to its key's window there, read from
// the key, as upstream's Child::refit_prefix: an internal node's prefix
// grows no further than its children's (allowed_prefix_len).
func (t *diskTree) refitPrefix(h, i, prefix int) {
	p := t.page(h, i)
	n := nodeLen(p)
	if h > 0 && prefix > t.prefixOf(p) {
		for e := 0; e <= n; e++ {
			prefix = min(prefix, t.prefixOf(t.page(h-1, t.child(h, i, e))))
		}
	}
	if prefix == t.prefixOf(p) {
		return
	}
	nl, w := t.layoutAt(h)
	for j := range n {
		var k diskKey
		if h == 0 {
			k = t.leafKey(p, j)
		} else {
			k = t.separatorKey(h, i, j)
		}
		inlineWrite(p, nl.inline+j*w, w, t.inlineKeyAt(&k, prefix))
	}
	t.setPrefix(p, prefix)
}

// refitBorderSiblings fits the prefix of the node left of each node along
// the right border to its fences, from the root down, as upstream's
// Tree::refit_border_siblings after fix_border.
func (t *diskTree) refitBorderSiblings() {
	if !t.inline {
		return
	}
	s := &t.stack
	s.depth, s.node[0], s.index[0] = 0, 0, 0
	for h := t.height; h > 0; h-- {
		n := nodeLen(t.page(h, s.node[s.depth]))
		t.refitChildToFences(s, n-1)
		s.index[s.depth] = n
		c := t.child(h, s.node[s.depth], n)
		s.depth++
		s.node[s.depth], s.index[s.depth] = c, 0
	}
}

// Topping up the right border (bbtree/src/tree/cut.rs).

// finish tops up the right border past the minimum from the full left
// siblings, then fits those siblings' prefixes to their new fences, as
// upstream's TreeBuilder::finish.
func (t *diskTree) finish() error {
	if t.root == nil {
		return nil
	}
	t.fixBorder()
	t.refitBorderSiblings()
	if t.height > fileMaxDepth {
		return fmt.Errorf("%w: tree depth", ErrFileFormat)
	}
	return nil
}

// fixBorder brings every node along the right border to one key more than
// its minimum (D22), from the root down, as upstream's
// Tree::fix_border::<LastEdge>.
func (t *diskTree) fixBorder() {
	t.lowerWhileRootEmpty()
	for h, i := t.height, 0; h > 0; h-- {
		n := nodeLen(t.page(h, i))
		if n == 0 {
			panic("gobbtree: a node on the border holds a key once stocked")
		}
		i = t.stockBorderChild(h, i, n-1)
	}
	t.lowerWhileRootEmpty()
}

// minLen is the fewest keys a node of height h below the root holds
// between removals (D22), upstream's Child::min_len.
func (t *diskTree) minLen(h int) int {
	nl, _ := t.layoutAt(h)
	return max(nl.capacity/8, 4)
}

// stockBorderChild brings the right child of separator j of node (h, i) to
// one key more than its minimum, as upstream's
// Siblings::stock_border_child: it merges the two children if they fit in
// one node with their separator, or else moves keys over from the left
// one. Returns the right child, merged or not. Built trees never merge:
// every node left of the border is full.
func (t *diskTree) stockBorderChild(h, i, j int) int {
	left, right := t.child(h, i, j), t.child(h, i, j+1)
	nl, _ := t.layoutAt(h - 1)
	target := t.minLen(h-1) + 1
	if nodeLen(t.page(h-1, right)) >= target {
		return right
	}
	if nodeLen(t.page(h-1, left))+1+nodeLen(t.page(h-1, right)) <= nl.capacity {
		t.merge(h, i, j)
		return left
	}
	for nodeLen(t.page(h-1, right)) < target {
		t.stealLeft(h, i, j)
	}
	return right
}

// stealLeft rotates one entry rightwards through separator j of node (h,
// i), as upstream's Siblings::steal_left: the left child's last key goes
// up, the separator comes down to the right child's front. Between leaves
// the value rotates through the right leaf's head (D17), and with inline
// keys the key too, only windows passing through the parent (D49).
// Between internal nodes the left one's last edge becomes the right one's
// first. What the left child gives up stays in its slots.
func (t *diskTree) stealLeft(h, i, j int) {
	fill := t.internalWindow(h, i, j)
	li, ri := t.child(h, i, j), t.child(h, i, j+1)
	t.movePrefixBackTo(h-1, ri, fill)
	parent, left, right := t.page(h, i), t.page(h-1, li), t.page(h-1, ri)
	ln, rn := nodeLen(left), nodeLen(right)
	if h > 1 {
		il := &t.l.internal
		s := t.readSeparator(left, ln-1)
		s.window = s.window.movedBackTo(fill)
		old := t.readSeparator(parent, j)
		t.writeSeparator(parent, j, s)
		if t.inline {
			shiftSlots(right, il.inline, t.iw, t.iw, 0, rn)
		} else {
			shiftSlots(right, il.keys, il.keyStride, t.l.key.size, 0, rn)
		}
		t.writeSeparator(right, 0, old)
		edge := t.child(h-1, li, ln)
		shiftSlots(right, il.trailing, 8, 8, 0, rn+1)
		t.setEdge(right, 0, edge)
		t.setLen(left, h-1, ln-1)
		t.setLen(right, h-1, rn+1)
		return
	}
	nl, ks, vs := &t.l.leaf, t.l.key.size, t.l.value.size
	key := t.leafKey(left, ln-1)
	window := diskWindow{t.prefixOf(left), 0}
	if t.inline {
		window.key = inlineRead(left, nl.inline+(ln-1)*t.lw, t.lw)
	}
	var value [256]byte
	moved := value[:0]
	if vs <= len(value) {
		moved = value[:vs]
	} else {
		moved = make([]byte, vs)
	}
	copy(moved, left[nl.trailing+ln*nl.stride:][:vs])
	shiftSlots(right, nl.trailing, nl.stride, vs, 0, rn+1)
	copy(right[nl.trailing:][:vs], moved)
	// replace_separator_key: with inline keys the key the parent gave up
	// comes from the head, which the left leaf's last key takes.
	var down diskKey
	downWindow := fill
	if t.inline {
		copy(down[:ks], right[nl.headKey:])
		copy(right[nl.headKey:][:ks], key[:ks])
		t.writeSeparator(parent, j, diskSeparator{window: window.movedBackTo(fill)})
	} else {
		down = t.readSeparator(parent, j).key
		t.writeSeparator(parent, j, diskSeparator{key: key})
		downWindow = diskWindow{}
	}
	shiftSlots(right, nl.keys, nl.keyStride, ks, 0, rn)
	if t.inline {
		shiftSlots(right, nl.inline, t.lw, t.lw, 0, rn)
	}
	if t.tags {
		shiftSlots(right, nl.tags, 1, 1, 0, rn)
	}
	t.writeLeafKey(right, 0, &down, downWindow)
	t.setLen(left, 0, ln-1)
	t.setLen(right, 0, rn+1)
}

// merge moves separator j of node (h, i) and all of its right child into
// the left child, as upstream's Siblings::merge: both children's prefixes
// move back to the parent's, filled from the separator's window, and the
// right child's page goes, the last at its height.
func (t *diskTree) merge(h, i, j int) {
	fill := t.internalWindow(h, i, j)
	li, ri := t.child(h, i, j), t.child(h, i, j+1)
	// remove_with_right_child: the separator and edge are the parent's
	// last, so nothing shifts down over them.
	parent := t.page(h, i)
	s := t.readSeparator(parent, j)
	t.setLen(parent, h, nodeLen(parent)-1)
	t.movePrefixBackTo(h-1, li, fill)
	t.movePrefixBackTo(h-1, ri, fill)
	left, right := t.page(h-1, li), t.page(h-1, ri)
	ln, rn := nodeLen(left), nodeLen(right)
	nl, w := t.layoutAt(h - 1)
	if h > 1 {
		t.writeSeparator(left, ln, s)
		if t.inline {
			copy(left[nl.inline+(ln+1)*w:][:rn*w], right[nl.inline:][:rn*w])
		} else {
			for x := range rn {
				copy(left[nl.keys+(ln+1+x)*nl.keyStride:][:t.l.key.size], right[nl.keys+x*nl.keyStride:])
			}
		}
		for x := 0; x <= rn; x++ {
			t.setEdge(left, ln+1+x, t.child(h-1, ri, x))
		}
	} else {
		ks, vs := t.l.key.size, t.l.value.size
		k, kw := s.key, diskWindow{}
		if t.inline {
			copy(k[:ks], right[nl.headKey:])
			kw = fill
		}
		t.writeLeafKey(left, ln, &k, kw)
		for x := range rn {
			copy(left[nl.keys+(ln+1+x)*nl.keyStride:][:ks], right[nl.keys+x*nl.keyStride:])
			if t.inline {
				copy(left[nl.inline+(ln+1+x)*w:][:w], right[nl.inline+x*w:])
			}
			if t.tags {
				left[nl.tags+ln+1+x] = right[nl.tags+x]
			}
		}
		for x := 0; x <= rn; x++ {
			copy(left[nl.trailing+(ln+1+x)*nl.stride:][:vs], right[nl.trailing+x*nl.stride:])
		}
	}
	t.setLen(left, h-1, ln+1+rn)
	t.levels[h-1].count--
}

// lowerWhileRootEmpty lowers the tree while its root is internal without
// keys, as upstream's lower_while_root_empty: the root's only child moves
// up into the root page, which keeps what else it held.
func (t *diskTree) lowerWhileRootEmpty() {
	for t.height > 0 && nodeLen(t.root) == 0 {
		h := t.height - 1
		copy(t.root, t.levels[h].page(t.child(t.height, 0, 0)))
		t.levels = t.levels[:h]
		t.height = h
	}
}
