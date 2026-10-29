package gobbtree

import (
	"iter"
)

// GetKeyValue returns the stored key, its value, and whether it was found.
func (t *treeCore[K, V, O]) GetKeyValue(key K) (stored K, value V, ok bool) {
	if t.generic != nil {
		ok = t.generic.read(key, func(k K, v V) { stored, value = k, v })
		return stored, value, ok
	}
	stored, value, ok = t.LowerBound(key)
	if ok && compareKeys[O](stored, key) == 0 {
		return
	}
	var k K
	var v V
	return k, v, false
}

// LowerBound returns the first entry whose key is at least key.
func (t *treeCore[K, V, O]) LowerBound(key K) (K, V, bool) { return t.bound(key, false) }

// UpperBound returns the first entry whose key is strictly greater than key.
func (t *treeCore[K, V, O]) UpperBound(key K) (K, V, bool) { return t.bound(key, true) }

func (t *treeCore[K, V, O]) bound(key K, after bool) (k K, v V, ok bool) {
	if t.length == 0 {
		return
	}
	if w := t.words; w != nil {
		code := numericKey[O](key)
		n := w.node(w.leafFor(code))
		i := n.lowerEntry(code)
		if after && i < int(n.n) && n.entries()[i].key == code {
			i++
		}
		if i == int(n.n) {
			if n.next == 0 {
				return
			}
			n = w.node(n.next)
			i = 0
		}
		e := n.entries()[i]
		return decodeWordKey[K](e.key, w.keyKind), t.wordValue(e.value), true
	}
	if t.generic != nil {
		b := AtLeast(key)
		if after {
			b = Above(key)
		}
		t.generic.walk(&b, false, func(key K, value V) bool {
			k, v, ok = key, value, true
			return false
		})
	}
	return k, v, ok
}

func (t *treeCore[K, V, O]) Keys() iter.Seq[K] {
	return func(yield func(K) bool) {
		for k := range t.All() {
			if !yield(k) {
				return
			}
		}
	}
}

func (t *treeCore[K, V, O]) Values() iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, v := range t.All() {
			if !yield(v) {
				return
			}
		}
	}
}

// Append moves other's entries into t. Duplicate keys keep t's stored key
// and take other's value. Afterwards other is empty. Appending to self is a no-op.
func (t *Tree[K, V]) Append(other *Tree[K, V]) { t.appendTree(&other.treeCore) }

func (t *treeCore[K, V, O]) appendTree(other *treeCore[K, V, O]) {
	if t == other || other.IsEmpty() {
		return
	}
	if t.IsEmpty() {
		*t = *other
		other.Clear()
		return
	}
	for k, v := range other.All() {
		t.Insert(k, v)
	}
	other.Clear()
}

// SplitOff moves entries at or above key into a new tree. This implementation
// moves boundary entries individually; it does not share nodes between owners.
func (t *Tree[K, V]) SplitOff(key K) *Tree[K, V] { return &Tree[K, V]{*t.splitOff(key)} }

func (t *treeCore[K, V, O]) splitOff(key K) *treeCore[K, V, O] {
	out := new(treeCore[K, V, O])
	if first, _, ok := t.First(); ok && compareKeys[O](first, key) >= 0 {
		*out = *t
		t.Clear()
		return out
	}
	for {
		k, v, ok := t.LowerBound(key)
		if !ok {
			return out
		}
		out.Insert(k, v)
		t.Delete(k)
	}
}

// Retain visits entries in order and removes those rejected by keep.
// The callback must not modify t. A panic preserves the unvisited entries.
func (t *treeCore[K, V, O]) Retain(keep func(K, V) bool) {
	for k, v, ok := t.First(); ok; k, v, ok = t.UpperBound(k) {
		if !keep(k, v) {
			t.Delete(k)
		}
	}
}

// ExtractIf removes and yields the entries in b that predicate accepts, as
// upstream's extract_if does. Stopping iteration leaves the remaining entries
// unvisited and unchanged. Like upstream's, it never panics on its bounds: a
// lower end above the upper one extracts nothing.
func (t *treeCore[K, V, O]) ExtractIf(b Bounds[K], predicate func(K, V) bool) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		var k K
		var v V
		var ok bool
		switch b.lowerKind {
		case unbounded:
			k, v, ok = t.First()
		case included:
			k, v, ok = t.LowerBound(b.lower)
		case excluded:
			k, v, ok = t.UpperBound(b.lower)
		}
		for ; ok && belowUpper[K, O](&b, k); k, v, ok = t.UpperBound(k) {
			if predicate(k, v) {
				t.Delete(k)
				if !yield(k, v) {
					return
				}
			}
		}
	}
}

// Update replaces key's value with change(stored key, value), as upstream's
// get_mut allows, and returns the new value. If change panics, the stored
// value is unchanged.
func (t *treeCore[K, V, O]) Update(key K, change func(K, V) V) (value V, ok bool) {
	stored, value, ok := t.GetKeyValue(key)
	if !ok {
		return
	}
	value = change(stored, value)
	t.Insert(key, value)
	return
}

func (t *treeCore[K, V, O]) LoadOrStore(key K, value V) (V, bool) {
	if current, ok := t.Get(key); ok {
		return current, true
	}
	t.Insert(key, value)
	return value, false
}

// Take removes key, returning the stored key and its value, as upstream's
// remove_entry does.
func (t *treeCore[K, V, O]) Take(key K) (K, V, bool) {
	stored, value, ok := t.GetKeyValue(key)
	if ok {
		t.Delete(key)
	}
	return stored, value, ok
}

// UpdateOrInsert inserts value if key is absent, and otherwise stores
// merge(stored key, old, value), as upstream's entry and_modify/or_insert
// does. It returns the old value and whether key was present.
func (t *treeCore[K, V, O]) UpdateOrInsert(key K, value V, merge func(key K, old, value V) V) (V, bool) {
	stored, old, ok := t.GetKeyValue(key)
	if ok {
		value = merge(stored, old, value)
	}
	t.Insert(key, value)
	return old, ok
}

// LoadOrStoreFunc returns key's value if present, and otherwise stores and
// returns make(), as upstream's entry or_insert_with does.
func (t *treeCore[K, V, O]) LoadOrStoreFunc(key K, make func() V) (V, bool) {
	if v, ok := t.Get(key); ok {
		return v, true
	}
	v := make()
	t.Insert(key, v)
	return v, false
}

// UpdateRange replaces each value with keys in b by change(key, value), in
// key order, as upstream's range_mut allows; like range_mut, it panics on
// bounds upstream rejects unless the tree is empty.
func (t *treeCore[K, V, O]) UpdateRange(b Bounds[K], change func(K, V) V) {
	if t.length != 0 {
		checkBounds[K, O](&b)
	}
	// Replacing a value never moves an entry, so the walk stays valid.
	t.scan(&b, false, func(k K, v V) bool { t.Insert(k, change(k, v)); return true })
}

// UpdateAll replaces every value by change(key, value), in key order, as
// upstream's iter_mut and values_mut allow.
func (t *treeCore[K, V, O]) UpdateAll(change func(K, V) V) {
	t.scan(&Bounds[K]{}, false, func(k K, v V) bool { t.Insert(k, change(k, v)); return true })
}

// InsertAll inserts every entry of seq, as upstream's Extend does.
func (t *treeCore[K, V, O]) InsertAll(seq iter.Seq2[K, V]) {
	for k, v := range seq {
		t.Insert(k, v)
	}
}
