package gobbtree

import (
	"sync"
	"sync/atomic"
	"testing"
)

type historyOperation struct {
	start, end       int64
	kind, key, value int
	result           Result[int]
}

// A small exhaustive checker validates returned values against an independent
// sequential map model, respecting the real-time order of nonoverlapping calls.
func linearizable(history []historyOperation) bool {
	var search func(uint16, [2]Result[int]) bool
	search = func(done uint16, state [2]Result[int]) bool {
		if done == uint16(1<<len(history))-1 {
			return true
		}
		for i, op := range history {
			if done&(1<<i) != 0 {
				continue
			}
			ready := true
			for j, before := range history {
				if done&(1<<j) == 0 && before.end < op.start {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			next := state
			expected := state[op.key]
			switch op.kind {
			case 0:
				next[op.key] = Result[int]{op.value, true}
			case 1:
				next[op.key] = Result[int]{}
			case 3:
				expected = Result[int]{}
				next = [2]Result[int]{}
			case 4:
				count := 0
				for _, entry := range state {
					if entry.OK {
						count++
					}
				}
				expected = Result[int]{count, true}
			case 5:
				if expected.OK {
					expected.Value++
					next[op.key] = expected
				}
			case 6:
				if !expected.OK {
					expected.Value = op.value
					next[op.key] = Result[int]{op.value, true}
				}
			}
			if op.result != expected {
				continue
			}
			if search(done|(1<<i), next) {
				return true
			}
		}
		return false
	}
	return search(0, [2]Result[int]{})
}
func TestConcurrentLinearizability(t *testing.T) {
	for round := 0; round < 100; round++ {
		var m Map[int, int]
		var tick atomic.Int64
		var history [12]historyOperation
		start := make(chan struct{})
		var wg sync.WaitGroup
		for worker := 0; worker < 4; worker++ {
			wg.Go(func() {
				<-start
				for step := 0; step < 3; step++ {
					i := worker*3 + step
					op := historyOperation{kind: (i + round) % 8, key: (worker + step + round) % 2, value: i + 1}
					op.start = tick.Add(1)
					switch op.kind {
					case 0:
						op.result.Value, op.result.OK = m.Insert(op.key, op.value)
					case 1:
						op.result.Value, op.result.OK = m.Delete(op.key)
					case 2:
						op.result.Value, op.result.OK = m.Get(op.key)
					case 3:
						m.Clear()
					case 4:
						op.result = Result[int]{m.Len(), true}
					case 5:
						op.result.Value, op.result.OK = m.Update(op.key, func(_, v int) int { return v + 1 })
					case 6:
						op.result.Value, op.result.OK = m.LoadOrStore(op.key, op.value)
					case 7:
						var dst [1]Result[int]
						m.GetMany([]int{op.key}, dst[:])
						op.result = dst[0]
					}
					op.end = tick.Add(1)
					history[i] = op
				}
			})
		}
		close(start)
		wg.Wait()
		if !linearizable(history[:]) {
			t.Fatalf("non-linearizable history: %+v", history)
		}
	}
	// Prove the checker rejects an impossible result, rather than only
	// accepting executions produced by the implementation under test.
	impossible := []historyOperation{{start: 1, end: 2, kind: 2, key: 0, result: Result[int]{9, true}}}
	if linearizable(impossible) {
		t.Fatal("checker accepted a read from an empty map")
	}
}
