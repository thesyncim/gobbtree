//go:build !goexperiment.simd || !arm64

package gobbtree

// descendText descends an owner's tree from h by text key s, with plain
// loads, to its leaf; or it stops at the internal node where s's summary
// ties a separator's, for the separators to settle.
func descendText(h *gheader, s string) (leaf, tie *gheader) { return descendTextBy(h, s) }
