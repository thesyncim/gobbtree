package gobbtree

import "testing"

func BenchmarkMutation(b *testing.B) {
	for _, concurrent := range []bool{false, true} {
		name := "Tree"
		if concurrent {
			name = "Map"
		}
		b.Run(name, func(b *testing.B) {
			for _, replace := range []bool{true, false} {
				name := "churn_pair"
				if replace {
					name = "replace"
				}
				b.Run(name, func(b *testing.B) {
					var store interface {
						Insert(uint64, uint64) (uint64, bool)
						Delete(uint64) (uint64, bool)
					}
					if concurrent {
						store = New[uint64, uint64]()
					} else {
						store = NewTree[uint64, uint64]()
					}
					var keys [4096]uint64
					for i := range keys {
						keys[i] = mixedKey(uint64(i))
						store.Insert(keys[i], uint64(i))
					}
					b.ReportAllocs()
					var sum, i uint64
					for b.Loop() {
						key := keys[i&4095]
						if !replace {
							v, _ := store.Delete(key)
							sum += v
						}
						v, _ := store.Insert(key, i)
						sum += v
						i++
					}
					benchmarkSink = sum
				})
			}
		})
	}
}
