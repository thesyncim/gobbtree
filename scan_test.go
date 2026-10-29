package gobbtree

import (
	"fmt"
	"iter"
	"sync"
	"testing"
	"time"
)

// scanMap adapts numeric and textual maps to one scan test.
type scanMap struct {
	insert, delete func(int)
	scan           func(lower, upper int, reverse bool, visit func(int) bool)
	all            func(visit func(int) bool)
}

func scanMaps() map[string]func() scanMap {
	textual := func(format string) func() scanMap {
		key := func(i int) string { return fmt.Sprintf(format, i) }
		parse := func(s string) int { var i int; fmt.Sscanf(s, format, &i); return i }
		return func() scanMap {
			m := New[string, [2]int]()
			return scanMap{
				insert: func(i int) { m.Insert(key(i), [2]int{i}) },
				delete: func(i int) { m.Delete(key(i)) },
				scan: func(lower, upper int, reverse bool, visit func(int) bool) {
					f := m.Range
					if reverse {
						f = m.RangeReverse
					}
					for k := range f(key(lower), key(upper)) {
						if !visit(parse(k)) {
							return
						}
					}
				},
				all: func(visit func(int) bool) {
					for k := range m.All() {
						if !visit(parse(k)) {
							return
						}
					}
				},
			}
		}
	}
	return map[string]func() scanMap{
		"numeric": func() scanMap {
			m := New[uint64, uint64]()
			return scanMap{
				insert: func(i int) { m.Insert(uint64(i), uint64(i)) },
				delete: func(i int) { m.Delete(uint64(i)) },
				scan: func(lower, upper int, reverse bool, visit func(int) bool) {
					f := m.Range
					if reverse {
						f = m.RangeReverse
					}
					for k := range f(uint64(lower), uint64(upper)) {
						if !visit(int(k)) {
							return
						}
					}
				},
				all: func(visit func(int) bool) {
					for k := range m.All() {
						if !visit(int(k)) {
							return
						}
					}
				},
			}
		},
		"textual": textual("k%08d"),
		// Keys sharing more than a node head holds take the long-prefix paths.
		"long prefix": textual("https://example.com/users/%08d"),
	}
}

// A scan holds no latch while its callback runs: a writer that splits and
// merges leaves finishes while the scan waits inside its callback.
func TestScanCallbackDoesNotBlockWriters(t *testing.T) {
	for name, build := range scanMaps() {
		t.Run(name, func(t *testing.T) {
			m := build()
			for i := 0; i < 4096; i += 2 {
				m.insert(i)
			}
			writer := make(chan struct{})
			m.all(func(int) bool {
				go func() {
					for i := 1; i < 4096; i += 2 {
						m.insert(i)
					}
					for i := 1; i < 4096; i += 2 {
						m.delete(i)
					}
					close(writer)
				}()
				select {
				case <-writer:
				case <-time.After(10 * time.Second):
					t.Fatal("writer blocked by a scan callback")
				}
				return false
			})
		})
	}
}

// Callbacks may use the map they scan.
func TestScanCallbackReentersMap(t *testing.T) {
	for name, build := range scanMaps() {
		t.Run(name, func(t *testing.T) {
			m := build()
			for i := 0; i < 1000; i++ {
				m.insert(2 * i)
			}
			visited := 0
			m.scan(0, 2000, false, func(k int) bool {
				m.insert(k + 1)
				m.scan(k, k+1, true, func(int) bool { return true })
				visited++
				return true
			})
			if visited < 1000 {
				t.Fatalf("visited %d", visited)
			}
		})
	}
}

// Entries present throughout a scan are visited exactly once, in order,
// while writers insert and remove others, splitting and merging leaves.
func TestScanDuringWrites(t *testing.T) {
	for name, build := range scanMaps() {
		t.Run(name, func(t *testing.T) {
			m := build()
			const stable = 3000
			for i := 0; i < stable; i++ {
				m.insert(4 * i)
			}
			stop := make(chan struct{})
			var writers sync.WaitGroup
			for w := 0; w < 2; w++ {
				writers.Go(func() {
					x := uint64(w + 1)
					for {
						select {
						case <-stop:
							return
						default:
						}
						x ^= x << 13
						x ^= x >> 7
						x ^= x << 17
						base := 4*int(x%stable) + 1
						for k := base; k < base+3; k++ {
							m.insert(k)
						}
						for k := base; k < base+3; k++ {
							m.delete(k)
						}
					}
				})
			}
			for round := 0; round < 20; round++ {
				for _, reverse := range []bool{false, true} {
					lower, upper := 4*(round*37%stable), 4*stable
					var seen []int
					m.scan(lower, upper, reverse, func(k int) bool {
						if k%4 == 0 {
							seen = append(seen, k)
						}
						return true
					})
					want := (upper - lower) / 4
					if len(seen) != want {
						t.Fatalf("reverse=%v visited %d stable keys, want %d", reverse, len(seen), want)
					}
					for i := 1; i < len(seen); i++ {
						if (seen[i] > seen[i-1]) == reverse {
							t.Fatalf("reverse=%v out of order: %d after %d", reverse, seen[i], seen[i-1])
						}
					}
				}
			}
			close(stop)
			writers.Wait()
		})
	}
}

// visitRange feeds an iterator to a callback until it returns false.
func visitRange[K, V any](seq iter.Seq2[K, V], visit func(K, V) bool) {
	for k, v := range seq {
		if !visit(k, v) {
			return
		}
	}
}
