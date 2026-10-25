package gobbtree

import (
	"cmp"
	"sync"
	"sync/atomic"
)

const structuralLanes = 16

type structuralLane struct {
	mu sync.RWMutex
	_  [40]byte
}

type structuralGates struct {
	writer sync.Mutex
	_      [56]byte
	lanes  [structuralLanes]structuralLane
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
	g := new(structuralGates)
	if s.state.CompareAndSwap(nil, g) {
		return g
	}
	return s.state.Load()
}

func structuralLaneCode[K cmp.Ordered](key K) uint64 {
	if code, ok := any(key).(uint64); ok {
		return code
	}
	return structuralLaneCodeSlow(key)
}
func structuralLaneCodeSlow[K cmp.Ordered](key K) uint64 {
	code := numericCode(key)
	if str, ok := any(key).(string); ok && len(str) > 8 {
		code ^= stringCode(str, max(0, len(str)-8))
	}
	return code
}

func (s *structuralLock) lane(code uint64) *sync.RWMutex {
	g := s.state.Load()
	if g == nil {
		return s.slowLane(code)
	}
	code ^= code >> 32
	code ^= code >> 16
	code ^= code >> 8
	return &g.lanes[code&(structuralLanes-1)].mu
}

func (s *structuralLock) slowLane(code uint64) *sync.RWMutex {
	s.gates()
	return s.lane(code)
}

func (s *structuralLock) RLock()   { s.lane(0).RLock() }
func (s *structuralLock) RUnlock() { s.lane(0).RUnlock() }

func (s *structuralLock) Lock() {
	g := s.gates()
	g.writer.Lock()
	for i := range g.lanes {
		g.lanes[i].mu.Lock()
	}
}

func (s *structuralLock) Unlock() {
	g := s.state.Load()
	for i := len(g.lanes) - 1; i >= 0; i-- {
		g.lanes[i].mu.Unlock()
	}
	g.writer.Unlock()
}
