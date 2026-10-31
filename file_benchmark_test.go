package gobbtree

import (
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
)

// fileMapOf writes keys, sorted, with themselves as values, and opens the
// snapshot.
func fileMapOf[K string | uint64](b *testing.B, keys []K) *FileMap[K, uint64] {
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	w, err := NewFileWriter(func(yield func(K, uint64) bool) {
		for i, k := range sorted {
			if !yield(k, uint64(i)) {
				return
			}
		}
	})
	if err != nil {
		b.Fatal(err)
	}
	m, err := OpenFileMap[K, uint64](w.Bytes())
	if err != nil {
		b.Fatal(err)
	}
	return m
}

func BenchmarkFileMap(b *testing.B) {
	for _, size := range []int{4096, 1 << 20} {
		keys := make([]uint64, size)
		for i := range keys {
			keys[i] = mixedKey(uint64(i))
		}
		m := fileMapOf(b, keys)
		queries := make([]uint64, 8192)
		for _, hit := range []bool{true, false} {
			label := "miss"
			if hit {
				label = "hit"
			}
			for i := range queries {
				x := mixedKey(uint64(i+17)) % uint64(size)
				if !hit {
					x += uint64(size)
				}
				queries[i] = mixedKey(x)
			}
			b.Run(fmt.Sprintf("uint64/%s/%d", label, size), func(b *testing.B) {
				b.ReportAllocs()
				var sum uint64
				index := 0
				for b.Loop() {
					v, _ := m.Get(queries[index&8191])
					sum += v
					index++
				}
				benchmarkSink = sum
			})
		}
		b.Run(fmt.Sprintf("uint64/parallel/%d", size), func(b *testing.B) {
			for i := range queries {
				queries[i] = mixedKey(mixedKey(uint64(i+17)) % uint64(size))
			}
			var total, worker atomic.Uint64
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				index := worker.Add(1) * 113
				var sum uint64
				for pb.Next() {
					v, _ := m.Get(queries[index&8191])
					sum += v
					index++
				}
				total.Add(sum)
			})
			benchmarkSink = total.Load()
		})
	}
	for _, length := range []int{16, 48} {
		keys := make([]string, 65536)
		for i := range keys {
			keys[i] = fmt.Sprintf("%0*x", length, mixedKey(uint64(i)))
		}
		m := fileMapOf(b, keys)
		// Clones, so that no compare finds the stored string's own bytes.
		queries := make([]string, len(keys))
		for i := range queries {
			queries[i] = string([]byte(keys[mixedKey(uint64(i))&65535]))
		}
		b.Run(fmt.Sprintf("string%d/hit/65536", length), func(b *testing.B) {
			b.ReportAllocs()
			var sum uint64
			index := 0
			for b.Loop() {
				v, _ := m.Get(queries[index&65535])
				sum += v
				index++
			}
			benchmarkSink = sum
		})
	}
}
