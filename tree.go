package gobbtree

import (
	"cmp"
	"iter"
)

// Pair is a key and value, as batch operations take them.
type Pair[K any, V any] struct {
	Key   K
	Value V
}

// Result is a value and a presence flag. A present zero value has OK=true.
type Result[V any] struct {
	Value V
	OK    bool
}

// Tree is an ordered map for a single owner. Its zero value is ready to use.
// Concurrent reads are safe only when no goroutine modifies the tree.
type Tree[K cmp.Ordered, V any] struct{ treeCore[K, V, natural[K]] }

// treeCore implements Tree and TreeOf for one key ordering, on the Map's
// tree, which it owns (generic_latched_owner.go): upstream's unsync and
// shared trees keep the same nodes.
type treeCore[K any, V any, O keyOrder[K]] struct {
	generic *genericTree[K, V, O]
	length  int
}

// NewTree creates an empty tree without synchronization.
func NewTree[K cmp.Ordered, V any]() *Tree[K, V] { return new(Tree[K, V]) }
func (t *treeCore[K, V, O]) Len() int            { return t.length }
func (t *treeCore[K, V, O]) IsEmpty() bool       { return t.length == 0 }
func (t *treeCore[K, V, O]) Clear() {
	t.generic = nil
	t.length = 0
}

// Get returns a copy of the value associated with key.
func (t *treeCore[K, V, O]) Get(key K) (V, bool) {
	if t.generic != nil {
		return t.generic.ownerGet(key)
	}
	var zero V
	return zero, false
}
func (t *treeCore[K, V, O]) Contains(key K) bool { _, ok := t.Get(key); return ok }

// Insert stores value, returning the previous value and whether it replaced
// an entry. Equal keys keep the original stored key, as in upstream bbtree.
func (t *treeCore[K, V, O]) Insert(key K, value V) (V, bool) {
	if t.generic == nil {
		t.generic = &genericTree[K, V, O]{owner: true}
	}
	old, found := t.generic.ownerInsert(key, value, insertReplace, nil)
	if !found {
		t.length++
	}
	return old, found
}

// Delete removes an entry, returning its previous value and presence.
func (t *treeCore[K, V, O]) Delete(key K) (V, bool) {
	_, v, ok := t.remove(seekKey, key, false)
	return v, ok
}

// remove removes seek's entry, dropping the tree once it is empty; handOut
// is false where the caller drops the key returned.
func (t *treeCore[K, V, O]) remove(seek int, key K, handOut bool) (K, V, bool) {
	if t.generic == nil {
		var k K
		var v V
		return k, v, false
	}
	k, v, ok := t.generic.ownerRemove(seek, key, nil, handOut)
	if ok {
		t.length--
		if t.length == 0 {
			t.generic = nil
		}
	}
	return k, v, ok
}

// end is the tree's first entry, or its last.
func (t *treeCore[K, V, O]) end(last bool) (k K, v V, ok bool) {
	if t.generic != nil {
		t.generic.walk(&Bounds[K]{}, last, func(key K, value V) bool {
			k, v, ok = key, value, true
			return false
		})
	}
	return k, v, ok
}

func (t *treeCore[K, V, O]) First() (K, V, bool) { return t.end(false) }
func (t *treeCore[K, V, O]) Last() (K, V, bool)  { return t.end(true) }
func (t *treeCore[K, V, O]) PopFirst() (K, V, bool) {
	var zero K
	return t.remove(seekFirst, zero, true)
}
func (t *treeCore[K, V, O]) PopLast() (K, V, bool) {
	var zero K
	return t.remove(seekLast, zero, true)
}

// Range iterates the entries with keys in [lower, upper), ascending, as
// Scan(Between(lower, upper)) does.
func (t *treeCore[K, V, O]) Range(lower, upper K) iter.Seq2[K, V] {
	return t.Scan(Between(lower, upper))
}

// RangeReverse is Range in descending order.
func (t *treeCore[K, V, O]) RangeReverse(lower, upper K) iter.Seq2[K, V] {
	return t.ScanReverse(Between(lower, upper))
}

// Ascend iterates the entries with keys at or above from, ascending.
func (t *treeCore[K, V, O]) Ascend(from K) iter.Seq2[K, V] { return t.Scan(AtLeast(from)) }

// Descend iterates the entries with keys at or below from, descending.
func (t *treeCore[K, V, O]) Descend(from K) iter.Seq2[K, V] { return t.ScanReverse(AtMost(from)) }

// Scan iterates the entries with keys in b, ascending, as upstream's range
// does. Neither the loop body nor another goroutine may mutate the tree
// meanwhile. Iterating a non-empty tree panics on bounds upstream rejects
// (see Bounds), as upstream's range does.
func (t *treeCore[K, V, O]) Scan(b Bounds[K]) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if t.length != 0 {
			checkBounds[K, O](&b)
		}
		t.scan(&b, false, yield)
	}
}

// ScanReverse is Scan in descending order.
func (t *treeCore[K, V, O]) ScanReverse(b Bounds[K]) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if t.length != 0 {
			checkBounds[K, O](&b)
		}
		t.scan(&b, true, yield)
	}
}

// scan visits the keys in b.
func (t *treeCore[K, V, O]) scan(b *Bounds[K], reverse bool, visit func(K, V) bool) {
	if t.generic != nil {
		t.generic.walk(b, reverse, visit)
	}
}

// All iterates all entries in ascending order. Mutation during iteration is forbidden.
func (t *treeCore[K, V, O]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if t.generic != nil {
			t.generic.walk(&Bounds[K]{}, false, yield)
		}
	}
}

// Reverse iterates all entries in descending order.
func (t *treeCore[K, V, O]) Reverse() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if t.generic != nil {
			t.generic.walk(&Bounds[K]{}, true, yield)
		}
	}
}

// GetMany fills dst in query order, including repeated keys and misses.
// It panics before writing if dst is shorter than keys. No allocation is needed.
func (t *treeCore[K, V, O]) GetMany(keys []K, dst []Result[V]) {
	if len(dst) < len(keys) {
		panic("gobbtree: GetMany destination too short")
	}

	if g := t.generic; g != nil && g.wordCodes() {
		// Whole groups of eight go through the batched lookup.
		start := 0
		for ; start+batchLanes <= len(keys); start += batchLanes {
			g.getMany8((*[batchLanes]K)(keys[start:]), (*[batchLanes]Result[V])(dst[start:]))
		}
		keys, dst = keys[start:], dst[start:]
	}
	for i, key := range keys {
		dst[i].Value, dst[i].OK = t.Get(key)
	}
}

// InsertMany inserts in input order and fills dst with previous values.
func (t *treeCore[K, V, O]) InsertMany(entries []Pair[K, V], dst []Result[V]) {
	if len(dst) < len(entries) {
		panic("gobbtree: InsertMany destination too short")
	}
	for i, e := range entries {
		dst[i].Value, dst[i].OK = t.Insert(e.Key, e.Value)
	}
}
