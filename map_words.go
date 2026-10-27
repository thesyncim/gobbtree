package gobbtree

import (
	"runtime"
)

// Native benchmarks favor these conversion forms on their respective targets.
const checkedMapWordValues = runtime.GOARCH == "arm64"

func (m *mapCore[K, V, O]) numericMap() *wordMap {
	if words := m.words.Load(); words != nil {
		return words
	}
	if m.wordDisabled.Load() {
		return nil
	}
	return m.initNumericMap()
}
func (m *mapCore[K, V, O]) initNumericMap() *wordMap {
	keyKind, valueKind := wordKeyKind[O, K](), wordKind[V]()
	if keyKind == 0 || keyKind == 11 || keyKind == 12 || keyKind == 13 || valueKind == 0 {
		m.wordDisabled.Store(true)
		return nil
	}
	m.structure.Lock()
	defer m.structure.Unlock()
	if words := m.words.Load(); words != nil {
		return words
	}
	words := &wordMap{structure: &m.structure, count: &m.count, keyKind: keyKind, valueKind: valueKind, tree: wordTree{shared: true, keyKind: keyKind, valueKind: valueKind}}
	m.words.Store(words)
	return words
}
func mapWordValue[V any](word uint64, found bool, kind uint8) (V, bool) {
	if found {
		if kind == 1 {
			return any(word).(V), true
		}
		return unpackWord[V](word, kind), true
	}
	var zero V
	return zero, false
}
func mapWordEntry[K any, V any](key, value uint64, found bool, words *wordMap) (K, V, bool) {
	if found {
		return decodeWordKey[K](key, words.keyKind), unpackWord[V](value, words.valueKind), true
	}
	var k K
	var v V
	return k, v, false
}
