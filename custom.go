package gobbtree

import (
	"iter"
	"math/bits"
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
// Keys are compared while they are stored, so Compare and Window must not
// depend on mutable state. Like a wrong Compare, a Window that breaks the
// contract gives wrong answers but never corrupts memory.
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

// Clone makes an independent tree; values and keys are shallow copies.
func (t *TreeOf[K, V]) Clone() *TreeOf[K, V] { return &TreeOf[K, V]{*t.clone()} }

// Append moves other's entries into t. Duplicate keys keep t's stored key
// and take other's value. Afterwards other is empty.
func (t *TreeOf[K, V]) Append(other *TreeOf[K, V]) { t.appendTree(&other.treeCore) }

// SplitOff moves entries at or above key into a new tree.
func (t *TreeOf[K, V]) SplitOff(key K) *TreeOf[K, V] { return &TreeOf[K, V]{*t.splitOff(key)} }

func (s *SetOf[K]) Clone() *SetOf[K]                         { return &SetOf[K]{*s.clone()} }
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

// A prefix is only a search shortcut; longer shared encodings still search
// correctly, from their first window past the cap.
const customPrefixLimit = 64

func (custom[K]) compare(a, b K) int              { return a.Compare(b) }
func (custom[K]) window(key K, offset int) uint64 { return key.Window(offset) }
func (custom[K]) textual() bool                   { return true }
func (custom[K]) sharedPrefix(a, b K) int {
	for offset := 0; offset < customPrefixLimit; offset += 8 {
		if x, y := a.Window(offset), b.Window(offset); x != y {
			return offset + bits.LeadingZeros64(x^y)/8
		}
	}
	return customPrefixLimit
}
func (custom[K]) comparePrefix(key, first K, prefix int) int {
	for offset := 0; offset < prefix; offset += 8 {
		x, y := key.Window(offset), first.Window(offset)
		if tail := offset + 8 - prefix; tail > 0 {
			x, y = x>>(8*tail), y>>(8*tail)
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// wordKeyKind admits built-in word keys to the numeric arenas, whose codes
// encode the natural order; keys that order themselves never use them.
func wordKeyKind[O keyOrder[K], K any]() uint8 {
	var o O
	if unsafe.Sizeof(o) != 0 {
		return 0
	}
	return wordKind[K]()
}
