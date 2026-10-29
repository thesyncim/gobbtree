//go:build !amd64 && !arm64

package gobbtree

import (
	"encoding/binary"
	"unsafe"
)

// loadWord is the eight bytes of s at i, big-endian; 0 <= i, i+8 <= len(s).
func loadWord(s string, i int) uint64 {
	return binary.BigEndian.Uint64((*[8]byte)(unsafe.Add(unsafe.Pointer(unsafe.StringData(s)), i))[:])
}
