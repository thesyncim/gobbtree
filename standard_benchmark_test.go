package gobbtree

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// Parallel comparisons use the same adapter overhead, worker schedule, keys,
// hit rate and write ratio. The unprotected built-in map runs only read cases.
func BenchmarkConcurrentMaps(b *testing.B) {
	for _, size := range []int{4096, 262144} {
		var keys [8192]uint64
		for i := range keys {
			keys[i] = mixedKey(mixedKey(uint64(i+17)) % uint64(size))
		}
		for _, workload := range []string{"read_hit", "read_miss50", "write10"} {
			for _, implementation := range []string{"Map", "sync.Map", "map+RWMutex", "map"} {
				if implementation == "map" && workload == "write10" {
					continue
				}
				b.Run(fmt.Sprintf("%d/%s/%s", size, workload, implementation), func(b *testing.B) {
					var get func(uint64) (uint64, bool)
					var put func(uint64, uint64)
					switch implementation {
					case "Map":
						m := New[uint64, uint64]()
						get = m.Get
						put = func(k, v uint64) { m.Insert(k, v) }
					case "sync.Map":
						m := new(sync.Map)
						get = func(k uint64) (uint64, bool) {
							v, ok := m.Load(k)
							if !ok {
								return 0, false
							}
							return v.(uint64), true
						}
						put = func(k, v uint64) { m.Store(k, v) }
					case "map+RWMutex":
						m := make(map[uint64]uint64, size)
						var mu sync.RWMutex
						get = func(k uint64) (uint64, bool) {
							mu.RLock()
							v, ok := m[k]
							mu.RUnlock()
							return v, ok
						}
						put = func(k, v uint64) { mu.Lock(); m[k] = v; mu.Unlock() }
					case "map":
						m := make(map[uint64]uint64, size)
						get = func(k uint64) (uint64, bool) { v, ok := m[k]; return v, ok }
						put = func(k, v uint64) { m[k] = v }
					}
					for i := 0; i < size; i++ {
						k := mixedKey(uint64(i))
						put(k, k)
					}
					queries := keys
					if workload == "read_miss50" {
						for i := 1; i < len(queries); i += 2 {
							queries[i] = mixedKey(uint64(size + i))
						}
					}
					var workers, checksum atomic.Uint64
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						worker := workers.Add(1) - 1
						index := worker * 113
						var sum, iteration uint64
						for pb.Next() {
							key := queries[index&8191]
							if workload == "write10" && iteration%10 == 0 {
								put(key, index)
							} else {
								v, _ := get(key)
								sum += v
							}
							index++
							iteration++
						}
						checksum.Add(sum)
					})
					benchmarkSink = checksum.Load()
				})
			}
		}
	}
}
