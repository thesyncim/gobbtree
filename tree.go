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

// treeCore implements Tree and TreeOf for one key ordering.
//
// Word keys with word values run on wordTree; every other key and value on
// genericTree, the Map's tree, as upstream's unsync and shared trees keep
// the same nodes.
type treeCore[K any, V any, O keyOrder[K]] struct {
	generic *genericTree[K, V, O]
	words   *wordTree
	length  int
}

// NewTree creates an empty tree without synchronization.
func NewTree[K cmp.Ordered, V any]() *Tree[K, V] { return new(Tree[K, V]) }
func (t *treeCore[K, V, O]) Len() int            { return t.length }
func (t *treeCore[K, V, O]) IsEmpty() bool       { return t.length == 0 }
func (t *treeCore[K, V, O]) Clear() {
	t.generic = nil
	t.words = nil
	t.length = 0
}

// Get returns a copy of the value associated with key.
func (t *treeCore[K, V, O]) Get(key K) (V, bool) {
	if words := t.words; words != nil {
		code := numericKey[O](key)
		if n := words.hintedLeaf(code); n != nil {
			// A key the hinted leaf holds is there; one between its first
			// and last keys is absent. Only a miss reads those.
			count := int(n.n)
			i := firstTag(&n.tags, count, code)
			if i < count && n.entries()[i].key != code {
				i = n.lowerEntry(code)
			}
			if i < count && n.entries()[i].key == code {
				value := n.entries()[i].value
				if words.valueKind == 1 {
					return any(value).(V), true
				}
				return unpackWord[V](value, words.valueKind), true
			}
			if count > 0 && code > n.entries()[0].key && code < n.entries()[count-1].key {
				var zero V
				return zero, false
			}
		}
		n := words.rootNode
		if hint := words.routes.lookup(code); hint != 0 {
			n = words.node(hint)
		}
		var parent, grand *wordNode
		edge, parentEdge := 0, 0
		for n != nil {
			if n.leaf {
				if parent != nil {
					words.hintParent(code, parent, grand, parentEdge)
				}
				i := firstTag(&n.tags, int(n.n), code)
				if i < int(n.n) && n.entries()[i].key != code {
					i = n.lowerEntry(code)
				}
				if i < int(n.n) && n.entries()[i].key == code {
					value := n.entries()[i].value
					if words.valueKind == 1 {
						return any(value).(V), true
					}
					return unpackWord[V](value, words.valueKind), true
				}
				break
			}
			index := 0
			if n.directory != 0 {
				index = directoryRoute(&n.keys, &words.directories[n.directory-1], int(n.n), code)
			} else {
				projection := code >> n.shift
				prefix := (*n.prefixWord() >> n.shift) &^ uint64(65535)
				if projection > prefix|65535 {
					index = int(n.n) - 1
				} else if projection >= prefix {
					index = max(0, upperWordInline(&n.inner().inline, int(n.n), uint16(projection))-1)
					if index > 0 && n.inner().inline[index] == uint16(projection) && n.keys[index] > code {
						index = max(0, lowerCodeScalar(&n.keys, index+1, code+1)-1)
					}
				}
			}
			grand, parentEdge = parent, edge
			parent, edge = n, index
			n = words.node(n.inner().children[index])
		}
		var zero V
		return zero, false
	}

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
	if words := t.words; words != nil {
		code, packed := numericKey[O](key), packWord(value)
		if leaf := words.deletedLeaf; leaf != nil {
			words.deletedLeaf = nil
			if code == words.deletedKey {
				words.insertLeaf(leaf, words.deletedIndex, code, packed)
				t.length++
				var zero V
				return zero, false
			}
			// A key between the leaf's first and last is its own, and the
			// deletion left it room.
			if count := int(leaf.n); count > 0 && code > leaf.entries()[0].key && code < leaf.entries()[count-1].key {
				if i := leaf.lowerEntry(code); leaf.entries()[i].key != code {
					words.insertLeaf(leaf, i, code, packed)
					t.length++
					var zero V
					return zero, false
				}
			}
		}
		// Appends past the largest key go straight to the rightmost leaf.
		if n := words.tail; n != nil && n.next == 0 && n.n > 0 && code > n.entries()[n.n-1].key {
			if n.n < nodeSize {
				words.inserted = code
				n.appendLeafPlain(code, packed)
				t.length++
				var zero V
				return zero, false
			}
			if words.appendFull(n, code, packed) {
				t.length++
				var zero V
				return zero, false
			}
		}
		if n := words.hintedLeaf(code); n != nil {
			// A key the hinted leaf holds is replaced there; one between its
			// first and last keys is its own, inserted if it has room. Only
			// a miss reads those.
			count := int(n.n)
			i := firstTag(&n.tags, count, code)
			if i < count && n.entries()[i].key != code {
				i = n.lowerEntry(code)
			}
			if i < count && n.entries()[i].key == code {
				old := n.entries()[i].value
				n.entries()[i].value = packed
				if words.valueKind == 1 {
					return any(old).(V), true
				}
				return unpackWord[V](old, words.valueKind), true
			}
			if count > 0 && code > n.entries()[0].key && code < n.entries()[count-1].key {
				if count < nodeSize {
					if i == count {
						// No tag matched: the key goes where it sorts.
						i = n.lowerEntry(code)
					}
					words.inserted = code
					if n.next == 0 {
						words.tail = n
					}
					n.insertLeafPlain(i, code, packed)
					t.length++
					var zero V
					return zero, false
				}
			}
		}
		n := words.rootNode
		if n == nil {
			words.insert(code, packed)
		} else {
			if hint := words.routes.lookup(code); hint != 0 {
				n = words.node(hint)
			}
			var parent, grand *wordNode
			edge, parentEdge := 0, 0
			for !n.leaf {
				index := 0
				if n.directory != 0 {
					index = directoryRoute(&n.keys, &words.directories[n.directory-1], int(n.n), code)
				} else {
					projection := code >> n.shift
					prefix := (*n.prefixWord() >> n.shift) &^ uint64(65535)
					if projection > prefix|65535 {
						index = int(n.n) - 1
					} else if projection >= prefix {
						index = max(0, upperWordInline(&n.inner().inline, int(n.n), uint16(projection))-1)
						if index > 0 && n.inner().inline[index] == uint16(projection) && n.keys[index] > code {
							index = max(0, lowerCodeScalar(&n.keys, index+1, code+1)-1)
						}
					}
				}
				grand, parentEdge = parent, edge
				parent, edge = n, index
				n = words.node(n.inner().children[index])
			}
			if parent != nil {
				words.hintParent(code, parent, grand, parentEdge)
			}
			i := firstTag(&n.tags, int(n.n), code)
			if i < int(n.n) && n.entries()[i].key != code {
				i = n.lowerEntry(code)
			}
			if i < int(n.n) && n.entries()[i].key == code {
				old := n.entries()[i].value
				n.entries()[i].value = packed
				if words.valueKind == 1 {
					return any(old).(V), true
				}
				return unpackWord[V](old, words.valueKind), true
			}
			if count := int(n.n); count < nodeSize {
				if i == count && count > 0 && code < n.entries()[count-1].key {
					i = n.lowerEntry(code)
				}
				if i > 0 {
					words.inserted = code
					if n.next == 0 {
						words.tail = n
					}
					n.insertLeafPlain(i, code, packed)
					t.length++
					var zero V
					return zero, false
				}
			}
			words.insertNew(n, i, code, packed)
		}
		t.length++
		if t.length >= 2*words.hints.built {
			words.refreshHints(t.length)
		}
		var zero V
		return zero, false
	}
	return t.insertGeneric(key, value)
}

