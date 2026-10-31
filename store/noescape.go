//go:build amd64 || arm64

package store

import "unsafe"

// noescape is p, through a call escape analysis can't see into, as the
// runtime's noescape: what p points to may stay in its frame (pointQuery).
//
//go:noescape
func noescape(p unsafe.Pointer) unsafe.Pointer
