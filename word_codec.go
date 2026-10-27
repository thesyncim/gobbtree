package gobbtree

func (t *treeCore[K, V, O]) initWords() bool {
	if t.shared {
		return false
	}
	keyKind, valueKind := wordKeyKind[O, K](), wordKind[V]()
	if keyKind == 0 || keyKind == 11 || keyKind == 12 || keyKind == 13 || valueKind == 0 {
		return false
	}
	t.words = &wordTree{keyKind: keyKind, valueKind: valueKind}
	return true
}
func decodeWordKey[K any](code uint64, kind uint8) K {
	switch kind {
	case 2, 3, 6, 8, 10:
		code ^= 1 << 63
	}
	return unpackWord[K](code, kind)
}
func (t *treeCore[K, V, O]) wordValue(value uint64) V {
	if t.words.valueKind == 1 {
		return any(value).(V)
	}
	return unpackWord[V](value, t.words.valueKind)
}
