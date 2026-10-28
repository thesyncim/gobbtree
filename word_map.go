package gobbtree

// wordMap is the Map engine for keys and values that fit a word: a
// latchedTree, and the kinds that decode its words.
type wordMap struct {
	tree               latchedTree
	keyKind, valueKind uint8
}

func (m *wordMap) get(key uint64) (uint64, bool) { return m.tree.get(key) }
func (m *wordMap) getMany8(keys *[batchLanes]uint64, dst *[batchLanes]Result[uint64]) {
	m.tree.getMany8(keys, dst)
}
func (m *wordMap) insert(key, value uint64) (uint64, bool) {
	return m.tree.insert(key, value, insertReplace, nil)
}
func (m *wordMap) updateOrInsert(key, value uint64, merge func(key, old, value uint64) uint64) (uint64, bool) {
	return m.tree.insert(key, value, insertMerge, merge)
}
func (m *wordMap) loadOrStore(key, value uint64) (uint64, bool) {
	if old, found := m.tree.insert(key, value, insertKeep, nil); found {
		return old, true
	}
	return value, false
}
func (m *wordMap) delete(key uint64) (uint64, bool) {
	_, v, ok := m.tree.remove(removeKey, key, nil)
	return v, ok
}
func (m *wordMap) deleteIf(key uint64, predicate func(key, value uint64) bool) (uint64, bool) {
	_, v, ok := m.tree.remove(removeKey, key, predicate)
	return v, ok
}
func (m *wordMap) read(key uint64, visit func(uint64, uint64)) bool { return m.tree.read(key, visit) }
func (m *wordMap) update(key uint64, change func(key, value uint64) uint64) (uint64, bool) {
	return m.tree.update(key, change)
}
func (m *wordMap) walk(lower, upper uint64, reverse, bounded bool, visit func(uint64, uint64) bool) {
	m.tree.walk(lower, upper, reverse, bounded, visit)
}
func (m *wordMap) isEmpty() bool { return m.tree.isEmpty() }

// pop removes the first entry, or the last.
func (m *wordMap) pop(last bool) (uint64, uint64, bool) {
	which := removeFirst
	if last {
		which = removeLast
	}
	return m.tree.remove(which, 0, nil)
}

// clear drops every entry. As upstream's, it is the owner's: no other
// goroutine may use the map meanwhile.
func (m *wordMap) clear() { m.tree = latchedTree{} }
