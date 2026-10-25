// Command compare is the Go half of the matched native upstream benchmark.
package main

import (
	"flag"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/thesyncim/gobbtree"
)

func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
func main() {
	size := flag.Int("size", 262144, "entries")
	rounds := flag.Int("rounds", 128, "query repetitions")
	standard := flag.Bool("standard", false, "include standard-library map lookups")
	threads := flag.Int("threads", 0, "additional parallel lookup workers")
	flag.Parse()
	if *size <= 0 || *rounds <= 0 {
		panic("size and rounds must be positive")
	}
	hardware, width := gobbtree.SIMD()
	fmt.Printf("%s %s/%s SIMD=%v width=%d n=%d rounds=%d\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, hardware, width, *size, *rounds)
	tree := gobbtree.NewTree[uint64, uint64]()
	concurrent := gobbtree.New[uint64, uint64]()
	var builtin map[uint64]uint64
	if *standard {
		builtin = make(map[uint64]uint64, *size)
	}
	var synced sync.Map
	for i := 0; i < *size; i++ {
		key := mix(uint64(i))
		tree.Insert(key, key)
		concurrent.Insert(key, key)
		if *standard {
			builtin[key] = key
			synced.Store(key, key)
		}
	}
	runtime.GC()
	var queries [8192]uint64
	report := func(name string, start time.Time, sum uint64) {
		fmt.Printf("%s %.3f ns/op checksum=%d\n", name, float64(time.Since(start).Nanoseconds())/float64(*rounds*len(queries)), sum)
	}
	for _, hit := range []bool{true, false} {
		label := "miss"
		if hit {
			label = "hit"
		}
		for i := range queries {
			x := mix(uint64(i+17)) % uint64(*size)
			if !hit {
				x += uint64(*size)
			}
			queries[i] = mix(x)
		}
		var sum uint64
		start := time.Now()
		for r := 0; r < *rounds; r++ {
			for _, q := range queries {
				v, _ := tree.Get(q)
				sum += v
			}
		}
		report("tree_"+label, start, sum)
		sum = 0
		start = time.Now()
		for r := 0; r < *rounds; r++ {
			for _, q := range queries {
				v, _ := concurrent.Get(q)
				sum += v
			}
		}
		report("map_"+label, start, sum)
		if *standard {
			sum = 0
			start = time.Now()
			for r := 0; r < *rounds; r++ {
				for _, q := range queries {
					sum += builtin[q]
				}
			}
			report("builtin_"+label, start, sum)
			sum = 0
			start = time.Now()
			for r := 0; r < *rounds; r++ {
				for _, q := range queries {
					if v, ok := synced.Load(q); ok {
						sum += v.(uint64)
					}
				}
			}
			report("syncmap_"+label, start, sum)

		}
		var dst [8]gobbtree.Result[uint64]
		sum = 0
		start = time.Now()
		for r := 0; r < *rounds; r++ {
			for i := 0; i < len(queries); i += 8 {
				tree.GetMany(queries[i:i+8], dst[:])
				for _, v := range dst {
					sum += v.Value
				}
			}
		}
		report("tree_batch8_"+label, start, sum)
	}
	if *threads > 0 {
		runtime.GOMAXPROCS(*threads)
		for i := range queries {
			queries[i] = mix(mix(uint64(i+17)) % uint64(*size))
		}
		for _, writeEvery := range []int{0, 10} {
			sums := make([]uint64, *threads)
			var ready, done sync.WaitGroup
			ready.Add(*threads)
			done.Add(*threads)
			begin := make(chan struct{})
			for worker := 0; worker < *threads; worker++ {
				go func() {
					defer done.Done()
					ready.Done()
					<-begin
					var sum uint64
					for r := 0; r < *rounds; r++ {
						for i := range queries {
							q := queries[(i+worker*113)&8191]
							if writeEvery != 0 && i%10 == 0 {
								concurrent.Insert(q, q^uint64(r&1))
							} else {
								v, _ := concurrent.Get(q)
								sum += v &^ 1
							}
						}
					}
					sums[worker] = sum
				}()
			}
			ready.Wait()
			start := time.Now()
			close(begin)
			done.Wait()
			elapsed := time.Since(start)
			var sum uint64
			for _, v := range sums {
				sum += v
			}
			label := "hit"
			if writeEvery != 0 {
				label = "write10"
			}
			fmt.Printf("map_parallel%d_%s %.3f ns/op checksum=%d\n", *threads, label, float64(elapsed.Nanoseconds())/float64(*threads**rounds*len(queries)), sum)
		}
	}
}
