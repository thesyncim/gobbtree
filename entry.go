package gobbtree

// Entry is a key's place in a Tree, occupied or vacant, as upstream's entry
// API gives it. Go cannot lend a reference into a leaf, so where upstream
// returns &mut V an Entry returns the value it stored. An Entry stays valid
// until the tree changes other than through it.
type Entry[K, V any] struct {
	tree     entryTree[K, V]
	key      K
	value    V
	occupied bool
}

type entryTree[K, V any] interface {
	GetKeyValue(K) (K, V, bool)
	Insert(K, V) (V, bool)
	Take(K) (K, V, bool)
}

// Entry returns key's entry, as upstream's entry does.
func (t *treeCore[K, V, O]) Entry(key K) Entry[K, V] {
	e := Entry[K, V]{tree: t, key: key}
	if stored, value, ok := t.GetKeyValue(key); ok {
		e.key, e.value, e.occupied = stored, value, true
	}
	return e
}

// FirstEntry returns the entry of the smallest key, as upstream's
// first_entry does.
func (t *treeCore[K, V, O]) FirstEntry() (Entry[K, V], bool) {
	k, v, ok := t.First()
	return Entry[K, V]{tree: t, key: k, value: v, occupied: ok}, ok
}

// LastEntry returns the entry of the largest key, as upstream's last_entry
// does.
func (t *treeCore[K, V, O]) LastEntry() (Entry[K, V], bool) {
	k, v, ok := t.Last()
	return Entry[K, V]{tree: t, key: k, value: v, occupied: ok}, ok
}

// Occupied reports whether the tree holds the entry's key.
func (e Entry[K, V]) Occupied() bool { return e.occupied }

// Key returns the stored key of an occupied entry, or the key asked for.
func (e Entry[K, V]) Key() K { return e.key }

// Value returns an occupied entry's value, as upstream's get does.
func (e Entry[K, V]) Value() (V, bool) { return e.value, e.occupied }

// OrInsert stores value if the entry is vacant, and returns the entry's
// value, as upstream's or_insert does.
func (e Entry[K, V]) OrInsert(value V) V {
	if !e.occupied {
		e.tree.Insert(e.key, value)
		return value
	}
	return e.value
}

// OrInsertWith stores make() if the entry is vacant, and returns the entry's
// value, as upstream's or_insert_with does.
func (e Entry[K, V]) OrInsertWith(make func() V) V {
	if !e.occupied {
		return e.OrInsert(make())
	}
	return e.value
}

// OrInsertWithKey stores make(key) if the entry is vacant, and returns the
// entry's value, as upstream's or_insert_with_key does.
func (e Entry[K, V]) OrInsertWithKey(make func(K) V) V {
	if !e.occupied {
		return e.OrInsert(make(e.key))
	}
	return e.value
}

// OrDefault stores the zero value if the entry is vacant, and returns the
// entry's value, as upstream's or_default does.
func (e Entry[K, V]) OrDefault() V {
	var zero V
	return e.OrInsert(zero)
}

// AndModify stores change(value) in an occupied entry, as upstream's
// and_modify does, and returns the entry.
func (e Entry[K, V]) AndModify(change func(V) V) Entry[K, V] {
	if e.occupied {
		e.value = change(e.value)
		e.tree.Insert(e.key, e.value)
	}
	return e
}

// Insert stores value in the entry, as upstream's insert does, and returns
// the value it replaced.
func (e Entry[K, V]) Insert(value V) (old V, replaced bool) {
	return e.tree.Insert(e.key, value)
}

// InsertEntry stores value in the entry and returns it occupied, as
// upstream's insert_entry does.
func (e Entry[K, V]) InsertEntry(value V) Entry[K, V] {
	e.tree.Insert(e.key, value)
	e.value, e.occupied = value, true
	return e
}

// Remove takes an occupied entry's key and value out of the tree, as
// upstream's remove_entry does.
func (e Entry[K, V]) Remove() (K, V, bool) {
	if !e.occupied {
		var k K
		var v V
		return k, v, false
	}
	return e.tree.Take(e.key)
}
