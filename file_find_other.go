//go:build !goexperiment.simd || !arm64

package gobbtree

func (m *FileMap[K, V]) findWord(data []byte, root int, word uint64) (keySlot, valueSlot int, ok bool) {
	return m.findWordBy(data, root, word)
}