func (t *treeCore[K, V, O]) insertGeneric(key K, value V) (V, bool) {
	if t.generic == nil {
		if t.initWords() {
			return t.Insert(key, value)
		}
		t.generic = &genericTree[K, V, O]{owner: true}
	}
	old, found := t.generic.insert(key, value, insertReplace, nil)
	if !found {
		t.length++
	}
	return old, found
}

// Delete removes an entry, returning its previous value and presence.
func (t *treeCore[K, V, O]) Delete(key K) (V, bool) {
	if t.words != nil {
		old, ok := t.words.deleteOwner(numericKey[O](key))
		if ok {
			t.length--
			return t.wordValue(old), true
		}
		var zero V
		return zero, false
	}
	return t.removeGeneric(seekKey, key)
}

// removeGeneric removes seek's entry from the generic tree, dropping the
// tree once it is empty.
func (t *treeCore[K, V, O]) removeGeneric(seek int, key K) (V, bool) {
	_, v, ok := t.removeEntryGeneric(seek, key)
	return v, ok
}

func (t *treeCore[K, V, O]) removeEntryGeneric(seek int, key K) (K, V, bool) {
	if t.generic == nil {
		var k K
		var v V
		return k, v, false
	}
	k, v, ok := t.generic.remove(seek, key, nil)
	if ok {
		t.length--
		if t.length == 0 {
			t.generic = nil
		}
	}
	return k, v, ok
}

