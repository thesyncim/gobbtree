package store

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"unsafe"
)

// heapPage is a page of records, upstream's HeapPage (heap_page.rs): its
// header, then records, then room.
//
//	| next | top | live | records … | room |
//
// Records go one after another, from recordsOffset to top, each where top
// was. One that dies leaves a hole, until its leaf slides the page's
// records together (leaf_heap.go).
//
//   - A chain of pages is a leaf's (leaf_heap.go): next links each to the
//     one before it. Only the goroutine holding the chain's leaf
//     exclusively reads or writes next and top, and gives the page back.
//   - live counts the bytes of the page's live records. A record of another
//     leaf may lie in a leaf's chain, once entries move between leaves; a
//     goroutine holding that other leaf kills it, through live, an atomic.
//   - An orphan is a page no chain holds any more: a compaction moved its
//     leaf's records out, and other leaves' are left. live keeps a bit for
//     it, so that whichever goroutine empties an orphan, by its last kill or
//     by orphaning it, gives it back: one of them, once.
//
// The header is a typed Go struct, so the collector sees next; the records'
// bytes, which hold no pointer, follow it in the same object, which a
// geometry's heapType makes. A record finds its page by the offset its
// header holds (record.go), as Go doesn't align a page to its size.
type heapPage struct {
	// next is the page before this one in its chain; nil at the chain's end.
	next *heapPage
	// top is where the next record goes, from the page's start.
	top uint32
	// live is the bytes of the page's live records, and orphanBit once no
	// chain holds the page.
	live atomic.Uint32
}

const (
	// recordsOffset is where a heap page's records start: past its header.
	recordsOffset = int(unsafe.Sizeof(heapPage{}))
	// orphanBit is the bit of live that marks an orphan.
	orphanBit = 1 << 31
	// mallocHeader is what Go's allocator puts before an object over 512
	// bytes that holds pointers, as each page does: a page object that
	// size short of the page size takes the page size of Go's heap. Without
	// the header the object still rounds up to that size.
	mallocHeader = 8
	// DefaultHeapPageSize is a store's heap page size by default, upstream's
	// HEAP_PAGE_SIZE, its leaves' size, 1 KiB.
	DefaultHeapPageSize = 1024
	minHeapPageSize     = 1024
	// maxHeapPageSize keeps a record's offset in its page, and a key's
	// length, to 2 bytes.
	maxHeapPageSize = 64 << 10
)

// geometry is a store's page sizes, and the Go types of its pages.
type geometry struct {
	// pageSize is what a page takes of Go's heap, which the budget counts.
	pageSize int
	// heapObject is a heap page's size as a Go object, and room the largest
	// record it holds, past its header, upstream's ROOM.
	heapObject, room int
	// overflowObject is an overflow page's size as a Go object, and
	// overflowRoom the value bytes it holds, past its link.
	overflowObject, overflowRoom int
	heapType, overflowType       reflect.Type
}

// newGeometry is the geometry of pages of size bytes, a power of two from
// 1 KiB to 64 KiB.
func newGeometry(size int) geometry {
	if size&(size-1) != 0 || size < minHeapPageSize || size > maxHeapPageSize {
		panic(fmt.Sprintf("store: a heap page size is a power of two from 1 KiB to 64 KiB, not %d", size))
	}
	g := geometry{pageSize: size, heapObject: size - mallocHeader, overflowObject: size - mallocHeader}
	g.room = g.heapObject - recordsOffset
	g.overflowRoom = g.overflowObject - overflowOffset
	g.heapType = pageType(reflect.TypeFor[heapPage](), g.room)
	g.overflowType = pageType(reflect.TypeFor[overflowPage](), g.overflowRoom)
	return g
}

// pageType is the type of a page object: header, then bytes of data.
func pageType(header reflect.Type, data int) reflect.Type {
	return reflect.StructOf([]reflect.StructField{
		{Name: "Header", Type: header},
		{Name: "Data", Type: reflect.ArrayOf(data, reflect.TypeFor[byte]())},
	})
}

// newHeapPage is a new heap page, with no record, in no chain yet.
func (g *geometry) newHeapPage() *heapPage {
	p := (*heapPage)(reflect.New(g.heapType).UnsafePointer())
	p.top = uint32(recordsOffset)
	return p
}

// at is the address offset bytes into the page, which lies within it.
func (p *heapPage) at(offset int) unsafe.Pointer { return unsafe.Add(unsafe.Pointer(p), offset) }

// records is the page's bytes past its header, for scribbling over.
func (p *heapPage) records(g *geometry) []byte {
	return unsafe.Slice((*byte)(p.at(recordsOffset)), g.room)
}

// The fields below are the chain holder's alone (heapPage's documentation):
// the caller holds the page's leaf exclusively.

// roomLeft is the bytes free after the last record.
func (p *heapPage) roomLeft(g *geometry) int { return g.heapObject - int(p.top) }

// used is the bytes records took: the live ones' and the holes'.
func (p *heapPage) used() int { return int(p.top) - recordsOffset }

// liveBytes is the bytes of the page's live records.
func (p *heapPage) liveBytes() int { return int(p.live.Load() &^ orphanBit) }

// appendRecord makes room for a record of size bytes, live, after the last
// one, and returns its offset; the page has the room.
func (p *heapPage) appendRecord(g *geometry, size int) int {
	if size > p.roomLeft(g) {
		panic("store: a record goes where it fits")
	}
	top := int(p.top)
	p.top = uint32(top + size)
	p.live.Add(uint32(size))
	return top
}

// setUsed sets where the next record goes to used bytes past the first:
// the page's records, once slid together, end there.
func (p *heapPage) setUsed(used int) { p.top = uint32(recordsOffset + used) }

// moveLive tells the page and to, both of one chain, where only its leaf's
// records live, that size bytes of live records moved from it to to.
func (p *heapPage) moveLive(to *heapPage, size int) {
	if p != to {
		p.live.Add(^uint32(size - 1))
		to.live.Add(uint32(size))
	}
}

// kill tells the page that size bytes of its live records died; whether
// the page is an orphan they left empty, which the caller gives back.
func (p *heapPage) kill(size int) bool {
	return p.live.Add(^uint32(size-1)) == orphanBit
}

// orphan lets the page go from its chain, which no longer holds it;
// whether it was empty, which the caller gives back.
func (p *heapPage) orphan() bool {
	p.next = nil
	return p.live.Or(orphanBit) == 0
}
