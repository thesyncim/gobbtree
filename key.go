package gobbtree

import (
	"cmp"
	"math"
	"reflect"
	"strings"
	"unsafe"
)

// numericCode keeps the common uint64 case small enough to inline into
// numeric lookup and batch paths. All other key types retain the full codec.
func numericCode[K cmp.Ordered](key K) uint64 {
	if value, ok := any(key).(uint64); ok {
		return value
	}
	return keyCode(key, 0)
}

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
	if offset >= len(s) {
		return 0
	}
	s = s[offset:]
	if len(s) >= 8 {
		return uint64(s[0])<<56 | uint64(s[1])<<48 | uint64(s[2])<<40 | uint64(s[3])<<32 | uint64(s[4])<<24 | uint64(s[5])<<16 | uint64(s[6])<<8 | uint64(s[7])
	}
	var u uint64
	for i := 0; i < len(s); i++ {
		u |= uint64(s[i]) << (56 - 8*i)
	}
	return u
}

func keyString[K cmp.Ordered](k K) (string, bool) {
	if s, ok := any(k).(string); ok {
		return s, true
	}
	if reflect.TypeFor[K]().Kind() == reflect.String {
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
	// sharedPrefix is the length of the encoding prefix shared by a and b.
	sharedPrefix(a, b K) int
	// comparePrefix orders key's first prefix encoding bytes against first's.
	comparePrefix(key, first K, prefix int) int
}

type natural[K cmp.Ordered] struct{}

func (natural[K]) compare(a, b K) int { return cmp.Compare(a, b) }
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
func (natural[K]) sharedPrefix(a, b K) int {
	x, ok := keyString(a)
	if !ok {
		return 0
	}
	y, _ := keyString(b)
	end, prefix := min(len(x), len(y), 65535), 0
	for prefix < end && x[prefix] == y[prefix] {
		prefix++
	}
	return prefix
}
func (natural[K]) comparePrefix(key, first K, prefix int) int {
	s, _ := keyString(key)
	f, _ := keyString(first)
	return cmp.Compare(s[:min(len(s), prefix)], f[:prefix])
}

// Ordering methods are called through the generic dictionary. natural is the
// only zero-size ordering and strings are the only 16-byte cmp.Ordered keys,
// so these size tests are constants that select direct built-in paths.
func naturalText[O keyOrder[K], K any]() bool {
	var o O
	var k K
	return unsafe.Sizeof(o) == 0 && unsafe.Sizeof(k) == unsafe.Sizeof("")
}
func textView[K any](key *K) string { return *(*string)(unsafe.Pointer(key)) }

func compareKeys[O keyOrder[K], K any](a, b K) int {
	if naturalText[O, K]() {
		return strings.Compare(textView(&a), textView(&b))
	}
	var o O
	return o.compare(a, b)
}
func windowKey[O keyOrder[K], K any](key K, offset int) uint64 {
	if naturalText[O, K]() {
		return stringCode(textView(&key), offset)
	}
	var o O
	return o.window(key, offset)
}
func comparePrefix[O keyOrder[K], K any](key, first K, prefix int) int {
	if naturalText[O, K]() {
		s, f := textView(&key), textView(&first)
		return strings.Compare(s[:min(len(s), prefix)], f[:prefix])
	}
	var o O
	return o.comparePrefix(key, first, prefix)
}

// numericKey keeps the common uint64 case small enough to inline.
func numericKey[O keyOrder[K], K any](key K) uint64 {
	if value, ok := any(key).(uint64); ok {
		return value
	}
	return windowKey[O](key, 0)
}
