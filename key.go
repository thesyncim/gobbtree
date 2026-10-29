package gobbtree

import (
	"cmp"
	"encoding/binary"
	"math"
	"reflect"
	"strings"
	"unsafe"
)

// keyCode is an order-preserving, zero-padded eight-byte window. Equal
// windows are always resolved against full keys (including embedded NULs).
func keyCode[K cmp.Ordered](key K, offset int) uint64 {
	switch k := any(key).(type) {
	case uint64:
		return k
	case uint:
		return uint64(k)
	case uintptr:
		return uint64(k)
	case uint32:
		return uint64(k)
	case uint16:
		return uint64(k)
	case uint8:
		return uint64(k)
	case int:
		return uint64(int64(k)) ^ (1 << 63)
	case int64:
		return uint64(k) ^ (1 << 63)
	case int32:
		return uint64(int64(k)) ^ (1 << 63)
	case int16:
		return uint64(int64(k)) ^ (1 << 63)
	case int8:
		return uint64(int64(k)) ^ (1 << 63)
	case string:
		return stringCode(k, offset)
	case float64:
		return floatCode(k)
	case float32:
		return floatCode(float64(k))
	default:
		// Defined types retain their underlying ordering. Reflection is confined
		// to this fallback; built-in keys do not go through reflect.Value.
		v := reflect.ValueOf(key)
		switch v.Kind() {
		case reflect.String:
			return stringCode(v.String(), offset)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return uint64(v.Int()) ^ (1 << 63)
		case reflect.Float32, reflect.Float64:
			return floatCode(v.Float())
		default:
			return v.Uint()
		}
	}
}

func floatCode(f float64) uint64 {
	if math.IsNaN(f) {
		return 0
	}
	if f == 0 {
		f = 0
	} // cmp.Compare equates -0 and +0.
	u := math.Float64bits(f)
	if u>>63 != 0 {
		return ^u
	}
	return u ^ (1 << 63)
}

func stringCode(s string, offset int) uint64 {
	if len(s) >= 8 && uint(offset) <= uint(len(s)-8) {
		// Eight whole bytes, the common case, inlined: one load.
		return binary.BigEndian.Uint64((*[8]byte)(unsafe.Add(unsafe.Pointer(unsafe.StringData(s)), offset))[:])
	}
	return stringCodeTail(s, offset)
}

// fastWindow is stringCode by one load, inlined, for a string of eight
// bytes or more and an offset inside it: the eight bytes there, or near the
// end the last eight shifted so that the rest lead. ok is false otherwise,
// for stringCode to settle; it calls nothing, so that it inlines.
func fastWindow(s string, offset int) (code uint64, ok bool) {
	if len(s) < 8 || uint(offset) >= uint(len(s)) {
		return 0, false
	}
	at := min(offset, len(s)-8)
	return loadWord(s, at) << (8 * (offset - at)), true
}

// stringCodeTail is stringCode where fewer than eight bytes are left.
func stringCodeTail(s string, offset int) uint64 {
	if offset >= len(s) {
		return 0
	}
	var u uint64
	for i, c := range []byte(s[offset:]) {
		u |= uint64(c) << (56 - 8*i)
	}
	return u
}

func keyString[K cmp.Ordered](k K) (string, bool) {
	if s, ok := any(k).(string); ok {
		return s, true
	}
	// Only string kinds have a string's size, a constant for each shape:
	// other keys skip reflection.
	if unsafe.Sizeof(k) == unsafe.Sizeof("") && reflect.TypeFor[K]().Kind() == reflect.String {
		// A defined string type shares string's representation.
		return *(*string)(unsafe.Pointer(&k)), true
	}
	return "", false
}

// keyOrder supplies the ordering of a container's keys. Built-in ordered keys
// use natural; every operation that compares, windows or prefixes keys goes
// through it, so the search paths are shared by every key family.
type keyOrder[K any] interface {
	compare(a, b K) int
	// window is an order-preserving eight-byte window at a byte offset.
	window(key K, offset int) uint64
	// textual reports keys whose windows may tie for distinct keys; their
	// nodes resolve ties against full keys and skip shared prefixes.
	textual() bool
	// own returns the key to store for a newly inserted key.
	own(key K) K
}

type natural[K cmp.Ordered] struct{}

func (natural[K]) compare(a, b K) int { return cmp.Compare(a, b) }
func (natural[K]) own(key K) K        { return key }
func (natural[K]) window(key K, offset int) uint64 {
	if s, ok := keyString(key); ok {
		return stringCode(s, offset)
	}
	return keyCode(key, offset)
}
func (natural[K]) textual() bool {
	var zero K
	_, ok := keyString(zero)
	return ok
}

// Ordering methods are called through the generic dictionary. natural is the
// only zero-size ordering and strings are the only 16-byte cmp.Ordered keys,
// so these size tests are constants that select direct built-in paths.
func naturalText[O keyOrder[K], K any]() bool {
	var o O
	var k K
	return unsafe.Sizeof(o) == 0 && unsafe.Sizeof(k) == unsafe.Sizeof("")
}

// directText adds the exact Bytes key, whose slice header begins with a
// string header over the same bytes, to the direct textual paths.
func directText[O keyOrder[K], K any]() bool {
	if naturalText[O, K]() {
		return true
	}
	var k K
	if unsafe.Sizeof(k) != unsafe.Sizeof(Bytes(nil)) {
		return false
	}
	_, ok := any(k).(Bytes)
	return ok
}
func textView[K any](key *K) string { return *(*string)(unsafe.Pointer(key)) }

// keyOfText is text s as a text key: a string kind, or Bytes over s's bytes,
// which its holder must not change.
func keyOfText[K any](s string) K {
	var k K
	if unsafe.Sizeof(k) == unsafe.Sizeof("") {
		return *(*K)(unsafe.Pointer(&s))
	}
	b := unsafe.Slice(unsafe.StringData(s), len(s))
	return *(*K)(unsafe.Pointer(&b))
}

func compareKeys[O keyOrder[K], K any](a, b K) int {
	if directText[O, K]() {
		return strings.Compare(textView(&a), textView(&b))
	}
	var o O
	return o.compare(a, b)
}
func ownKey[O keyOrder[K], K any](key K) K {
	if !isCustom[O, K]() {
		return key
	}
	var o O
	return o.own(key)
}
func windowKey[O keyOrder[K], K any](key K, offset int) uint64 {
	if directText[O, K]() {
		return stringCode(textView(&key), offset)
	}
	var o O
	return o.window(key, offset)
}

// numericKey keeps the common uint64 case small enough to inline.
func numericKey[O keyOrder[K], K any](key K) uint64 {
	if value, ok := any(key).(uint64); ok {
		return value
	}
	return windowKey[O](key, 0)
}
