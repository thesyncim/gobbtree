package gobbtree

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestFirstNumericReaderDuringWrites(t *testing.T) {
	for round := 0; round < 32; round++ {
		var m Map[uint64, uint64]
		var group sync.WaitGroup
		for worker := uint64(0); worker < 4; worker++ {
			group.Go(func() {
				for i := worker; i < 1024; i += 4 {
					m.Insert(i, i)
				}
			})
		}
		group.Go(func() {
			for key := uint64(0); key < 1024; key++ {
				if value, ok := m.Get(key); ok && value != key {
					t.Error("first reader observed incomplete entry")
				}
			}
		})
		group.Wait()
		for key := uint64(0); key < 1024; key++ {
			if value, ok := m.Get(key); !ok || value != key {
				t.Fatal("entry lost across reader activation")
			}
		}
	}
}

// Optimistic readers must never return a torn or shifted entry while writers
// move neighboring entries inside the same leaves. Even keys stay present with
// fixed values; odd keys between them are deleted and reinserted continuously.
func TestOptimisticReadsDuringLeafShifts(t *testing.T) {
	const keys = 1 << 14
	var m Map[uint64, uint64]
	for k := uint64(0); k < keys; k += 2 {
		m.Insert(k, k*3+1)
	}
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for w := uint64(0); w < 3; w++ {
		writers.Go(func() {
			x := w*0x9e3779b97f4a7c15 + 1
			for {
				select {
				case <-stop:
					return
				default:
				}
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				k := x%keys | 1
				if _, found := m.Delete(k); !found {
					m.Insert(k, k*3+1)
				}
			}
		})
	}
	var readers sync.WaitGroup
	for r := uint64(0); r < 3; r++ {
		readers.Go(func() {
			x := r*0x2545f4914f6cdd1d + 7
			var batch [8]uint64
			var results [8]Result[uint64]
			for i := 0; i < 200000; i++ {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				k := x % keys &^ 1
				if value, found := m.Get(k); !found || value != k*3+1 {
					t.Errorf("Get(%d) = %d, %v during leaf shifts", k, value, found)
					return
				}
				if i%8 == 0 {
					for j := range batch {
						batch[j] = (k + uint64(j)*2) % keys
					}
					m.GetMany(batch[:], results[:])
					for j, result := range results {
						if !result.OK || result.Value != batch[j]*3+1 {
							t.Errorf("GetMany(%d) = %d, %v during leaf shifts", batch[j], result.Value, result.OK)
							return
						}
					}
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	writers.Wait()
}

// Textual leaves with word values are read optimistically from the live node.
// Stable keys must keep their values while their zero-extended twins, which
// tie their windows, are deleted and reinserted beside them.
func TestOptimisticTextReadsDuringLeafShifts(t *testing.T) {
	const keys = 1 << 12
	stable := make([]string, keys)
	var m Map[string, uint64]
	for i := range stable {
		stable[i] = fmt.Sprintf("key:%06d", i*7919%keys)
		m.Insert(stable[i], uint64(i)*3+1)
	}
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for w := uint64(0); w < 3; w++ {
		writers.Go(func() {
			x := w*0x9e3779b97f4a7c15 + 1
			for {
				select {
				case <-stop:
					return
				default:
				}
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				twin := stable[x%keys] + "\x00"
				if _, found := m.Delete(twin); !found {
					m.Insert(twin, 0)
				}
			}
		})
	}
	var readers sync.WaitGroup
	for r := uint64(0); r < 3; r++ {
		readers.Go(func() {
			x := r*0x2545f4914f6cdd1d + 7
			for i := 0; i < 200000; i++ {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				k := x % keys
				key := strings.Clone(stable[k])
				if value, found := m.Get(key); !found || value != k*3+1 {
					t.Errorf("Get(%q) = %d, %v during leaf shifts", key, value, found)
					return
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	writers.Wait()
}
