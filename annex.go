package gobbtree

import (
	"reflect"
	"sync/atomic"
	"unsafe"
)

// LeafAnnex is data an AnnexedMapOf keeps in each of its leaves, after the
// leaf, for the code that owns where its keys and values point, such as
// the store's chain of heap pages (store): it reads and changes it while an
// insert or a removal holds the leaf (AnnexedLeaf), as upstream's
// LeafAnnex. The map keeps it with its leaf:
//
//   - a new leaf's annex is A's zero value;
//   - a split gives the new leaf, right of the one that splits, a zero
//     annex, then calls Split on the one that splits;
//   - a merge calls Merge on the leaf that stays with the one that goes;
//   - Clear calls Release on each.
//
// Entries also move between neighbouring leaves without a call, as an
// insert or a removal rebalances them.
type LeafAnnex[A any] interface {
	*A
	// Split is called on a leaf's annex as the leaf splits into itself and
	// right's leaf, which took the entries right of the split.
	Split(right *A)
	// Merge is called on a leaf's annex as right's leaf merges into it and
	// goes; right is zeroed after.
	Merge(right *A)
	// Release is called on a leaf's annex as the leaf goes with the map's
	// entries (Clear); the annex is zeroed after.
	Release()
}

// AnnexedMapOf is MapOf for a map whose leaves keep an annex A
// (LeafAnnex), upstream's BBTreeMap with WithLeafAnnex. Its
// UpdateOrInsertWithLeaf and DeleteWithLeaf hand their callbacks the leaf
// they hold. The zero value is ready to use.
//
// Its keys' Compare and Window may read what changes only while their leaf
// is held exclusively, as a store's keys read the records they point to,
// which a callback moves: the map compares a key only holding its leaf, or
// a copy of it the callback left equal (ReplaceKey). So its iterations, as
// upstream's scan, run their loop bodies holding each leaf shared, which
// must not change the map.
type AnnexedMapOf[K Key[K], V any, A any, PA LeafAnnex[A]] struct {
	mapCore[K, V, annexOrder[K, A, PA]]
}

func NewAnnexedMapOf[K Key[K], V any, A any, PA LeafAnnex[A]]() *AnnexedMapOf[K, V, A, PA] {
	return new(AnnexedMapOf[K, V, A, PA])
}

// AnnexedLeaf is a leaf an operation of an AnnexedMapOf holds exclusively,
// for its callback, as upstream's AnnexedLeaf: its annex and its keys. It
// is valid only during the callback.
type AnnexedLeaf[K any, A any] struct {
	annex *A
	// keys are the leaf's, in place where the leaf keeps its keys as Ks,
	// and head its copy of the first, where it keeps one; a text leaf's
	// keys are read into keys, and head is nil.
	keys []K
	head *atomic.Pointer[headCopy[K]]
}

// Annex is the leaf's annex.
func (l AnnexedLeaf[K, A]) Annex() *A { return l.annex }

// Keys are the leaf's keys, in order, not to be changed but by ReplaceKey.
func (l AnnexedLeaf[K, A]) Keys() []K { return l.keys }

// ReplaceKey replaces the leaf's key i with key, which must compare equal
// to it: a key that refers to where its bytes lie, as the store's do, is
// replaced as they move. Where the leaf keeps a copy of its first key, for
// lookups past it, the copy is replaced too. Text keys are their bytes,
// which the leaf keeps: an equal key is the same one, and nothing changes.
func (l AnnexedLeaf[K, A]) ReplaceKey(i int, key K) {
	l.keys[i] = key
	// The leaf's head is a copy of its first key, which lookups past its
	// separator compare racing: they read the old copy or the new, equal
	// either way, and whose windows, the key's bytes, stay.
	if i == 0 && l.head != nil {
		if c := l.head.Load(); c != nil {
			replaced := *c
			replaced.key = key
			l.head.Store(&replaced)
		}
	}
}

