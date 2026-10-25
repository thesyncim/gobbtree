package gobbtree

import (
	"sync/atomic"
	"testing"
)

func BenchmarkParallelGet(b *testing.B) {
	m := New[uint64, uint64]()
	for i := uint64(0); i < 262144; i++ {
		k := mixedKey(i)
		m.Insert(k, k)
	}
	var queries [8192]uint64
	for i := range queries {
		queries[i] = mixedKey(mixedKey(uint64(i+17)) % 262144)
	}
	var total atomic.Uint64
	var worker atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
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
}
