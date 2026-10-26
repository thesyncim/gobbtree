package gobbtree

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"reflect"
)

func diskCompare[K cmp.Ordered](data []byte, slot int, d diskType, key K) int {
	if d.kind == reflect.String {
		n := int(binary.NativeEndian.Uint32(data[slot:]))
		start := slot + int(binary.NativeEndian.Uint32(data[slot+4:]))
		return bytes.Compare(data[start:start+n], []byte(reflect.ValueOf(key).String()))
	}
	return cmp.Compare(diskRead[K](data, slot, d), key)
}

func diskWindow[K cmp.Ordered](key K, prefix, width int) uint64 {
	v := reflect.ValueOf(key)
	if v.Kind() == reflect.String {
		s := v.String()
		var word uint64
		for i := 0; i < width; i++ {
			word <<= 8
			if prefix+i < len(s) {
				word |= uint64(s[prefix+i])
			}
		}
		return word
	}
	if prefix >= 8 {
		return 0
	}
	var word uint64
	if v.Kind() == reflect.Int64 {
		word = uint64(v.Int()) ^ (1 << 63)
	} else {
		word = v.Uint()
	}
	return word << (prefix * 8) >> (64 - width*8)
}

func (m *FileMap[K, V]) compareShared(key K) int {
	if m.layout.leaf.width == 0 {
		return 0
	}
	start := (m.root &^ 1) + m.layout.area
	n := int(m.data[start])
	if n == 0 {
		return 0
	}
	prefix := m.data[start+1 : start+1+n]
	v := reflect.ValueOf(key)
	if v.Kind() == reflect.String {
		s := v.String()
		// VarBytes uses WithInlineKey: a short encoding is zero-padded,
		// including a one-key map's 64-byte shared prefix.
		for i, b := range prefix {
			var q byte
			if i < len(s) {
				q = s[i]
			}
			if q < b {
				return -1
			}
			if q > b {
				return 1
			}
		}
		return 0
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], diskWindow(key, 0, 8))
	return bytes.Compare(encoded[:min(n, 8)], prefix)
}

func diskInlineRangeScalar(data []byte, width int, target uint64) (int, int) {
	value := func(i int) uint64 {
		slot := i * width
		switch width {
		case 2:
			return uint64(binary.NativeEndian.Uint16(data[slot:]))
		case 4:
			return uint64(binary.NativeEndian.Uint32(data[slot:]))
		default:
			return binary.NativeEndian.Uint64(data[slot:])
		}
	}
	n := len(data) / width
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if value(mid) < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	first := lo
	hi = n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if value(mid) <= target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return first, lo
}
