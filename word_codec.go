package gobbtree

import "math"

func (t *treeCore[K, V, O]) initWords() bool {
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

func wordKind[V any]() uint8 {
	var zero V
	switch any(zero).(type) {
	case uint64:
		return 1
	case int:
		return 2
	case int64:
		return 3
	case uint:
		return 4
	case uint32:
		return 5
	case int32:
		return 6
	case uint16:
		return 7
	case int16:
		return 8
	case uint8:
		return 9
	case int8:
		return 10
	case float64:
		return 11
	case float32:
		return 12
	case bool:
		return 13
	case uintptr:
		return 14
	default:
		return 0
	}
}

// Keep the common conversion within the inlining budget.
func packWord[V any](value V) uint64 {
	if word, ok := any(value).(uint64); ok {
		return word
	}
	return packWordOther(value)
}
func packWordOther[V any](value V) uint64 {
	switch v := any(value).(type) {
	case uint64:
		return v
	case int:
		return uint64(v)
	case int64:
		return uint64(v)
	case uint:
		return uint64(v)
	case uint32:
		return uint64(v)
	case int32:
		return uint64(v)
	case uint16:
		return uint64(v)
	case int16:
		return uint64(v)
	case uint8:
		return uint64(v)
	case int8:
		return uint64(v)
	case float64:
		return math.Float64bits(v)
	case float32:
		return uint64(math.Float32bits(v))
	case bool:
		if v {
			return 1
		}
		return 0
	case uintptr:
		return uint64(v)
	default:
		panic("gobbtree: non-word value in atomic value path")
	}
}
func unpackWord[V any](word uint64, kind uint8) V {
	// Each assertion is guaranteed by the kind selected from V at construction.
	switch kind {
	case 1:
		return any(word).(V)
	case 2:
		return any(int(word)).(V)
	case 3:
		return any(int64(word)).(V)
	case 4:
		return any(uint(word)).(V)
	case 5:
		return any(uint32(word)).(V)
	case 6:
		return any(int32(word)).(V)
	case 7:
		return any(uint16(word)).(V)
	case 8:
		return any(int16(word)).(V)
	case 9:
		return any(uint8(word)).(V)
	case 10:
		return any(int8(word)).(V)
	case 11:
		return any(math.Float64frombits(word)).(V)
	case 12:
		return any(math.Float32frombits(uint32(word))).(V)
	case 13:
		return any(word != 0).(V)
	case 14:
		return any(uintptr(word)).(V)
	default:
		panic("gobbtree: invalid atomic value kind")
	}
}