// endGeneric is the generic tree's first entry, or its last.
func (t *treeCore[K, V, O]) endGeneric(last bool) (k K, v V, ok bool) {
	if t.generic != nil {
		t.generic.walk(&Bounds[K]{}, last, func(key K, value V) bool {
			k, v, ok = key, value, true
			return false
		})
	}
	return k, v, ok
}

func (t *treeCore[K, V, O]) First() (K, V, bool) {
	if t.words != nil {
		if id := t.words.end(false); id != 0 {
			n := t.words.node(id)
			i := 0

			return decodeWordKey[K](n.entries()[i].key, t.words.keyKind), t.wordValue(n.entries()[i].value), true
		}
		var k K
		var v V
		return k, v, false
	}
	return t.endGeneric(false)
}
func (t *treeCore[K, V, O]) Last() (K, V, bool) {
	if t.words != nil {
		if id := t.words.end(true); id != 0 {
			n := t.words.node(id)
			i := 0
			i = int(n.n) - 1
			return decodeWordKey[K](n.entries()[i].key, t.words.keyKind), t.wordValue(n.entries()[i].value), true
		}
		var k K
		var v V
		return k, v, false
	}
	return t.endGeneric(true)
}
func (t *treeCore[K, V, O]) PopFirst() (K, V, bool) {
	if t.words == nil {
		var zero K
		return t.removeEntryGeneric(seekFirst, zero)
	}
	k, v, ok := t.First()
	if ok {
		t.Delete(k)
	}
	return k, v, ok
}
func (t *treeCore[K, V, O]) PopLast() (K, V, bool) {
	if t.words == nil {
		var zero K
		return t.removeEntryGeneric(seekLast, zero)
	}
	k, v, ok := t.Last()
	if ok {
		t.Delete(k)
	}
	return k, v, ok
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
	if words := t.words; words != nil {
		low, high, open, empty := wordBounds[K, O](b)
		if !empty {
			words.scan(low, high, open, reverse, func(k, v uint64) bool { return visit(decodeWordKey[K](k, words.keyKind), t.wordValue(v)) })
		}
		return
	}
	if t.generic != nil {
		t.generic.walk(b, reverse, visit)
	}
}

// All iterates all entries in ascending order. Mutation during iteration is forbidden.
func (t *treeCore[K, V, O]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if t.words != nil {
			t.words.all(false, func(k, v uint64) bool { return yield(decodeWordKey[K](k, t.words.keyKind), t.wordValue(v)) })
			return
		}
		if t.generic != nil {
			t.generic.walk(&Bounds[K]{}, false, yield)
		}
	}
}

// Reverse iterates all entries in descending order.
func (t *treeCore[K, V, O]) Reverse() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if t.words != nil {
			t.words.all(true, func(k, v uint64) bool { return yield(decodeWordKey[K](k, t.words.keyKind), t.wordValue(v)) })
			return
		}
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
	if t.words != nil {
		if t.words.keyKind == 1 && t.words.valueKind == 1 {
			t.words.getMany(any(keys).([]uint64), any(dst).([]Result[uint64]))
			return
		}
		for start := 0; start < len(keys); start += batchLanes {
			count := min(batchLanes, len(keys)-start)
			var codes [batchLanes]uint64
			var values [batchLanes]Result[uint64]
			for i := 0; i < count; i++ {
				codes[i] = numericKey[O](keys[start+i])
			}
			t.words.getMany(codes[:count], values[:count])
			for i := 0; i < count; i++ {
				var value V
				if values[i].OK {
					value = t.wordValue(values[i].Value)
				}
				dst[start+i] = Result[V]{value, values[i].OK}
			}
		}
		return
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
