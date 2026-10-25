package gobbtree

import (
	"fmt"
	"testing"
)

func mixedKey(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

var benchmarkSink uint64

func BenchmarkLookup(b *testing.B) {
	for _, size := range []int{4096, 262144} {
		tr := NewTree[uint64, uint64]()
		m := New[uint64, uint64]()
		for i := 0; i < size; i++ {
			k := mixedKey(uint64(i))
			tr.Insert(k, k)
			m.Insert(k, k)
		}
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
			b.Run(fmt.Sprintf("Tree/%s/%d", label, size), func(b *testing.B) {
				b.ReportAllocs()
				var sum uint64
				index := 0
				for b.Loop() {
					v, _ := tr.Get(queries[index&8191])
					sum += v
					index++
				}
				benchmarkSink = sum
			})
			b.Run(fmt.Sprintf("Map/%s/%d", label, size), func(b *testing.B) {
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
	}
}
func BenchmarkBatch(b *testing.B) {
	tr := NewTree[uint64, uint64]()
	const size = 262144
	for i := 0; i < size; i++ {
		k := mixedKey(uint64(i))
		tr.Insert(k, k)
	}
	var keys [64]uint64
	var dst [64]Result[uint64]
	for i := range keys {
		keys[i] = mixedKey(mixedKey(uint64(i+17)) % size)
	}
	b.ReportAllocs()
	var sum uint64
	for b.Loop() {
		tr.GetMany(keys[:], dst[:])
		sum += dst[sum%64].Value
	}
	benchmarkSink = sum
}
func BenchmarkSearch(b *testing.B) {
	var keys [nodeSize]uint64
	for i := range keys {
		keys[i] = uint64(i) * 2
	}
	b.Run("selected", func(b *testing.B) {
		var sum uint64
		var q uint64
		for b.Loop() {
			q = mixedKey(q + 1)
			sum += uint64(lowerCode(&keys, nodeSize, q&63))
		}
		benchmarkSink = sum
	})
	b.Run("binary", func(b *testing.B) {
		var sum uint64
		var q uint64
		for b.Loop() {
			q = mixedKey(q + 1)
			sum += uint64(lowerCodeScalar(&keys, nodeSize, q&63))
		}
		benchmarkSink = sum
	})
}
func BenchmarkReplace(b *testing.B) {
	tr := NewTree[uint64, uint64]()
	for i := 0; i < 4096; i++ {
		tr.Insert(mixedKey(uint64(i)), uint64(i))
	}
	var sum uint64
	var i uint64
	b.ReportAllocs()
	for b.Loop() {
		v, _ := tr.Insert(mixedKey(i&4095), i)
		sum += v
		i++
	}
	benchmarkSink = sum
}
func BenchmarkScan64(b *testing.B) {
	tr := NewTree[uint64, uint64]()
	for i := uint64(0); i < 262144; i++ {
		tr.Insert(i, i)
	}
	var sum uint64
	var start uint64
	b.ReportAllocs()
	for b.Loop() {
		start = mixedKey(start) % 262080
		tr.Scan(start, start+64, func(k, v uint64) bool { sum += v; return true })
	}
	benchmarkSink = sum
}
func BenchmarkStringLookup(b *testing.B) {
	tr := NewTree[string, uint64]()
	keys := make([]string, 65536)
	for i := range keys {
		keys[i] = fmt.Sprintf("/tenant/long/shared/prefix/records/%016x", mixedKey(uint64(i)))
		tr.Insert(keys[i], uint64(i))
	}
	b.ReportAllocs()
	var sum uint64
	index := 0
	for b.Loop() {
		v, _ := tr.Get(keys[mixedKey(uint64(index))&65535])
		sum += v
		index++
	}
	benchmarkSink = sum
}
