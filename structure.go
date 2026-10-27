package gobbtree

import (
	"sync"
	"sync/atomic"
)

const structuralLanes = 16

type structuralLane struct {
	mu sync.RWMutex
	_  [40]byte
}

type structuralGates struct {
	writer   sync.Mutex
	parallel atomic.Bool
	_        [52]byte
	lanes    [structuralLanes]structuralLane
}

// Ordinary leaf writers share a lane selected by key. Structural operations
// acquire every lane, preserving the same exclusion as a single RWMutex
// without making independent leaf writers update one reader-count cache line.
// Allocation is lazy so an empty map needs no lock array.
type structuralLock struct {
	state atomic.Pointer[structuralGates]
}

func (s *structuralLock) gates() *structuralGates {
	if g := s.state.Load(); g != nil {
		return g
	}
	return s.initGates()
}

func (s *structuralLock) initGates() *structuralGates {
	g := new(structuralGates)
	if s.state.CompareAndSwap(nil, g) {
		return g
	}
	return s.state.Load()
}

func structuralLaneCode[O keyOrder[K], K any](key K) uint64 {
	if code, ok := any(key).(uint64); ok {
		return code
	}
	return structuralLaneCodeSlow[O](key)
}
func structuralLaneCodeSlow[O keyOrder[K], K any](key K) uint64 {
	code := numericKey[O](key)
	if str, ok := any(key).(string); ok && len(str) > 8 {
		code ^= stringCode(str, max(0, len(str)-8))
	}
	return code
}

func (s *structuralLock) lane(code uint64) *sync.RWMutex {
	if g := s.state.Load(); g != nil && g.parallel.Load() {
		return g.lane(code)
	}
	return s.parallelLane(code)
}

func (s *structuralLock) parallelLane(code uint64) *sync.RWMutex {
	g := s.gates()
	g.writer.Lock()
	g.parallel.Store(true)
	g.writer.Unlock()
	return g.lane(code)
}

func (g *structuralGates) lane(code uint64) *sync.RWMutex {
	code ^= code >> 32
	code ^= code >> 16
	code ^= code >> 8
	return &g.lanes[code&(structuralLanes-1)].mu
}

type structuralGuard struct {
	g    *structuralGates
	lane *sync.RWMutex
}

// While operations are serial, the writer mutex alone excludes structural
// changes. Contention permanently enables independent lanes. The transition
// holds writer, so it cannot race a structural operation that skipped lanes.
// A serial guard excludes every writer: parallel is rechecked under writer,
// because another writer may enable lanes between the first check and TryLock.
func (s *structuralLock) acquire(code uint64) structuralGuard {
	g := s.gates()
	if !g.parallel.Load() {
		if g.writer.TryLock() {
			if !g.parallel.Load() {
				return structuralGuard{g: g}
			}
			g.writer.Unlock()
		} else {
			g.writer.Lock()
			g.parallel.Store(true)
			g.writer.Unlock()
		}
	}
	lane := g.lane(code)
	lane.RLock()
	return structuralGuard{lane: lane}
}

func (g structuralGuard) unlock() {
	if g.lane != nil {
		g.lane.RUnlock()
	} else {
		g.g.writer.Unlock()
	}
}

func (s *structuralLock) RLock()   { s.lane(0).RLock() }
func (s *structuralLock) RUnlock() { s.lane(0).RUnlock() }

func (s *structuralLock) Lock() {
	g := s.gates()
	g.writer.Lock()
	if g.parallel.Load() {
		for i := range g.lanes {
			g.lanes[i].mu.Lock()
		}
	}
}

func (s *structuralLock) Unlock() {
	g := s.state.Load()
	if g.parallel.Load() {
		for i := len(g.lanes) - 1; i >= 0; i-- {
			g.lanes[i].mu.Unlock()
		}
	}
	g.writer.Unlock()
}
