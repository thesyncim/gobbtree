// Command memory keeps one populated container alive for external RSS sampling.
package main

import (
	"encoding/json"
	"flag"
	"os"
	"runtime"
	"runtime/metrics"
	"sync"
	"time"

	"github.com/thesyncim/gobbtree"
)

func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ (x >> 31)
}
func main() {
	size := flag.Int("size", 5000000, "entries")
	kind := flag.String("kind", "tree", "tree, map, builtin, or syncmap")
	flag.Parse()
	if *size < 0 {
		panic("negative size")
	}
	var keep any
	var readAll func()
	var sum uint64
	var buildNS int64
	start := time.Now()
	switch *kind {
	case "tree":
		t := gobbtree.NewTree[uint64, uint64]()
		for i := 0; i < *size; i++ {
			k := mix(uint64(i))
			t.Insert(k, k)
		}
		buildNS = time.Since(start).Nanoseconds()
		readAll = func() {
			for i := 0; i < *size; i++ {
				v, ok := t.Get(mix(uint64(i)))
				if !ok {
					panic("missing key")
				}
				sum += v
			}
		}
		keep = t
	case "map":
		t := gobbtree.New[uint64, uint64]()
		for i := 0; i < *size; i++ {
			k := mix(uint64(i))
			t.Insert(k, k)
		}
		buildNS = time.Since(start).Nanoseconds()
		readAll = func() {
			for i := 0; i < *size; i++ {
				v, ok := t.Get(mix(uint64(i)))
				if !ok {
					panic("missing key")
				}
				sum += v
			}
		}
		keep = t
	case "builtin":
		t := make(map[uint64]uint64, *size)
		for i := 0; i < *size; i++ {
			k := mix(uint64(i))
			t[k] = k
		}
		buildNS = time.Since(start).Nanoseconds()
		readAll = func() {
			for i := 0; i < *size; i++ {
				v, ok := t[mix(uint64(i))]
				if !ok {
					panic("missing key")
				}
				sum += v
			}
		}
		keep = t
	case "syncmap":
		t := new(sync.Map)
		for i := 0; i < *size; i++ {
			k := mix(uint64(i))
			t.Store(k, k)
		}
		buildNS = time.Since(start).Nanoseconds()
		readAll = func() {
			for i := 0; i < *size; i++ {
				v, ok := t.Load(mix(uint64(i)))
				if !ok {
					panic("missing key")
				}
				sum += v.(uint64)
			}
		}
		keep = t
	default:
		panic("unknown kind")
	}
	report := func(phase string) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		scan := []metrics.Sample{{Name: "/gc/scan/heap:bytes"}}
		metrics.Read(scan)
		if err := json.NewEncoder(os.Stdout).Encode(struct {
			BuildNS  int64  `json:"build_ns"`
			PID      int    `json:"pid"`
			Kind     string `json:"kind"`
			Phase    string `json:"phase"`
			Entries  int    `json:"entries"`
			Checksum uint64 `json:"checksum"`
			Heap     uint64 `json:"heap_bytes"`
			Objects  uint64 `json:"heap_objects"`
			Scan     uint64 `json:"scan_bytes"`
		}{buildNS, os.Getpid(), *kind, phase, *size, sum, mem.HeapAlloc, mem.HeapObjects, scan[0].Value.Uint64()}); err != nil {
			panic(err)
		}
	}
	wait := func() {
		var signal [1]byte
		if _, err := os.Stdin.Read(signal[:]); err != nil {
			panic(err)
		}
	}
	report("built")
	wait()
	readAll()
	report("read")
	wait()
	runtime.GC()
	report("collected")
	wait()
	runtime.KeepAlive(keep)
}