// UpdateOrInsertWithLeaf runs occupied on the entry whose key equals key,
// its index among the leaf's Keys and its value to change, the index being
// what ReplaceKey takes, as upstream's occupied repoints the stored key
// through its &K; or, if the map holds none, vacant on key and value,
// which go in right after it; each with the leaf the entry is in, or goes
// in, held exclusively. found tells which ran. Either may refuse, for want
// of what it would hold, by returning false: the map is then unchanged, and
// ok is false. vacant runs before a leaf that is full splits, on that leaf.
// Callbacks must not use the map.
//
// vacant returns the key that goes in: key, or one equal to it that refers
// to what vacant wrote for it, as upstream's vacant repoints the new key
// through its &K (bbtree-store's new.replace(record)): a store's key points
// to its record, which only vacant, holding the leaf, can write. It returns
// the key rather than taking a *K, which would put every new key on the
// heap.
func (m *AnnexedMapOf[K, V, A, PA]) UpdateOrInsertWithLeaf(key K, value V, occupied func(leaf AnnexedLeaf[K, A], i int, key K, value *V) bool, vacant func(leaf AnnexedLeaf[K, A], key K, value V) (K, bool)) (found, ok bool) {
	t := &m.generic
	return t.insertWithLeaf(key, value,
		func(n *gleaf[K, V], i int) bool {
			return occupied(annexedLeafOf[A](t, n), i, t.key(n, i), &n.values[i])
		},
		func(n *gleaf[K, V], key K) (K, bool) { return vacant(annexedLeafOf[A](t, n), key, value) })
}

// DeleteWithLeaf removes the entry whose key equals key and returns it, the
// key as the map stored it, false without one. f runs once on it, with the
// leaf it is in, held exclusively, right before it comes out. f must not use
// the map.
func (m *AnnexedMapOf[K, V, A, PA]) DeleteWithLeaf(key K, f func(leaf AnnexedLeaf[K, A], key K, value V)) (K, V, bool) {
	t := &m.generic
	return t.removeWithLeaf(key, func(n *gleaf[K, V], i int) { f(annexedLeafOf[A](t, n), t.key(n, i), n.values[i]) })
}

// Hold finds key's entry and holds its leaf shared, for a container that
// copies what it needs of the entry, as a store's lookup copies its
// record's value, with no callback: Read calls its callback through a
// function value, under a deferred release. The caller must not use the map
// while it holds the entry, and must Release it; a panic meanwhile leaves
// the leaf held, as upstream's contract has it of code run on an entry. false,
// holding nothing, without one.
func (m *AnnexedMapOf[K, V, A, PA]) Hold(key K) (HeldEntry[K, V], bool) {
	n, i, ok := m.generic.hold(key)
	return HeldEntry[K, V]{n, i}, ok
}

// HeldEntry is an entry of an AnnexedMapOf whose leaf Hold holds shared.
type HeldEntry[K any, V any] struct {
	n *gleaf[K, V]
	i int
}

// Key is the entry's stored key.
func (e HeldEntry[K, V]) Key() K { return e.n.keyed()[e.i&(nodeSize-1)] }

// Value is the entry's value.
func (e HeldEntry[K, V]) Value() V { return e.n.values[e.i&(nodeSize-1)] }

// Release lets go of the entry's leaf.
func (e HeldEntry[K, V]) Release() { e.n.latch.unlockShared() }

// Peek finds key's candidates in its leaf as Hold finds its entry, but
// latches nothing, for a container that reads what it keeps of an entry
// itself, as a store reads a record, and checks its reads by the leaf's
// version, as a sequence lock's reader does: every writer that changes the
// leaf, its keys or what only its holder changes of what they refer to,
// holds it exclusively, and moves its version. The caller reads each
// candidate's stored key by Key, racing, and what it refers to, and trusts
// what it read only once UnchangedAfter says the leaf held still since the
// peek read it. absent is the map's holding no such key;
// ok is false, settling nothing, where the leaf can't be read racing, or
// keys are more than a word, whose racing reads could tear: the caller then
// holds the entry. A read racing writes nothing, so readers of one leaf
// share no cache line they write, where Hold's latch is one.
func (m *AnnexedMapOf[K, V, A, PA]) Peek(key K) (p LeafPeek[K, V], absent, ok bool) {
	if unsafe.Sizeof(*new(K)) != unsafe.Sizeof(uintptr(0)) {
		return p, false, false
	}
	t := &m.generic
	if t.root.Load() == nil {
		return p, true, true
	}
	n, word, lo, hi, tag, state := t.leafOfKey(key, t.code(key), false)
	switch state {
	case codesAbsent:
		return p, true, true
	case codesRacing, codesRacingEdge:
		return LeafPeek[K, V]{
			n: n, version: word &^ latchSharedMask, lo: lo, hi: hi, acc: uint64(hi),
			tag: tag, tagged: hi-lo > 1 && t.tagged(), edge: state == codesRacingEdge,
		}, false, true
	}
	return p, false, false
}

