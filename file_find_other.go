//go:build !goexperiment.simd || !arm64

package gobbtree

// findWordFast settles nothing where the target has no vectors to count
// windows with in the loop: findWordBy searches.
func (m *FileMap[K, V]) findWordFast(*fileImage, uint64) (keySlot, valueSlot, state int) {
	return 0, 0, wordUnsettled
}

func fastWordPages(*diskLayout) bool { return false }
