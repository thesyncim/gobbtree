package gobbtree

import (
	"sync/atomic"
	"unsafe"
)

// counterCells is how many cells concurrent leaf writers spread a count over.
const counterCells = 16

// counter is a map's entry count. Serial and structural writers adjust base;
// concurrent leaf writers add to one of a few cells instead, so writers on
// different cores rarely write one cache line: a single shared count costs
// every insert and removal a line that all writers contend for. Load sums
// base and the cells, so it is exact whenever no write runs concurrently.
type counter struct {
	base  atomic.Int64
	cells atomic.Pointer[[counterCells]counterCell]
}

// A cell fills two 64-byte lines, which some cores fetch in pairs.
type counterCell struct {
	value atomic.Int64
	_     [120]byte
}

func (c *counter) Add(delta int64) { c.base.Add(delta) }

// storeAdd is Add for the only writer, where stores stay ordered.
func (c *counter) storeAdd(delta int64) {
	*(*int64)(unsafe.Pointer(&c.base)) = c.base.Load() + delta
}

func (c *counter) Load() int64 {
	sum := c.base.Load()
	if cells := c.cells.Load(); cells != nil {
		for i := range cells {
			sum += cells[i].value.Load()
		}
	}
	return sum
}

// Store sets the count. No concurrent leaf writer may run.
func (c *counter) Store(value int64) {
	if cells := c.cells.Load(); cells != nil {
		for i := range cells {
			cells[i].value.Store(0)
		}
	}
	c.base.Store(value)
}

// addSpread adds delta for a concurrent leaf writer, to a cell chosen by the
// writing goroutine's stack address: a goroutine keeps writing the cell its
// core already holds, and goroutines' stacks lie at least 2 KiB apart.
func (c *counter) addSpread(delta int64) {
	cells := c.cells.Load()
	if cells == nil {
		cells = new([counterCells]counterCell)
		if !c.cells.CompareAndSwap(nil, cells) {
			cells = c.cells.Load()
		}
	}
	var probe byte
	h := uintptr(unsafe.Pointer(&probe)) >> 11
	h ^= h >> 4
	h ^= h >> 8
	cells[h%counterCells].value.Add(delta)
}
