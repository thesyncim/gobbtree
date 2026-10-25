//go:build goexperiment.simd && amd64

package gobbtree

import (
	"os"
	"simd"
	"simd/archsimd"
	"strconv"
	"testing"
)

func TestSIMDAMD64Dispatch(t *testing.T) {
	hardware, width := SIMD()
	t.Logf("hardware=%v width=%d portableWidth=%d emulated=%v AVX=%v AVX2=%v AVX512=%v GODEBUG=%q", hardware, width, simd.VectorBitSize(), simd.Emulated(), archsimd.X86.AVX(), archsimd.X86.AVX2(), archsimd.X86.AVX512(), os.Getenv("GODEBUG"))
	if simd.Emulated() {
		if hardware || width != 0 {
			t.Fatal("emulation selected a hardware kernel")
		}
	}
	if text := os.Getenv("GOBBTREE_EXPECT_WIDTH"); text != "" {
		want, err := strconv.Atoi(text)
		if err != nil {
			t.Fatal(err)
		}
		if width != want || hardware != (want != 0) {
			t.Fatalf("backend width=%d hardware=%v, expected %d", width, hardware, want)
		}
	}
}