// LeafPeek is a key's candidates in its leaf, which Peek read racing.
type LeafPeek[K any, V any] struct {
	n       *gleaf[K, V]
	version uint64
	lo, hi  int
	// acc folds every word the peek read, from hi on, which the leaf's
	// reads fed: each check loads after them.
	acc    uint64
	tag    uint8
	tagged bool
	edge   bool
}

// Len is how many candidates there are, the keys whose codes are the key's.
func (p *LeafPeek[K, V]) Len() int { return p.hi - p.lo }

// Key is candidate j's stored key, read racing; false where the hash tag
// the leaf keeps for it isn't the key's, which it then isn't.
func (p *LeafPeek[K, V]) Key(j int) (k K, ok bool) {
	i := (p.lo + j) & (nodeSize - 1)
	if p.tagged {
		got := p.n.tags[i]
		p.acc ^= uint64(got)
		if got != p.tag {
			return k, false
		}
	}
	return p.n.keyed()[i], true
}

// Edge reports candidates that settle the key only where it is among them:
// at a leaf a hint named, by its first key or its last, none being the key
// leaves its absence unsettled.
func (p *LeafPeek[K, V]) Edge() bool { return p.edge }

// UnchangedAfter reports whether the leaf is still at the version the peek
// read it at, read after the load that gave w, and every read of the
// peek's, which its address depends on: what the caller read to reach w,
// the candidates' keys among it, is as the leaf held it then.
func (p *LeafPeek[K, V]) UnchangedAfter(w uint64) bool {
	return *after((*uint64)(unsafe.Pointer(&p.n.latch)), w^p.acc)&^latchSharedMask == p.version
}

// HoldWrite holds key's leaf exclusively, found as a write finds it, with
// the index of key's entry there, or of where it goes in, for a container
// that writes what it keeps of an entry itself, as a store writes its
// record, with no callbacks: UpdateOrInsertWithLeaf calls its own through
// function values. false, holding nothing, where the write needs more than
// the leaf, a new key in a full leaf, which splits, or the leaf can't be
// found racing: the caller then writes through UpdateOrInsertWithLeaf. The
// caller must not use the map while it holds the leaf, and must Insert or
// Release; a panic meanwhile leaves the leaf held, as upstream's contract
// has it of code run on an entry.
func (m *AnnexedMapOf[K, V, A, PA]) HoldWrite(key K) (LeafWrite[K, V, A, PA], bool) {
	n, version, w0, i, found, ok := m.generic.holdWrite(key)
	return LeafWrite[K, V, A, PA]{&m.generic, n, version, w0, i, found}, ok
}

// LeafWrite is a leaf of an AnnexedMapOf that HoldWrite holds exclusively.
type LeafWrite[K Key[K], V any, A any, PA LeafAnnex[A]] struct {
	t       *genericTree[K, V, annexOrder[K, A, PA]]
	n       *gleaf[K, V]
	version uint64
	w0      uint64
	i       int
	found   bool
}

// Leaf is the held leaf.
func (w LeafWrite[K, V, A, PA]) Leaf() AnnexedLeaf[K, A] { return annexedLeafOf[A](w.t, w.n) }

// Found reports whether the leaf holds the key's entry.
func (w LeafWrite[K, V, A, PA]) Found() bool { return w.found }

// Index is the index among the leaf's Keys of the key's entry, or of where
// it goes in.
func (w LeafWrite[K, V, A, PA]) Index() int { return w.i }

// Value is the key's entry's value, to change; the leaf must hold it.
func (w LeafWrite[K, V, A, PA]) Value() *V { return &w.n.values[w.i&(nodeSize-1)] }

