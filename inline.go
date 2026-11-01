package gobbtree

import (
	"fmt"
	"reflect"
)

// InlineWidth is how many bytes of each window a map's nodes keep of a
// custom key, upstream's InlineWidth: the first 2, 4, or all 8, the
// default. Narrower windows tie more often, and a tie compares the keys.
type InlineWidth int

// The widths a key may declare.
const (
	InlineWidthTwo   InlineWidth = 2
	InlineWidthFour  InlineWidth = 4
	InlineWidthEight InlineWidth = 8
)

// The rest of upstream's InlineKey: methods a custom key type may add to
// Compare and Window, which its maps then use, as upstream's impls declare
// WIDTH, HAS_HASH_TAG and HAS_CMP_FROM and provide hash_tag, cmp_from and
// cmp_prefix. A map finds them once, as it makes its root, and calls each
// directly, as a function of the key type's method set.
type (
	// WidthKey declares how many bytes of each window the nodes keep,
	// upstream's WIDTH. It is called once, on the key type's zero value.
	WidthKey interface{ InlineWidth() InlineWidth }

	// HashTagKey's HashTag is a byte that equal keys share, upstream's
	// hash_tag: leaves keep each key's, and a lookup whose window ties
	// with several keys of a leaf compares only those with its tag. A tag
	// that differs between equal keys gives wrong answers.
	HashTagKey interface{ HashTag() uint8 }

	// CompareFromKey's CompareFrom orders the key against other as
	// Compare does, given that the first equalLen bytes of their
	// encodings, zero-padded, are equal, upstream's cmp_from: the map calls
	// it where their windows tie, with what the node's keys share and the
	// bytes of the windows it keeps, so that a key reads its bytes from
	// where keys can differ. Internal nodes then keep 4 bytes of 8-byte
	// windows, as for strings: their extra ties cost little. A lookup that
	// races a writer may pass an equalLen that a change makes untrue, and
	// discard the answer: CompareFrom must return without panicking, or
	// reading past either key, whatever equalLen is.
	CompareFromKey[K any] interface {
		CompareFrom(other K, equalLen int) int
	}

	// ComparePrefixKey's ComparePrefix orders the key's encoding, padded
	// with zeros, against prefix in its first len(prefix) bytes, upstream's
	// cmp_prefix: a search checks its key against what every key of the
	// map shares, at most 56 bytes, once, before the root. prefix is the
	// map's: it must not be kept or changed. Without it, the map compares
	// windows.
	ComparePrefixKey interface{ ComparePrefix(prefix []byte) int }
)

// inlineOptions is what a custom key type's methods ask of its nodes: the
// widths its leaves and internal nodes keep, and the optional methods, each
// a function of the key type's method set, which the tree calls directly
// rather than through its dictionary.
type inlineOptions[K any] struct {
	leafWidth, innerWidth int
	hashTag               func(K) uint8
	compareFrom           func(K, K, int) int
	comparePrefix         func(K, []byte) int
	// clone is the key's Clone, which a map stores each new key through,
	// found once rather than asserted at every insert.
	clone func(K) K
	// window is the key's Window, called directly: through the map's key
	// order it is three calls, none inlined.
	window func(K, int) uint64
}

// inlineOptionsOf finds custom key type K's optional methods. A method of
// one of their names but another signature is a mistake it reports.
func inlineOptionsOf[K any]() inlineOptions[K] {
	o := inlineOptions[K]{leafWidth: 8, innerWidth: 8}
	kt := reflect.TypeFor[K]()
	if kt.Kind() == reflect.Interface {
		return o
	}
	find := func(name string, want reflect.Type) reflect.Value {
		m, ok := kt.MethodByName(name)
		if !ok {
			return reflect.Value{}
		}
		if m.Type != want {
			panic(fmt.Sprintf("gobbtree: %v's %s is %v, not %v", kt, name, m.Type, want))
		}
		return m.Func
	}
	if f := find("HashTag", reflect.TypeFor[func(K) uint8]()); f.IsValid() {
		o.hashTag = f.Interface().(func(K) uint8)
	}
	if f := find("CompareFrom", reflect.TypeFor[func(K, K, int) int]()); f.IsValid() {
		o.compareFrom = f.Interface().(func(K, K, int) int)
	}
	if f := find("ComparePrefix", reflect.TypeFor[func(K, []byte) int]()); f.IsValid() {
		o.comparePrefix = f.Interface().(func(K, []byte) int)
	}
	if m, ok := kt.MethodByName("Window"); ok && m.Type == reflect.TypeFor[func(K, int) uint64]() {
		o.window = m.Func.Interface().(func(K, int) uint64)
	}
	// A Clone of another signature is no Key's Clone, as before: not a
	// mistake.
	if m, ok := kt.MethodByName("Clone"); ok && m.Type == reflect.TypeFor[func(K) K]() {
		o.clone = m.Func.Interface().(func(K) K)
	}
	if f := find("InlineWidth", reflect.TypeFor[func(K) InlineWidth]()); f.IsValid() {
		var zero K
		switch w := f.Interface().(func(K) InlineWidth)(zero); w {
		case InlineWidthTwo, InlineWidthFour, InlineWidthEight:
			o.leafWidth, o.innerWidth = int(w), int(w)
		default:
			panic(fmt.Sprintf("gobbtree: %v's InlineWidth is %d, not 2, 4 or 8", kt, w))
		}
	}
	// As upstream's node plan does: a key that compares from where keys
	// differ keeps 4 bytes of 8-byte windows in internal nodes.
	if o.compareFrom != nil && o.innerWidth == 8 {
		o.innerWidth = 4
	}
	return o
}

// narrowed is code cut to the w bytes a node of that width keeps, its top.
func narrowed(code uint64, w int) uint64 { return code >> (64 - 8*uint(w&15)) }

// equalThrough is how many leading bytes keys whose codes, at prefix p and
// w bytes wide, are equal share: windows below 8 bytes of prefix are a
// key's first, past the prefix (windowIn).
func equalThrough(p, w int) int {
	if p < 8 {
		return min(p+w, 8)
	}
	return p + w
}
