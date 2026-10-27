package gobbtree

import (
	"iter"
)

// GetKeyValue returns the stored key, its value, and whether it was found.
func (t *treeCore[K, V, O]) GetKeyValue(key K) (stored K, value V, ok bool) {
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
	n := t.root.findLeaf(key)
	i := n.lower(key)
	if after && i < n.n && compareKeys[O](n.keys[i], key) == 0 {
		i++
	}
	if i == n.n {
		n = n.leaf.next
		if n == nil {
			return
		}
		i = 0
	}
	return n.keys[i], n.leaf.values[i], true
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

// ExtractIf removes and yields matching entries in [lower,upper). Stopping
// iteration leaves the remaining entries unvisited and unchanged.
func (t *treeCore[K, V, O]) ExtractIf(lower, upper K, predicate func(K, V) bool) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if compareKeys[O](lower, upper) >= 0 {
			return
		}
		for k, v, ok := t.LowerBound(lower); ok && compareKeys[O](k, upper) < 0; k, v, ok = t.UpperBound(k) {
			if predicate(k, v) {
				t.Delete(k)
				if !yield(k, v) {
					return
				}
			}
		}
	}
}

// Update changes an existing value and returns the value that was stored.
// If change panics, the stored value is unchanged.
func (t *treeCore[K, V, O]) Update(key K, change func(V) V) (value V, ok bool) {
	value, ok = t.Get(key)
	if !ok {
		return
	}
	value = change(value)
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