// Insert puts key, equal to the key held for, with value in at Index, the
// leaf not holding it, and lets go of the leaf.
func (w LeafWrite[K, V, A, PA]) Insert(key K, value V) {
	w.t.insertKeyed(w.n, w.i, key, value, w.w0)
	w.n.latch.unlockExclusive(w.version)
}

// Release lets go of the leaf.
func (w LeafWrite[K, V, A, PA]) Release() { w.n.latch.unlockExclusive(w.version) }

// HoldRemove holds key's leaf exclusively, found as a write finds it
// (HoldWrite), with the index of key's entry there, for a container that
// takes what it keeps of an entry out itself, as a store kills its record,
// with no callback: DeleteWithLeaf calls its own through a function value.
// found is false, holding nothing, where the map holds no such key. ok is
// false, holding nothing, where the removal needs more than the leaf: the
// entry is the leaf's first, whose key may be a separator, or the leaf
// would be left underfull, or the leaf can't be found racing; the caller
// then removes through DeleteWithLeaf. The caller must not use the map
// while it holds the leaf, and must Remove or Release; a panic meanwhile
// leaves the leaf held, as upstream's contract has it of code run on an
// entry.
func (m *AnnexedMapOf[K, V, A, PA]) HoldRemove(key K) (w LeafRemove[K, V, A, PA], found, ok bool) {
	n, version, i, found, ok := m.generic.holdRemove(key)
	return LeafRemove[K, V, A, PA]{&m.generic, n, version, i}, found, ok
}

// LeafRemove is a leaf of an AnnexedMapOf that HoldRemove holds
// exclusively, with the entry to remove.
type LeafRemove[K Key[K], V any, A any, PA LeafAnnex[A]] struct {
	t       *genericTree[K, V, annexOrder[K, A, PA]]
	n       *gleaf[K, V]
	version uint64
	i       int
}

// Leaf is the held leaf.
func (w LeafRemove[K, V, A, PA]) Leaf() AnnexedLeaf[K, A] { return annexedLeafOf[A](w.t, w.n) }

// Index is the index among the leaf's Keys of the entry.
func (w LeafRemove[K, V, A, PA]) Index() int { return w.i }

// Key is the entry's key, as the map stored it.
func (w LeafRemove[K, V, A, PA]) Key() K { return w.n.keyed()[w.i&(nodeSize-1)] }

// Value is the entry's value.
func (w LeafRemove[K, V, A, PA]) Value() V { return w.n.values[w.i&(nodeSize-1)] }

// Remove takes the entry out, and lets go of the leaf.
func (w LeafRemove[K, V, A, PA]) Remove() {
	w.t.removeEntry(w.n, w.i, false)
	w.n.latch.unlockExclusive(w.version)
}

// Release lets go of the leaf, the entry in it.
func (w LeafRemove[K, V, A, PA]) Release() { w.n.latch.unlockExclusive(w.version) }

// WalkLeaves walks the map's leaves with entries in b, in key order or in
// reverse, a leaf at a time: Next hands out each leaf's keys and values in
// b, ascending, holding the leaf shared until the next call, or Close. It is
// for a container that runs its own loop over a leaf's entries, as an
// iterator the compiler inlines into its caller's range loop, so that the
// loop's body is the caller's own code, as upstream's closures are inlined
// into its scans: Go would otherwise call the body through a function value
// at every entry.
//
// The walk is upstream's latched scan, and its contract: the caller must
// not use the map while it holds a leaf, and must call Close if it stops
// before Next returns false. A panic that leaves a leaf held leaves it so:
// as upstream's, code running inside a walk must not panic. It panics as
// Scan does.
func (m *AnnexedMapOf[K, V, A, PA]) WalkLeaves(b Bounds[K], reverse bool) LeafWalk[K, V, A, PA] {
	checkBounds[K, annexOrder[K, A, PA]](&b)
	return LeafWalk[K, V, A, PA]{w: leafWalk[K, V, annexOrder[K, A, PA]]{t: &m.generic, b: b, reverse: reverse}}
}

// LeafWalk is a walk of an AnnexedMapOf's leaves (WalkLeaves).
type LeafWalk[K Key[K], V any, A any, PA LeafAnnex[A]] struct {
	w leafWalk[K, V, annexOrder[K, A, PA]]
}

