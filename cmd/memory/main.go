// Command memory keeps one populated container alive for external RSS sampling.
package main

import (
	"flag"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
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

// keyOf returns stored key x in a key pattern, matching cmd/compare.
func keyOf(x uint64, pattern string) uint64 {
	switch pattern {
	case "sequential":
		return x << 1
	case "clustered":
		return mix(x>>10)&^(1<<20-1) | (x&1023)<<1
	}
	return mix(x)
}
func main() {
	size := flag.Int("size", 5000000, "entries")
	kind := flag.String("kind", "tree", "tree, map, builtin, or syncmap")
	stats := flag.Bool("stats", false, "include supplemental Go heap diagnostics")
	pattern := flag.String("keys", "mixed", "key pattern: mixed, sequential, or clustered")
	flag.Parse()
	switch *pattern {
	case "mixed", "sequential", "clustered":
	default:
		panic("unknown key pattern")
	}
	key := func(x uint64) uint64 { return keyOf(x, *pattern) }
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
			k := key(uint64(i))
			t.Insert(k, k)
		}
		buildNS = time.Since(start).Nanoseconds()
		readAll = func() {
			for i := 0; i < *size; i++ {
				v, ok := t.Get(key(uint64(i)))
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
			k := key(uint64(i))
			t.Insert(k, k)
		}
		buildNS = time.Since(start).Nanoseconds()
		readAll = func() {
			for i := 0; i < *size; i++ {
				v, ok := t.Get(key(uint64(i)))
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
			k := key(uint64(i))
			t[k] = k
		}
		buildNS = time.Since(start).Nanoseconds()
		readAll = func() {
			for i := 0; i < *size; i++ {
				v, ok := t[key(uint64(i))]
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
			k := key(uint64(i))
			t.Store(k, k)
		}
		buildNS = time.Since(start).Nanoseconds()
		readAll = func() {
			for i := 0; i < *size; i++ {
				v, ok := t.Load(key(uint64(i)))
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
	record := make([]byte, 0, 512)
	report := func(phase string) {
		record = append(record[:0], `{"kind":"`...)
		record = append(record, (*kind)...)
		record = append(record, `","phase":"`...)
		record = append(record, phase...)
		record = append(record, '"')
		record = appendNumber(record, "pid", uint64(os.Getpid()))
		record = appendNumber(record, "build_ns", uint64(buildNS))
		record = appendNumber(record, "entries", uint64(*size))
		record = appendNumber(record, "checksum", sum)
		if *stats {
			record = appendHeapStats(record)
		}
		record = append(record, '}', '\n')
		if _, err := os.Stdout.Write(record); err != nil {
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

func appendNumber(dst []byte, key string, value uint64) []byte {
	dst = append(dst, ',', '"')
	dst = append(dst, key...)
	dst = append(dst, '"', ':')
	return strconv.AppendUint(dst, value, 10)
}

// Keep diagnostic stack space and runtime introspection out of the RSS-only path.
func appendHeapStats(dst []byte) []byte {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	scan := []metrics.Sample{{Name: "/gc/scan/heap:bytes"}}
	metrics.Read(scan)
	dst = appendNumber(dst, "heap_bytes", mem.HeapAlloc)
	dst = appendNumber(dst, "heap_objects", mem.HeapObjects)
	return appendNumber(dst, "scan_bytes", scan[0].Value.Uint64())
}
