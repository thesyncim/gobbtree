//go:build !goexperiment.simd || (!amd64 && !arm64)

package gobbtree

func upperWordInline(values *[nodeSize]uint16, n int, key uint16) int {
	return upperWordInlineScalar(values, n, key)
}