// Next lets go of the leaf it last handed out, and hands out the next leaf
// with entries in the walk's bounds, held shared: its keys and values in
// them, ascending, valid until the next call or Close. false, holding
// nothing, past the last.
func (w *LeafWalk[K, V, A, PA]) Next() (keys []K, values []V, ok bool) {
	n, from, to, ok := w.w.next()
	if !ok {
		return nil, nil, false
	}
	if windowed[annexOrder[K, A, PA], K]() {
		keys = n.keyed()[from:to:to]
	} else {
		keys = make([]K, to-from)
		for i := range keys {
			keys[i] = w.w.t.key(n, from+i)
		}
	}
	return keys, n.values[from:to:to], true
}

// Close lets go of the leaf the walk holds, if any, and ends the walk.
func (w *LeafWalk[K, V, A, PA]) Close() { w.w.close() }

// annexOrder orders keys by their Key methods, as custom does, for a map
// whose leaves keep annexes A. Its size, two bytes, tells it from natural
// and custom where the trees test shapes: it takes custom's paths, and the
// annexes' (annexed).
type annexOrder[K Key[K], A any, PA LeafAnnex[A]] struct{ _ [2]byte }

func (annexOrder[K, A, PA]) compare(a, b K) int              { return a.Compare(b) }
func (annexOrder[K, A, PA]) window(key K, offset int) uint64 { return key.Window(offset) }
func (annexOrder[K, A, PA]) textual() bool                   { return true }
func (annexOrder[K, A, PA]) own(key K) K                     { return custom[K]{}.own(key) }

func (annexOrder[K, A, PA]) annexType() reflect.Type { return reflect.TypeFor[A]() }
func (annexOrder[K, A, PA]) splitAnnex(left, right unsafe.Pointer) {
	PA((*A)(left)).Split((*A)(right))
}
func (annexOrder[K, A, PA]) mergeAnnex(left, right unsafe.Pointer) {
	PA((*A)(left)).Merge((*A)(right))
	*(*A)(right) = *new(A)
}
func (annexOrder[K, A, PA]) releaseAnnex(annex unsafe.Pointer) {
	PA((*A)(annex)).Release()
	*(*A)(annex) = *new(A)
}

// leafAnnexes is what an annexed tree's order does with its leaves'
// annexes, by their addresses.
type leafAnnexes interface {
	annexType() reflect.Type
	splitAnnex(left, right unsafe.Pointer)
	mergeAnnex(left, right unsafe.Pointer)
	releaseAnnex(annex unsafe.Pointer)
}

// annexed reports a tree whose leaves keep annexes: of the orders, only
// annexOrder is two bytes. A size test, it folds for each shape.
func annexed[O keyOrder[K], K any]() bool { return unsafe.Sizeof(*(*O)(nil)) == 2 }

// annexes is an annexed tree's order, which handles its annexes.
func (t *genericTree[K, V, O]) annexes() leafAnnexes { return any(*new(O)).(leafAnnexes) }

// setupAnnexes makes an annexed tree's leaves the type that keeps an annex
// after its leaf, as the tree first makes its root.
func (t *genericTree[K, V, O]) setupAnnexes() {
	leaf := reflect.TypeFor[keyedLeaf[K, V]]()
	if prefixed[O, K]() {
		leaf = reflect.TypeFor[textLeaf[K, V]]()
	}
	t.annexLeaf = reflect.StructOf([]reflect.StructField{
		{Name: "Leaf", Type: leaf},
		{Name: "Annex", Type: t.annexes().annexType()},
	})
	t.annexAt = t.annexLeaf.Field(1).Offset
}

// newAnnexedLeaf is a new leaf of an annexed tree, its annex zero.
func (t *genericTree[K, V, O]) newAnnexedLeaf() *gleaf[K, V] {
	return (*gleaf[K, V])(reflect.New(t.annexLeaf).UnsafePointer())
}

// annexOf is the address of leaf n's annex.
func (t *genericTree[K, V, O]) annexOf(n *gleaf[K, V]) unsafe.Pointer {
	return unsafe.Add(unsafe.Pointer(n), t.annexAt)
}

