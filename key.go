package gobbtree

import (
	"cmp"
	"math"
	"reflect"
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
	v := reflect.ValueOf(k)
	if v.Kind() == reflect.String {
		return v.String(), true
	}
	return "", false
}
