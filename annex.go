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