// splitAnnex and mergeAnnex call the annexes' Split and Merge for leaves
// of an annexed tree. Callers test annexed first: the inliner prices these
// bodies before the test folds, so a call here would stay in every tree.
func (t *genericTree[K, V, O]) splitAnnex(left, right *gleaf[K, V]) {
	t.annexes().splitAnnex(t.annexOf(left), t.annexOf(right))
}

func (t *genericTree[K, V, O]) mergeAnnex(left, right *gleaf[K, V]) {
	t.annexes().mergeAnnex(t.annexOf(left), t.annexOf(right))
}

// releaseAnnexes releases the annex of every leaf under h, for Clear.
func (t *genericTree[K, V, O]) releaseAnnexes(h *gheader) {
	if h == nil {
		return
	}
	if h.leaf {
		t.annexes().releaseAnnex(t.annexOf(asLeaf[K, V](h)))
		return
	}
	n := asInner[K](h)
	for i := range int(n.count.Load()) {
		t.releaseAnnexes(n.children[i].Load())
	}
}

// annexedLeafOf is leaf n of annexed tree t, held, whose annexes are As,
// for a callback: its keys in place, and its head, where it keeps them as
// Ks; a text leaf's keys read into a slice of their own, as what a callback
// does to them, nothing, needs no leaf.
func annexedLeafOf[A any, K any, V any, O keyOrder[K]](t *genericTree[K, V, O], n *gleaf[K, V]) AnnexedLeaf[K, A] {
	if prefixed[O, K]() {
		return textAnnexedLeafOf[A](t, n)
	}
	count := int(n.count.Load())
	return AnnexedLeaf[K, A]{
		annex: (*A)(t.annexOf(n)),
		keys:  n.keyed()[:count:count],
		head:  &(*keyedLeaf[K, V])(unsafe.Pointer(n)).head,
	}
}

// textAnnexedLeafOf is annexedLeafOf for a text leaf, out of line: the
// keyed case inlines into its callers without it.
func textAnnexedLeafOf[A any, K any, V any, O keyOrder[K]](t *genericTree[K, V, O], n *gleaf[K, V]) AnnexedLeaf[K, A] {
	keys := make([]K, n.count.Load())
	for i := range keys {
		keys[i] = t.key(n, i)
	}
	return AnnexedLeaf[K, A]{annex: (*A)(t.annexOf(n)), keys: keys}
}

// compareSeparatorHeld compares internal node n's separator i with key for
// an annexed tree, whose keys may read what only a writer holding their
// leaf changes, as upstream's latched tree compares keys that don't order
// by their own bytes (D57): it walks to the leaf whose first key the
// separator is, and compares that key holding the leaf shared. false where
// a racing walk finds no such leaf; the caller's check of n's version
// settles the rest.
func (t *genericTree[K, V, O]) compareSeparatorHeld(n *ginner[K], i int, key K, equal int) (int, bool) {
	h := n.children[i&(nodeSize-1)].Load()
	for depth := 0; h != nil && !h.leaf && depth < maxDepth; depth++ {
		h = asInner[K](h).children[0].Load()
	}
	if h == nil || !h.leaf {
		return 0, false
	}
	leaf := asLeaf[K, V](h)
	leaf.latch.lockShared()
	defer leaf.latch.unlockShared()
	if leaf.count.Load() == 0 {
		return 0, false
	}
	return t.compareTied(t.key(leaf, 0), key, equal), true
}

// walkHeld is walk for an annexed tree, as upstream's latched scan: visit
// runs on each entry in b, in key order or in reverse, holding its leaf
// shared, where walk copies a leaf's entries and resumes from a key it
// compares unheld. The walk goes on to the held leaf's neighbor through the
// internal nodes its descent kept, and latches it before it lets go of the
// held one; where one of those nodes changed, it searches again from the
// held leaf's last key, which the leaf keeps where it is. An entry present
// throughout is visited once.
func (t *genericTree[K, V, O]) walkHeld(b *Bounds[K], reverse bool, visit func(K, V) bool) {
	t.walkLeavesHeld(b, reverse, func(n *gleaf[K, V], from, to int) bool {
		// A custom key is read in place, from the leaf's keys.
		keys := n.keyed()
		key := func(i int) K {
			if windowed[O, K]() {
				return keys[i&(nodeSize-1)]
			}
			return t.key(n, i)
		}
		if !reverse {
			for i := from; i < to; i++ {
				if !visit(key(i), n.values[i&(nodeSize-1)]) {
					return false
				}
			}
			return true
		}
		for i := to - 1; i >= from; i-- {
			if !visit(key(i), n.values[i&(nodeSize-1)]) {
				return false
			}
		}
		return true
	})
}

