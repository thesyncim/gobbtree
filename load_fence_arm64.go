package gobbtree

import "sync/atomic"

// loadFence orders every earlier load before every later one. A plain load
// may complete after a later acquire load on arm64; a value-returning
// read-modify-write is a full barrier there (LDADDAL). Its word is private,
// so no cache line is shared between readers.
func loadFence() {
	var private atomic.Uint32
	private.Add(1)
}
