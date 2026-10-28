package main

import (
	"fmt"
	"iter"
	"sync"

	"github.com/thesyncim/gobbtree"
)

// scaleMap is what a core-scaling cell needs from a concurrent map.
type scaleMap interface {
	Get(uint64) (uint64, bool)
	Insert(uint64, uint64) (uint64, bool)
	Delete(uint64) (uint64, bool)
	All() iter.Seq2[uint64, uint64]
}

// syncScale is a sync.Map, as Go programs use it for concurrent access.
type syncScale struct{ m *sync.Map }

func (s syncScale) Get(k uint64) (uint64, bool) {
	v, ok := s.m.Load(k)
	if !ok {
		return 0, false
	}
	return v.(uint64), true
}
func (s syncScale) Insert(k, v uint64) (uint64, bool) {
	old, ok := s.m.Swap(k, v)
	if !ok {
		return 0, false
	}
	return old.(uint64), true
}
func (s syncScale) Delete(k uint64) (uint64, bool) {
	v, ok := s.m.LoadAndDelete(k)
	if !ok {
		return 0, false
	}
	return v.(uint64), true
}
func (s syncScale) All() iter.Seq2[uint64, uint64] {
	return func(yield func(uint64, uint64) bool) {
		s.m.Range(func(k, v any) bool { return yield(k.(uint64), v.(uint64)) })
	}
}

// lockedScale is a built-in map behind a sync.RWMutex.
type lockedScale struct {
	mu sync.RWMutex
	m  map[uint64]uint64
}

func (s *lockedScale) Get(k uint64) (uint64, bool) {
	s.mu.RLock()
	v, ok := s.m[k]
	s.mu.RUnlock()
	return v, ok
}
func (s *lockedScale) Insert(k, v uint64) (uint64, bool) {
	s.mu.Lock()
	old, ok := s.m[k]
	s.m[k] = v
	s.mu.Unlock()
	return old, ok
}
func (s *lockedScale) Delete(k uint64) (uint64, bool) {
	s.mu.Lock()
	v, ok := s.m[k]
	delete(s.m, k)
	s.mu.Unlock()
	return v, ok
}
func (s *lockedScale) All() iter.Seq2[uint64, uint64] {
	return func(yield func(uint64, uint64) bool) {
		for k, v := range s.m {
			if !yield(k, v) {
				return
			}
		}
	}
}

// scaleMixes lists the core-scaling cells. writeEvery is the period of owner
// writes among a worker's operations: 0 reads only, 1 writes only.
var scaleMixes = map[string]int{
	"read":    0,
	"r90":     10,
	"r50":     2,
	"replace": 1,
	"churn":   1,
	"scan":    10,
}

// main runs a core-scaling cell through runScale, so main.go also builds
// against libraries older than this file's API.
func init() { runScale = scale }

// scanLength is how many entries each scan operation visits.
const scanLength = 100

// ownedKeys returns the keys worker owns, indexes congruent to worker modulo
// workers, in the order it visits them, and the fresh key paired with each.
// Each worker starts at its own slot, so workers walking sequential or
// clustered keys do not write one leaf in lockstep.
func ownedKeys(worker, workers int, size uint64, keyAt func(uint64) uint64) (stored, fresh []uint64) {
	owned := (size - uint64(worker) + uint64(workers) - 1) / uint64(workers)
	stored, fresh = make([]uint64, owned), make([]uint64, owned)
	step, slot := coprimeStep(owned), mix(uint64(worker)+1)%owned
	for t := range stored {
		x := uint64(worker) + slot*uint64(workers)
		stored[t], fresh[t] = keyAt(x), keyAt(x+size)
		slot = (slot + step) % owned
	}
	return stored, fresh
}

// scaleState folds a final entry into the state checksum.
func scaleState(key, value uint64) uint64 { return mix(key) ^ value }

// expectedScaleState derives the final state checksum from one worker's
// write schedule, independently of the map.
func expectedScaleState(name string, stored, fresh []uint64, writes int) uint64 {
	owned := len(stored)
	passes, partial := writes/owned, writes%owned
	var state uint64
	if name != "churn" {
		for t, k := range stored {
			touches := passes
			if t < partial {
				touches++
			}
			state += scaleState(k, k^uint64(touches&1))
		}
		return state
	}
	// After whole passes one key set is present; the partial pass deleted a
	// prefix of it and inserted the other set from half an orbit ahead.
	present, absent := stored, fresh
	if passes&1 != 0 {
		present, absent = fresh, stored
	}
	for _, k := range present {
		state += scaleState(k, k)
	}
	for t := 0; t < partial; t++ {
		k := absent[(t+owned/2)%owned]
		state += scaleState(k, k) - scaleState(present[t], present[t])
	}
	return state
}