// walkLeavesHeld is walkHeld a leaf at a time: leaf runs once on each leaf
// with entries in b, holding it shared, with the range [from, to) of them
// in b, in key order or in reverse, until it returns false. A panicking leaf
// lets go of the leaf it holds.
func (t *genericTree[K, V, O]) walkLeavesHeld(b *Bounds[K], reverse bool, leaf func(n *gleaf[K, V], from, to int) bool) {
	w := leafWalk[K, V, O]{t: t, b: *b, reverse: reverse}
	defer w.close()
	for {
		n, lo, hi, ok := w.next()
		if !ok || !leaf(n, lo, hi) {
			return
		}
	}
}

// leafWalk walks an annexed tree's leaves with entries in its bounds, in
// key order or in reverse, holding the one next handed out shared until the
// next call or close, as upstream's latched scan holds each leaf its closure
// runs on: it latches a leaf's neighbor before it lets go of the leaf, and
// where a node on the way changed, it searches again from the leaf's last
// key, which the leaf keeps where it is. An entry present throughout is in
// one leaf it hands out. A leaf wholly within the bounds is known so by its
// first key and its last.
type leafWalk[K any, V any, O keyOrder[K]] struct {
	t       *genericTree[K, V, O]
	b       Bounds[K]
	path    gpath[K]
	n       *gleaf[K, V] // the leaf held, or nil
	reverse bool
	started bool
	// last is whether the leaf held reaches the bound the walk goes to.
	last bool
	done bool
	// touched is what touchNext last read, kept so that its loads are.
	touched uint64
}

// touchNext reads, plainly, the leaf the walk likely goes to after the one
// held, the next child of the held leaf's parent, its header's line and its
// keys' first, so that their fetches overlap the caller's loop over the
// held leaf's entries: the hardware can't guess a node's address. A hint
// only: the walk latches whatever leaf it then finds. For custom keys,
// whose leaves keep their keys as K.
func (w *leafWalk[K, V, O]) touchNext() {
	level := w.path.depth - 1
	if level < 0 {
		return
	}
	at := &w.path.levels[level]
	edge := at.edge + 1
	if w.reverse {
		edge = at.edge - 1
	}
	if edge < 0 || edge >= at.count {
		return
	}
	c := *(**gheader)(unsafe.Pointer(&at.n.children[edge&(nodeSize-1)]))
	if c == nil || !c.leaf {
		return
	}
	// The header's line and the keys' first, of a keyed leaf's.
	w.touched = uint64(*(*int32)(unsafe.Pointer(&c.count))) ^ uint64(*(*uint8)(unsafe.Pointer(&asLeaf[K, V](c).keyed()[0])))
}

// next lets go of the leaf held and hands out the next with entries in the
// bounds, held shared, with the range [lo, hi) of them; false, holding
// nothing, past the last.
func (w *leafWalk[K, V, O]) next() (n *gleaf[K, V], lo, hi int, ok bool) {
	if w.done {
		return nil, 0, 0, false
	}
	t, b := w.t, &w.b
	if !w.started {
		w.started = true
		if t.root.Load() == nil {
			w.done = true
			return nil, 0, 0, false
		}
		seek, from, strict := seekFirst, b.lower, false
		if w.reverse {
			seek, from, strict = seekLast, b.upper, b.upperKind == excluded
		}
		var code uint64
		if !w.reverse && b.lowerKind != unbounded || w.reverse && b.upperKind != unbounded {
			seek, code = seekKey, t.code(from)
		}
		w.n, _ = t.descend(from, code, seek, strict, shared, &w.path)
	} else if w.last || !w.advance() {
		w.close()
		return nil, 0, 0, false
	}
	// Only a bounded walk compares its keys with the bounds.
	lower, upper := b.lowerKind != unbounded, b.upperKind != unbounded
	for {
		n := w.n
		count := int(n.count.Load())
		lo, hi := 0, count
		if lower && count > 0 && !aboveLower[K, O](b, t.key(n, 0)) {
			for lo < count && !aboveLower[K, O](b, t.key(n, lo)) {
				lo++
			}
		}
		if upper && count > 0 && !belowUpper[K, O](b, t.key(n, count-1)) {
			for hi > lo && !belowUpper[K, O](b, t.key(n, hi-1)) {
				hi--
			}
		}
		// The walk ends at the bound its direction reaches.
		w.last = !w.reverse && hi < count || w.reverse && lo > 0
		if lo < hi {
			// The touch reads racing, plainly: only where racing reads may.
			if seqlockPlainStores && windowed[O, K]() && !w.last {
				w.touchNext()
			}
			return n, lo, hi, true
		}
		if w.last || !w.advance() {
			w.close()
			return nil, 0, 0, false
		}
	}
}

