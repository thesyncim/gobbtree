package gobbtree

// Stable, typed slabs contain numeric data only. Integer free lists permit
// reuse without making the collector trace one allocation per search column.
type numericSlabs[T numericColumns] struct {
	blocks [][]T
	used   int
	free   []uint32
}

func (a *numericSlabs[T]) alloc() (*T, uint32) {
	if count := len(a.free); count != 0 {
		id := a.free[count-1]
		a.free = a.free[:count-1]
		index := id - 1
		return &a.blocks[index>>8][index&255], id
	}
	if len(a.blocks) == 0 || a.used == len(a.blocks[len(a.blocks)-1]) {
		if len(a.blocks) >= 1<<24 {
			panic("gobbtree: numeric arena exhausted")
		}
		size := 1
		if len(a.blocks) != 0 {
			size = min(256, 2*len(a.blocks[len(a.blocks)-1]))
		}
		a.blocks = append(a.blocks, make([]T, size))
		a.used = 0
	}
	index := uint32(len(a.blocks)-1)<<8 | uint32(a.used)
	a.used++
	return &a.blocks[index>>8][index&255], index + 1
}
func (a *numericSlabs[T]) release(id uint32) { a.free = append(a.free, id) }

type nodeArena struct {
	columns numericSlabs[numericColumns]
}
