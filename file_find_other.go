//go:build !goexperiment.simd || !arm64

package gobbtree

import "reflect"

// findWordFast is findWordBy where the target has no vectors to count
// windows with in the loop, as descendCode is descendCodeBy there: it
// settles every lookup, and panics on a corrupt map as findWordBy does.
func (m *FileMap[K, V]) findWordFast(img *fileImage, word uint64) (keySlot, valueSlot, state int) {
	keySlot, valueSlot, ok := m.findWordBy(img.data, img.root, word)
	if ok {
		return keySlot, valueSlot, wordFound
	}
	return keySlot, valueSlot, wordAbsent
}

// fastWordPages tells layouts findWordBy searches, as findWordFast does.
func fastWordPages(l *diskLayout) bool { return l.wordPages }

// fastTextPages tells layouts findTextBy searches, as findTextFast does.
func fastTextPages(l *diskLayout) bool {
	return l.key.kind == reflect.String && l.leaf.width == 8 && l.internal.width == 4 &&
		l.leaf.capacity >= 8 && l.internal.capacity >= 8 && l.leaf.headKey >= 0
}

// findTextFast is findTextBy where the target has no vectors to count
// windows with in the loop.
func (m *FileMap[K, V]) findTextFast(img *fileImage, s string) (keySlot, valueSlot, state int) {
	keySlot, valueSlot, ok := m.findTextBy(img, s)
	if ok {
		return keySlot, valueSlot, wordFound
	}
	return keySlot, valueSlot, wordAbsent
}
