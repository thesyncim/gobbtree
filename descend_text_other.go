//go:build !goexperiment.simd || !arm64

package gobbtree

// descendText descends an owner's tree from h by text key s, with plain
// loads, and searches s's leaf by its code: the node it stopped at, the
// slot and what it found there, a text constant.
func descendText(h *gheader, s string) (*gheader, int, int) { return descendTextBy(h, s) }

// descendTextOrdered descends a racing reader's text tree by s, as
// textLeafOrdered does, and searches the leaf by its code, a key that fits
// its windows and lacks its prefix being absent: the leaf, its latch word, the slot, what it found there, and the
// words read of the leaf, for the caller's closing check; false where it
// can't settle.
func descendTextOrdered(root **gheader, s string) (*gheader, uint64, int, int, uint64, bool) {
	return descendTextOrderedBy(root, s)
}

// hintedCode settles code through r's hint in an owner's tree, with plain
// loads: the leaf the hint names, code's slot there and whether code is
// there, settled where the leaf holds it or it lies between the leaf's
// first key and its last. Where place is true, an absent code's slot is
// the one it would take.
func hintedCode(r *codeHints, d *leafDir, code uint64, place bool) (*gheader, int, bool, bool) {
	return hintedCodeBy(r, d, code, place)
}

// descendCode descends an owner's tree from h by code, the code of a key
// that codes order exactly, with plain loads, and searches its leaf: the
// leaf, its slot for code, and whether code is there (seekCode).
func descendCode(h *gheader, code uint64, place bool) (*gheader, int, bool) {
	return descendCodeBy(h, code, place)
}
