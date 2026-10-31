//go:build !goexperiment.simd || (!amd64 && !arm64)

package gobbtree

func diskInlineRange(data []byte, width int, target uint64) (int, int) {
	return diskInlineRangeScalar(data, width, target)
}

func diskSlotRange(slots []byte, width, n int, target uint64) (int, int) {
	return diskInlineRange(slots[:n*width], width, target)
}