// scale runs one core-scaling cell on m, which holds the size stored keys:
// workers run ops operations each. Writes go to keys the worker owns, so
// every value, and the final state, is deterministic; reads sample the shared
// queries buffer from per-worker offsets and add values with the low bit
// cleared, which writes toggle. Churn pairs count as one operation: a delete of
// an owned key and an insert of a fresh owned key half an orbit away, in
// another leaf. Building the keys, starting workers, reducing checksums and
// verifying the final state are untimed.
func scale(target any, name string, workers, ops int, queries []uint64, size uint64, keyAt func(uint64) uint64) {
	var m scaleMap
	ordered, _ := target.(*gobbtree.Map[uint64, uint64])
	switch target := target.(type) {
	case *gobbtree.Map[uint64, uint64]:
		m = target
	case *sync.Map:
		m = syncScale{target}
	case map[uint64]uint64:
		m = &lockedScale{m: target}
	default:
		panic("unknown scale backend")
	}
	writeEvery, ok := scaleMixes[name]
	if !ok {
		panic("unknown scale mix")
	}
	if size < uint64(workers) {
		panic("scale needs at least one key per worker")
	}
	stored, fresh := make([][]uint64, workers), make([][]uint64, workers)
	var build sync.WaitGroup
	for worker := range workers {
		build.Go(func() { stored[worker], fresh[worker] = ownedKeys(worker, workers, size, keyAt) })
	}
	build.Wait()
	mask, scan, churn := len(queries)-1, name == "scan", name == "churn"
	if scan && ordered == nil {
		panic("scan needs an ordered map")
	}
	elapsed, sum := runParallel(workers, func(worker int) uint64 {
		var sum uint64
		stored, fresh := stored[worker], fresh[worker]
		offset := worker * len(queries) / workers
		t, flip := 0, uint64(1)
		if churn {
			// Each pass deletes every key of one set in visit order and inserts
			// the other set's, then the sets swap roles.
			remove, add := stored, fresh
			u := len(stored) / 2
			for j := 0; j < ops; j++ {
				q := remove[t]
				v, ok := m.Delete(q)
				if !ok || v != q {
					panic("churn must delete an existing key")
				}
				sum += v
				if _, replaced := m.Insert(add[u], add[u]); replaced {
					panic("churn must insert a new key")
				}
				if u++; u == len(add) {
					u = 0
				}
				if t++; t == len(remove) {
					t, remove, add = 0, add, remove
				}
			}
			return sum
		}
		for j := 0; j < ops; j++ {
			if writeEvery != 0 && j%writeEvery == 0 {
				k := stored[t]
				v, _ := m.Insert(k, k^flip)
				sum += v
				if t++; t == len(stored) {
					t, flip = 0, flip^1
				}
				continue
			}
			q := queries[(j+offset)&mask]
			if scan {
				// Scan has range_cloned's contract, so the Rust driver
				// takes 100 entries from range_cloned(q..).
				n := 0
				for _, v := range ordered.Scan(gobbtree.AtLeast(q)) {
					sum += v &^ 1
					if n++; n == scanLength {
						break
					}
				}
				continue
			}
			v, _ := m.Get(q)
			sum += v &^ 1
		}
		return sum
	})
	writes := 0
	if writeEvery != 0 {
		writes = (ops + writeEvery - 1) / writeEvery
	}
	var want uint64
	for worker := range workers {
		want += expectedScaleState(name, stored[worker], fresh[worker], writes)
	}
	var state uint64
	count := 0
	for k, v := range m.All() {
		state += scaleState(k, v)
		count++
	}
	if count != int(size) {
		panic(fmt.Sprintf("scale %s final count %d, want %d", name, count, size))
	}
	if state != want {
		panic(fmt.Sprintf("scale %s final state %d, want %d", name, state, want))
	}
	fmt.Printf("scale_%s_w%d %.3f ns/op checksum=%d state=%d count=%d\n", name, workers, float64(elapsed.Nanoseconds())/float64(workers*ops), sum, state, count)
}