// advance latches the held leaf's neighbor and lets go of the leaf: false,
// holding nothing, where it has none.
func (w *leafWalk[K, V, O]) advance() bool {
	t, n := w.t, w.n
	next, end := t.neighborHeld(&w.path, w.reverse)
	for next == nil && !end {
		count := int(n.count.Load())
		if count == 0 {
			// Only the root leaf is ever empty, and it has no neighbor
			// while held.
			return false
		}
		last := count - 1
		if w.reverse {
			last = 0
		}
		k := t.key(n, last)
		t.descend(k, t.code(k), seekKey, false, optimistic, &w.path)
		next, end = t.neighborHeld(&w.path, w.reverse)
	}
	n.latch.unlockShared()
	w.n = next
	return !end
}

// close lets go of the leaf held, if any, and ends the walk.
func (w *leafWalk[K, V, O]) close() {
	if w.n != nil {
		w.n.latch.unlockShared()
		w.n = nil
	}
	w.done = true
}

// neighborHeld latches shared the neighbor of the leaf path leads to, on
// the right, or the left in reverse: from the nearest node of the path with
// an edge on that side, down the edges nearest the leaf, each node read at
// a version, which the path then keeps. The latched leaf is the neighbor if
// every node from there down is still at that version. end where the leaf
// has no neighbor, and none and false where a node changed.
func (t *genericTree[K, V, O]) neighborHeld(path *gpath[K], reverse bool) (next *gleaf[K, V], end bool) {
	for level := path.depth - 1; level >= 0; level-- {
		at := &path.levels[level]
		edge := at.edge + 1
		if reverse {
			edge = at.edge - 1
		}
		if edge < 0 || edge >= at.count {
			continue
		}
		// The node's version is checked once the neighbor is latched
		// (unchanged, below), which a stale child fails.
		h := at.n.children[edge&(nodeSize-1)].Load()
		if h == nil {
			return nil, false
		}
		depth := level + 1
		for !h.leaf {
			if depth == maxDepth {
				return nil, false
			}
			c := asInner[K](h)
			v := c.latch.readVersion()
			count := int(c.count.Load())
			if count == 0 || count > nodeSize {
				return nil, false
			}
			e := 0
			if reverse {
				e = count - 1
			}
			h = c.children[e&(nodeSize-1)].Load()
			if h == nil || !c.latch.isUnchanged(v) {
				return nil, false
			}
			path.levels[depth] = gpathLevel[K]{n: c, version: v, edge: e, count: count}
			depth++
		}
		h.latch.lockShared()
		path.levels[level].edge, path.depth = edge, depth
		if !path.unchanged(level) {
			h.latch.unlockShared()
			return nil, false
		}
		return asLeaf[K, V](h), false
	}
	return nil, path.depth == 0 || path.unchanged(0)
}

// UpdateRange and UpdateAll would change the map from inside an iteration,
// which holds each leaf an AnnexedMapOf visits: they panic. Upstream's
// shared map has neither.
func (m *AnnexedMapOf[K, V, A, PA]) UpdateRange(Bounds[K], func(K, V) V) {
	panic("gobbtree: UpdateRange on an AnnexedMapOf, whose iterations hold their leaves")
}

func (m *AnnexedMapOf[K, V, A, PA]) UpdateAll(func(K, V) V) {
	panic("gobbtree: UpdateAll on an AnnexedMapOf, whose iterations hold their leaves")
}
