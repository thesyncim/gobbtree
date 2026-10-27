//go:build !arm64

package gobbtree

// loadFence orders earlier loads before later ones. amd64 already keeps
// loads in order; other targets do not use plain optimistic reads.
func loadFence() {}
