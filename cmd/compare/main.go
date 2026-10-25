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
	kind := flag.String("kind", "all", "all, tree, map, builtin, or syncmap")
	size := flag.Int("size", 262144, "entries")
	queryCount := flag.Int("queries", 8192, "query buffer length (power of two)")
	rounds := flag.Int("rounds", 128, "query repetitions")
	standard := flag.Bool("standard", false, "include standard-library map lookups")
	threads := flag.Int("threads", 0, "additional parallel lookup workers")
	flag.Parse()
	if *size <= 0 || *rounds <= 0 {
		panic("size and rounds must be positive")
	}
	if *queryCount < 8 || *queryCount&(*queryCount-1) != 0 {
		panic("queries must be a power of two, at least eight")
	}
	switch *kind {
	case "all", "tree", "map", "builtin", "syncmap":
	default:
		panic("unknown kind")
	}
	doTree := *kind == "all" || *kind == "tree"
	doMap := *kind == "all" || *kind == "map"
	doBuiltin := *kind == "builtin" || (*kind == "all" && *standard)
	doSync := *kind == "syncmap" || (*kind == "all" && *standard)
	hardware, width := gobbtree.SIMD()
	fmt.Printf("%s %s/%s SIMD=%v width=%d n=%d rounds=%d queries=%d kind=%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, hardware, width, *size, *rounds, *queryCount, *kind)
	tree := gobbtree.NewTree[uint64, uint64]()
	concurrent := gobbtree.New[uint64, uint64]()
	var builtin map[uint64]uint64
	if doBuiltin {
		builtin = make(map[uint64]uint64, *size)
	}
	var synced sync.Map
	for i := 0; i < *size; i++ {
		key := mix(uint64(i))
		if doTree {
			tree.Insert(key, key)
		}
		if doMap {
			concurrent.Insert(key, key)
		}
		if doBuiltin {
			builtin[key] = key
		}
		if doSync {
			synced.Store(key, key)
		}
	}
	runtime.GC()
	queries := make([]uint64, *queryCount)
	queryMask := len(queries) - 1
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
		var start time.Time
		if doTree {
			start = time.Now()
			for r := 0; r < *rounds; r++ {
				for _, q := range queries {
					v, _ := tree.Get(q)
					sum += v
				}
			}
			report("tree_"+label, start, sum)
		}
		if doMap {
			sum = 0
			start = time.Now()
			for r := 0; r < *rounds; r++ {
				for _, q := range queries {
					v, _ := concurrent.Get(q)
					sum += v
				}
			}
			report("map_"+label, start, sum)
		}
		if doBuiltin {
			sum = 0
			start = time.Now()
			for r := 0; r < *rounds; r++ {
				for _, q := range queries {
					sum += builtin[q]
				}
			}
			report("builtin_"+label, start, sum)
		}
		if doSync {
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
		if doTree {
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
	}
	if *threads > 0 && (doMap || doBuiltin || doSync) {
		runtime.GOMAXPROCS(*threads)
		for i := range queries {
			queries[i] = mix(mix(uint64(i+17)) % uint64(*size))
		}
		var mutex sync.RWMutex
		for _, writeEvery := range []int{0, 10} {
			label := "hit"
			if writeEvery != 0 {
				label = "write10"
			}
			if doMap {
				parallel(fmt.Sprintf("map_parallel%d_%s", *threads, label), *threads, *rounds*len(queries), func(worker int) uint64 {
					var sum uint64
					for r := 0; r < *rounds; r++ {
						for i := range queries {
							q := queries[(i+worker*113)&queryMask]
							if writeEvery != 0 && i%10 == 0 {
								concurrent.Insert(q, q^uint64(r&1))
							} else {
								v, _ := concurrent.Get(q)
								sum += v &^ 1
							}
						}
					}
					return sum
				})
			}
			if doSync {
				parallel(fmt.Sprintf("syncmap_parallel%d_%s", *threads, label), *threads, *rounds*len(queries), func(worker int) uint64 {
					var sum uint64
					for r := 0; r < *rounds; r++ {
						for i := range queries {
							q := queries[(i+worker*113)&queryMask]
							if writeEvery != 0 && i%10 == 0 {
								synced.Store(q, q^uint64(r&1))
							} else {
								v, ok := synced.Load(q)
								if ok {
									sum += v.(uint64) &^ 1
								}
							}
						}
					}
					return sum
				})
			}
			if doBuiltin {
				parallel(fmt.Sprintf("rwmap_parallel%d_%s", *threads, label), *threads, *rounds*len(queries), func(worker int) uint64 {
					var sum uint64
					for r := 0; r < *rounds; r++ {
						for i := range queries {
							q := queries[(i+worker*113)&queryMask]
							if writeEvery != 0 && i%10 == 0 {
								mutex.Lock()
								builtin[q] = q ^ uint64(r&1)
								mutex.Unlock()
							} else {
								mutex.RLock()
								v := builtin[q]
								mutex.RUnlock()
								sum += v &^ 1
							}
						}
					}
					return sum
				})
				if writeEvery == 0 {
					parallel(fmt.Sprintf("builtin_parallel%d_hit", *threads), *threads, *rounds*len(queries), func(worker int) uint64 {
						var sum uint64
						for r := 0; r < *rounds; r++ {
							for i := range queries {
								sum += builtin[queries[(i+worker*113)&queryMask]] &^ 1
							}
						}
						return sum
					})
				}
			}
		}
	}
}

// The callback runs once per worker, outside the per-operation dispatch.
// Worker creation, final checksum reduction and previous-case GC are untimed.
func parallel(name string, workers, operations int, work func(int) uint64) {
	runtime.GC()
	sums := make([]uint64, workers)
	var ready, done sync.WaitGroup
	ready.Add(workers)
	done.Add(workers)
	begin := make(chan struct{})
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-begin
			sums[worker] = work(worker)
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
	fmt.Printf("%s %.3f ns/op checksum=%d\n", name, float64(elapsed.Nanoseconds())/float64(workers*operations), sum)
}
