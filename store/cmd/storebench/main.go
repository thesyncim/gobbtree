// Command storebench is upstream bbtree-store's examples/bench.rs, ported:
// a store's insert, get and scan times at one goroutine, inserts and gets
// at eight, and the bytes its pages take, nodes' and heap pages'; then the
// heap's bytes under churn, as values change size and entries go and come
// back; then values too long for a record, in overflow pages. One store at
// a time.
//
//	go run ./store/cmd/storebench [entries]
//
// Upstream counts its nodes' bytes through its allocator; a Go store's
// nodes are the collector's, so they are what the heap holds, once
// collected, past the budget's pages: the map's nodes, and what else the
// store keeps in Go's memory. Each goroutine builds its keys in one buffer
// it reuses, where upstream's are arrays on the stack: the store's
// lookups take their key's bytes as a slice, which would otherwise go to
// the heap at each call.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thesyncim/gobbtree/store"
)

// counting is the bytes of the heap pages out, upstream's Counting's heap.
type counting struct{ heap atomic.Int64 }

func (c *counting) TakePages(n int) bool {
	c.heap.Add(int64(n * store.DefaultHeapPageSize))
	return true
}
func (c *counting) GivePages(n int) { c.heap.Add(-int64(n * store.DefaultHeapPageSize)) }

// key writes entry i's key into k, 20 bytes, in an order apart from i's.
func key(k []byte, i uint64) []byte {
	k = binary.BigEndian.AppendUint64(k[:0], i*0x9E37_79B9_7F4A_7C15)
	for len(k) < 20 {
		k = append(k, 'k')
	}
	return k
}

// value writes entry i's value into v: 32 bytes.
func value(v []byte, i uint64) []byte {
	v = v[:0]
	for range 32 {
		v = append(v, byte(i))
	}
	return v
}

func nsPer(start time.Time, count uint64) float64 {
	return float64(time.Since(start).Nanoseconds()) / float64(count)
}

// heapInUse is the bytes Go's heap holds, once collected.
func heapInUse() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

func oneThread(s *store.Store, c *counting, base int64, entries uint64) {
	k, v := make([]byte, 0, 20), make([]byte, 0, 32)
	start := time.Now()
	for i := range entries {
		if _, err := s.Insert(key(k, i), value(v, i)); err != nil {
			panic(err)
		}
	}
	insert := nsPer(start, entries)

	var buf []byte
	start = time.Now()
	for i := range entries {
		var ok bool
		if buf, ok = s.GetInto(key(k, i*7919%entries), buf[:0]); !ok {
			panic("a key went missing")
		}
	}
	get := nsPer(start, entries)

	start = time.Now()
	bytes := 0
	for key, value := range s.Scan(store.Bounds{}) {
		bytes += len(key) + len(value)
	}
	scan := nsPer(start, entries)
	if bytes != int(entries)*52 {
		panic("the scan missed entries")
	}

	heap := c.heap.Load()
	nodes := float64(heapInUse()-base-heap) / float64(entries)
	heapPer := float64(heap) / float64(entries)
	fmt.Printf("1 thread:  insert %.0f ns, get %.0f ns, scan %.1f ns an entry; nodes %.1f + heap %.1f = %.1f bytes an entry\n",
		insert, get, scan, nodes, heapPer, nodes+heapPer)
}

// churn churns over the store's entries keys: each op replaces a value with
// one of 16 to 64 bytes, or removes the entry, or puts a removed one back.
func churn(s *store.Store, c *counting, entries uint64) {
	present := make([]bool, entries)
	for i := range present {
		present[i] = true
	}
	liveBytes := int(entries) * (20 + 32 + 9)
	state := uint64(0x2545_F491_4F6C_DD1D)
	k, v, old := make([]byte, 0, 20), make([]byte, 0, 64), make([]byte, 0, 64)
	start := time.Now()
	for round := uint64(1); round <= 4; round++ {
		for range entries {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			i := state % entries
			length := 16 + int(state>>40)%49
			if present[i] && state>>60 < 4 {
				old, _ = s.GetInto(key(k, i), old[:0])
				if !s.Remove(key(k, i)) {
					panic("a key went missing")
				}
				liveBytes -= 20 + 9 + len(old)
				present[i] = false
			} else {
				var had bool
				old, had = s.GetInto(key(k, i), old[:0])
				v = v[:0]
				for range length {
					v = append(v, byte(i))
				}
				if _, err := s.Insert(key(k, i), v); err != nil {
					panic(err)
				}
				liveBytes += 20 + 9 + length
				if had {
					liveBytes -= 20 + 9 + len(old)
				}
				present[i] = true
			}
		}
		fmt.Printf("churn round %d: heap %.2f bytes a live record byte, %.0f ns an op\n",
			round, float64(c.heap.Load())/float64(liveBytes), nsPer(start, entries*round))
	}
}

func threads(entries uint64) {
	const goroutines = 8
	s := store.NewIn(&counting{})
	start := time.Now()
	var wg sync.WaitGroup
	for t := range uint64(goroutines) {
		wg.Go(func() {
			k, v := make([]byte, 0, 20), make([]byte, 0, 32)
			for i := t; i < entries; i += goroutines {
				if _, err := s.Insert(key(k, i), value(v, i)); err != nil {
					panic(err)
				}
			}
		})
	}
	wg.Wait()
	insert := nsPer(start, entries)
	start = time.Now()
	for t := range uint64(goroutines) {
		wg.Go(func() {
			k := make([]byte, 0, 20)
			var buf []byte
			for i := t; i < entries; i += goroutines {
				var ok bool
				if buf, ok = s.GetInto(key(k, i*7919%entries), buf[:0]); !ok {
					panic("a key went missing")
				}
			}
		})
	}
	wg.Wait()
	get := nsPer(start, entries)
	fmt.Printf("8 threads: insert %.0f ns, get %.0f ns an entry, wall time\n", insert, get)
}

// longValues is a sixteenth as many entries, of 4,000-byte values: three
// overflow pages each, and the rest in the record.
func longValues(entries uint64) {
	const length = 4000
	entries /= 16
	c := &counting{}
	s := store.NewIn(c)
	k, v := make([]byte, 0, 20), make([]byte, length)
	start := time.Now()
	for i := range entries {
		for j := range v {
			v[j] = byte(i)
		}
		if _, err := s.Insert(key(k, i), v); err != nil {
			panic(err)
		}
	}
	insert := nsPer(start, entries)
	var buf []byte
	start = time.Now()
	for i := range entries {
		var ok bool
		if buf, ok = s.GetInto(key(k, i*7919%entries), buf[:0]); !ok || len(buf) != length {
			panic("a long value went missing")
		}
	}
	get := nsPer(start, entries)
	heap := float64(c.heap.Load()) / float64(entries*length)
	fmt.Printf("%d entries of %d-byte values: insert %.0f ns, get %.0f ns; heap %.3f bytes a value byte\n",
		entries, length, insert, get, heap)
}

func main() {
	entries := uint64(10_000_000)
	if len(os.Args) > 1 {
		n, err := strconv.ParseUint(os.Args[1], 10, 64)
		if err != nil {
			panic(err)
		}
		entries = n
	}
	fmt.Printf("%d entries of 20-byte keys and 32-byte values\n", entries)
	base := heapInUse()
	c := &counting{}
	s := store.NewIn(c)
	oneThread(s, c, base, entries)
	churn(s, c, entries)
	s.Clear()
	s = nil
	threads(entries)
	longValues(entries)
}
