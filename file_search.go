package gobbtree

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"reflect"
	"strings"
	"unsafe"
)

// compareAt compares the key in slot with key, as cmp.Compare does.
func (m *FileMap[K, V]) compareAt(data []byte, slot int, key K) int {
	if unsafe.Sizeof(key) == unsafe.Sizeof("") {
		// The only ordered keys of a string's size are strings.
		n := int(binary.NativeEndian.Uint32(data[slot:]))
		start := slot + int(binary.NativeEndian.Uint32(data[slot+4:]))
		stored := data[start : start+n]
		return strings.Compare(unsafe.String(unsafe.SliceData(stored), n), *(*string)(unsafe.Pointer(&key)))
	}
	return cmp.Compare(diskReadPlain[K](data, slot, m.layout.key.size), key)
}

// keyWord is an integer key of 8 bytes as the big-endian word that orders
// its encoding: its own bits, with a signed key's sign bit flipped.
func (m *FileMap[K, V]) keyWord(key K) uint64 {
	if unsafe.Sizeof(key) != 8 {
		return 0
	}
	word := *(*uint64)(unsafe.Pointer(&key))
	if m.layout.key.kind == reflect.Int64 {
		word ^= 1 << 63
	}
	return word
}

// window is key's inline key: width bytes of its encoding from prefix on,
// zero past its end, as the writer's diskInline puts them.
func (m *FileMap[K, V]) window(key K, prefix, width int) uint64 {
	if unsafe.Sizeof(key) == unsafe.Sizeof("") {
		s := *(*string)(unsafe.Pointer(&key))
		if prefix >= 0 && prefix <= len(s)-8 {
			b := unsafe.Slice(unsafe.StringData(s), len(s))
			return binary.BigEndian.Uint64(b[prefix:]) >> (64 - width*8)
		}
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
	return m.keyWord(key) << (prefix * 8) >> (64 - width*8)
}

// compareShared compares key with the prefix every key of the map begins
// with: 0 if it begins with it too, else the order of the two.
func (m *FileMap[K, V]) compareShared(data []byte, root int, key K) int {
	if m.layout.leaf.width == 0 {
		return 0
	}
	start := (root &^ 1) + m.layout.area
	n := int(data[start])
	if n == 0 {
		return 0
	}
	prefix := data[start+1 : start+1+n]
	if unsafe.Sizeof(key) == unsafe.Sizeof("") {
		// VarBytes uses WithInlineKey: a short encoding is zero-padded,
		// including a one-key map's 64-byte shared prefix.
		s := *(*string)(unsafe.Pointer(&key))
		k := min(len(s), n)
		if c := bytes.Compare(unsafe.Slice(unsafe.StringData(s), k), prefix[:k]); c != 0 {
			return c
		}
		for _, b := range prefix[k:] {
			if b != 0 {
				return -1
			}
		}
		return 0
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], m.keyWord(key))
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
