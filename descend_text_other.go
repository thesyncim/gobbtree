//go:build !goexperiment.simd || !arm64

package gobbtree

// descendText descends an owner's tree from h by text key s, with plain
// loads, and searches s's leaf by its code: the node it stopped at, the
// slot and what it found there, a text constant.
func descendText(h *gheader, s string) (*gheader, int, int) { return descendTextBy(h, s) }

// descendCode descends an owner's tree from h by code, the code of a key
// that codes order exactly, with plain loads, and searches its leaf: the
// leaf, its slot for code, and whether code is there (seekCode).
func descendCode(h *gheader, code uint64, place bool) (*gheader, int, bool) {
	return descendCodeBy(h, code, place)
}
