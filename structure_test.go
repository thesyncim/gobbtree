package gobbtree

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestStructuralExclusion(t *testing.T) {
	var lock structuralLock
	var readers atomic.Int64
	var writer atomic.Bool
	var wg sync.WaitGroup
	for lane := 0; lane < structuralLanes; lane++ {
		wg.Go(func() {
			for i := 0; i < 500; i++ {
				gate := lock.lane(uint64(lane))
				gate.RLock()
				readers.Add(1)
				if writer.Load() {
					t.Error("structural operation overlaps a shared operation")
				}
				if i%31 == 0 {
					runtime.Gosched()
				}
				readers.Add(-1)
				gate.RUnlock()
			}
		})
	}
	for worker := 0; worker < 3; worker++ {
		wg.Go(func() {
			for i := 0; i < 500; i++ {
				lock.Lock()
				if writer.Swap(true) || readers.Load() != 0 {
					t.Error("exclusive structural access violated")
				}
				if i%31 == 0 {
					runtime.Gosched()
				}
				writer.Store(false)
				lock.Unlock()
			}
		})
	}
	wg.Wait()
}

func TestAdaptiveStructuralExclusion(t *testing.T) {
	var lock structuralLock
	first := lock.acquire(1)
	if first.lane != nil || lock.gates().parallel.Load() {
		t.Fatal("first operation did not use the serial gate")
	}
	first.unlock()
	var readers atomic.Int64
	var writer atomic.Bool
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Go(func() {
			for i := 0; i < 500; i++ {
				if worker%4 == 0 {
					lock.Lock()
					if writer.Swap(true) || readers.Load() != 0 {
						t.Error("structural operation overlapped readers or writers")
					}
					runtime.Gosched()
					writer.Store(false)
					lock.Unlock()
				} else {
					guard := lock.acquire(uint64(worker))
					readers.Add(1)
					if writer.Load() {
						t.Error("ordinary operation overlapped structural change")
					}
					if i%31 == 0 {
						runtime.Gosched()
					}
					readers.Add(-1)
					guard.unlock()
				}
			}
		})
	}
	wg.Wait()
	if !lock.gates().parallel.Load() {
		t.Fatal("concurrent operations did not activate lanes")
	}
}
