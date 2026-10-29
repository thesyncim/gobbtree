//go:build amd64 || arm64

package gobbtree

import (
	"math/bits"
	"unsafe"
)

// loadWord is the eight bytes of s at i, big-endian; 0 <= i, i+8 <= len(s).
// These little-endian targets load unaligned words: one load and a byte
// reversal.
func loadWord(s string, i int) uint64 {
	return bits.ReverseBytes64(*(*uint64)(unsafe.Add(unsafe.Pointer(unsafe.StringData(s)), i)))
}
