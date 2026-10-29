package gobbtree

import (
	"bytes"
	"iter"
	"unsafe"
)

// Key is implemented by key types that define their own order, such as
// composite keys, case-insensitive names or reversed orders.
//
// Window exposes the order to the inline-key search. It returns bytes
// offset..offset+8 of an encoding of the key, big-endian and zero past the
// encoding's end. The encoding must order like Compare: if one key's
// zero-padded encoding sorts before another's, the first key must compare
// less. Distinct keys may share an encoding; ties are settled by Compare.
//
// A key ordered by several fields encodes them one after the other, for
// example a uint32 as four big-endian bytes followed by a string's bytes.
// StringWindow, BytesWindow and Uint64Window window common encodings.
//
// Equal keys must have equal windows. Keys are compared while they are
// stored, so Compare and Window must not depend on mutable state. Like a
// wrong Compare, a Window that breaks the contract gives wrong answers but
// never corrupts memory.
//
// A key that refers to memory its caller may reuse, such as a byte slice,
// also implements Clone() K. Containers then store the clone of each newly
// inserted key and never retain a lookup's key.
type Key[K any] interface {
	Compare(other K) int
	Window(offset int) uint64
}

// TreeOf is Tree for keys that define their own order.
type TreeOf[K Key[K], V any] struct{ treeCore[K, V, custom[K]] }

// MapOf is Map for keys that define their own order.
type MapOf[K Key[K], V any] struct{ mapCore[K, V, custom[K]] }

// SetOf is Set for keys that define their own order.
type SetOf[K Key[K]] struct{ setCore[K, custom[K]] }

func NewTreeOf[K Key[K], V any]() *TreeOf[K, V] { return new(TreeOf[K, V]) }
func NewMapOf[K Key[K], V any]() *MapOf[K, V]   { return new(MapOf[K, V]) }
func NewSetOf[K Key[K]]() *SetOf[K]             { return new(SetOf[K]) }

// Append moves other's entries into t. Duplicate keys keep t's stored key
// and take other's value. Afterwards other is empty.
func (t *TreeOf[K, V]) Append(other *TreeOf[K, V]) { t.appendTree(&other.treeCore) }

// SplitOff moves entries at or above key into a new tree.
func (t *TreeOf[K, V]) SplitOff(key K) *TreeOf[K, V] { return &TreeOf[K, V]{*t.splitOff(key)} }

func (s *SetOf[K]) Append(other *SetOf[K])                   { s.tree.appendTree(&other.tree) }
func (s *SetOf[K]) SplitOff(key K) *SetOf[K]                 { return &SetOf[K]{*s.splitOff(key)} }
func (s *SetOf[K]) Union(other *SetOf[K]) iter.Seq[K]        { return s.merge(&other.setCore, 7) }
func (s *SetOf[K]) Intersection(other *SetOf[K]) iter.Seq[K] { return s.merge(&other.setCore, 4) }
func (s *SetOf[K]) Difference(other *SetOf[K]) iter.Seq[K]   { return s.merge(&other.setCore, 1) }
func (s *SetOf[K]) SymmetricDifference(other *SetOf[K]) iter.Seq[K] {
	return s.merge(&other.setCore, 3)
}
func (s *SetOf[K]) IsSubset(other *SetOf[K]) bool   { return s.isSubset(&other.setCore) }
func (s *SetOf[K]) IsSuperset(other *SetOf[K]) bool { return other.isSubset(&s.setCore) }
func (s *SetOf[K]) IsDisjoint(other *SetOf[K]) bool { return s.isDisjoint(&other.setCore) }

// Bytes is a byte-string key ordered like bytes.Compare; nil and empty are
// the same key. Containers store a copy of each new key, so callers may reuse
// their buffers, and lookups do not allocate. Keys returned by a container
// share its storage and must not be modified.
type Bytes []byte

func (b Bytes) Compare(other Bytes) int  { return bytes.Compare(b, other) }
func (b Bytes) Window(offset int) uint64 { return BytesWindow(b, offset) }
func (b Bytes) Clone() Bytes             { return bytes.Clone(b) }

// StringWindow windows a string's bytes, which order like the string.
func StringWindow(s string, offset int) uint64 { return stringCode(s, offset) }

// BytesWindow windows a byte slice, which orders like bytes.Compare.
func BytesWindow(b []byte, offset int) uint64 {
	return stringCode(unsafe.String(unsafe.SliceData(b), len(b)), offset)
}

// Uint64Window windows v's eight big-endian bytes, which order like v.
func Uint64Window(v uint64, offset int) uint64 {
	if uint(offset) >= 8 {
		return 0
	}
	return v << (8 * offset)
}

// custom orders keys by their Key methods. It is not zero-size, which keeps
// it off the direct built-in paths that natural selects by size.
type custom[K Key[K]] struct{ _ byte }

func (custom[K]) compare(a, b K) int              { return a.Compare(b) }
func (custom[K]) window(key K, offset int) uint64 { return key.Window(offset) }
func (custom[K]) textual() bool                   { return true }
func (custom[K]) own(key K) K {
	if b, ok := any(key).(Bytes); ok {
		clone := b.Clone()
		return *(*K)(unsafe.Pointer(&clone))
	}
	// Test the type with its zero value, so keys without Clone never box.
	var zero K
	if _, ok := any(zero).(interface{ Clone() K }); !ok {
		return key
	}
	return any(key).(interface{ Clone() K }).Clone()
}

// wordKeyKind admits built-in word keys to the numeric arenas, whose codes
// encode the natural order; keys that order themselves never use them.
func wordKeyKind[O keyOrder[K], K any]() uint8 {
	if !wordSized[O, K]() {
		return 0
	}
	return wordKind[K]()
}

// wordSized reports keys that may be word keys: in their natural order and
// no wider than a word. Sizes are constants for each shape, so where it is
// inlined, text keys skip the kinds' type switch, and the code it guards.
func wordSized[O keyOrder[K], K any]() bool {
	return unsafe.Sizeof(*(*O)(nil)) == 0 && unsafe.Sizeof(*(*K)(nil)) <= 8
}

func isCustom[O keyOrder[K], K any]() bool {
	var o O
	return unsafe.Sizeof(o) != 0
}
