package gobbtree

import (
	"reflect"
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
	leaf  unsafe.Pointer
	view  leafView[K]
}

// Annex is the leaf's annex.
func (l AnnexedLeaf[K, A]) Annex() *A { return l.annex }

// Keys are the leaf's keys, in order, not to be changed but by ReplaceKey.
func (l AnnexedLeaf[K, A]) Keys() []K { return l.view.leafKeys(l.leaf) }

// ReplaceKey replaces the leaf's key i with key, which must compare equal
// to it: a key that refers to where its bytes lie, as the store's do, is
// replaced as they move. Where the leaf keeps a copy of its first key, for
// lookups past it, the copy is replaced too.
func (l AnnexedLeaf[K, A]) ReplaceKey(i int, key K) { l.view.replaceKey(l.leaf, i, key) }

// UpdateOrInsertWithLeaf runs occupied on the entry whose key equals key,
// its value to change, or, if the map holds none, vacant on key and value,
// which go in right after it; each with the leaf the entry is in, or goes
// in, held exclusively. found tells which ran. Either may refuse, for want
// of what it would hold, by returning false: the map is then unchanged, and
// ok is false. vacant runs before a leaf that is full splits, on that leaf.
// Callbacks must not use the map.
func (m *AnnexedMapOf[K, V, A, PA]) UpdateOrInsertWithLeaf(key K, value V, occupied func(leaf AnnexedLeaf[K, A], key K, value *V) bool, vacant func(leaf AnnexedLeaf[K, A], key K, value V) bool) (found, ok bool) {
	t := &m.generic
	return t.insertWithLeaf(key, value,
		func(n *gleaf[K, V], i int) bool { return occupied(annexedLeafOf[A](t, n), t.key(n, i), &n.values[i]) },
		func(n *gleaf[K, V], key K) bool { return vacant(annexedLeafOf[A](t, n), key, value) })
}

// DeleteWithLeaf removes the entry whose key equals key and returns it, the
// key as the map stored it, false without one. f runs once on it, with the
// leaf it is in, held exclusively, right before it comes out. f must not use
// the map.
func (m *AnnexedMapOf[K, V, A, PA]) DeleteWithLeaf(key K, f func(leaf AnnexedLeaf[K, A], key K, value V)) (K, V, bool) {
	t := &m.generic
	return t.removeWithLeaf(key, func(n *gleaf[K, V], i int) { f(annexedLeafOf[A](t, n), t.key(n, i), n.values[i]) })
}

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
// for a callback.
func annexedLeafOf[A any, K any, V any, O keyOrder[K]](t *genericTree[K, V, O], n *gleaf[K, V]) AnnexedLeaf[K, A] {
	return AnnexedLeaf[K, A]{annex: (*A)(t.annexOf(n)), leaf: unsafe.Pointer(n), view: t}
}

// leafView is what an AnnexedLeaf asks of its tree, by the leaf's address.
type leafView[K any] interface {
	leafKeys(leaf unsafe.Pointer) []K
	replaceKey(leaf unsafe.Pointer, i int, key K)
}

func (t *genericTree[K, V, O]) leafKeys(leaf unsafe.Pointer) []K {
	n := (*gleaf[K, V])(leaf)
	count := int(n.count.Load())
	if !prefixed[O, K]() {
		return n.keyed()[:count:count]
	}
	keys := make([]K, count)
	for i := range keys {
		keys[i] = t.key(n, i)
	}
	return keys
}

func (t *genericTree[K, V, O]) replaceKey(leaf unsafe.Pointer, i int, key K) {
	n := (*gleaf[K, V])(leaf)
	if i < 0 || i >= int(n.count.Load()) {
		panic("gobbtree: key index out of range")
	}
	if prefixed[O, K]() {
		// Text keys are their bytes, which the leaf keeps: an equal key is
		// the same one.
		return
	}
	n.keyed()[i] = key
	// The leaf's head is a copy of its first key, which lookups past its
	// separator compare racing: they read the old copy or the new, equal
	// either way.
	if i == 0 {
		if head := &(*keyedLeaf[K, V])(leaf).head; head.Load() != nil {
			k := key
			head.Store(&k)
		}
	}
}

// compareSeparatorHeld compares internal node n's separator i with key for
// an annexed tree, whose keys may read what only a writer holding their
// leaf changes, as upstream's latched tree compares keys that don't order
// by their own bytes (D57): it walks to the leaf whose first key the
// separator is, and compares that key holding the leaf shared. false where
// a racing walk finds no such leaf; the caller's check of n's version
// settles the rest.
func (t *genericTree[K, V, O]) compareSeparatorHeld(n *ginner[K], i int, key K) (int, bool) {
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
	return t.compare(t.key(leaf, 0), key), true
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
	if t.root.Load() == nil {
		return
	}
	var path gpath[K]
	seek, from, strict := seekFirst, b.lower, false
	if reverse {
		seek, from, strict = seekLast, b.upper, b.upperKind == excluded
	}
	var code uint64
	if !reverse && b.lowerKind != unbounded || reverse && b.upperKind != unbounded {
		seek, code = seekKey, t.code(from)
	}
	n, _ := t.descend(from, code, seek, strict, shared, &path)
	defer func() {
		if n != nil {
			n.latch.unlockShared()
		}
	}()
	for {
		count := int(n.count.Load())
		if !reverse {
			for i := 0; i < count; i++ {
				k := t.key(n, i)
				if !aboveLower[K, O](b, k) {
					continue
				}
				if !belowUpper[K, O](b, k) || !visit(k, n.values[i]) {
					return
				}
			}
		} else {
			for i := count - 1; i >= 0; i-- {
				k := t.key(n, i)
				if !belowUpper[K, O](b, k) {
					continue
				}
				if !aboveLower[K, O](b, k) || !visit(k, n.values[i]) {
					return
				}
			}
		}
		next, end := t.neighborHeld(&path, reverse)
		for next == nil && !end {
			if count == 0 {
				// Only the root leaf is ever empty, and it has no
				// neighbor while held.
				return
			}
			last := count - 1
			if reverse {
				last = 0
			}
			k := t.key(n, last)
			t.descend(k, t.code(k), seekKey, false, optimistic, &path)
			next, end = t.neighborHeld(&path, reverse)
		}
		n.latch.unlockShared()
		n = next
		if end {
			return
		}
	}
}

// neighborHeld latches shared the neighbor of the leaf path leads to, on
// the right, or the left in reverse: from the nearest node of the path with
// an edge on that side, down the edges nearest the leaf, each node read at
// a version, which the path then keeps. The latched leaf is the neighbor if
// every node from there down is still at that version. end where the leaf
// has no neighbor, and none and false where a node changed.
func (t *genericTree[K, V, O]) neighborHeld(path *gpath[K], reverse bool) (next *gleaf[K, V], end bool) {
	for level := path.depth - 1; level >= 0; level-- {
		at := path.levels[level]
		edge := at.edge + 1
		if reverse {
			edge = at.edge - 1
		}
		if edge < 0 || edge >= at.count {
			continue
		}
		h := at.n.children[edge&(nodeSize-1)].Load()
		if h == nil || !at.n.latch.isUnchanged(at.version) {
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
