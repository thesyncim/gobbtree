//go:build !amd64 && !arm64

package store

import "unsafe"

// noescape is p. Without the assembly that hides p, a point query goes to
// the heap, as a scan's bounds do.
func noescape(p unsafe.Pointer) unsafe.Pointer { return p }
