//go:build !goexperiment.simd || !arm64

package gobbtree

// descendText descends an owner's tree from h by text key s, with plain
// loads, and searches s's leaf by its code: the node it stopped at, the
// slot and what it found there, a text constant.
func descendText(h *gheader, s string) (*gheader, int, int) { return descendTextBy(h, s) }
