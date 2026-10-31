package store

import (
	"reflect"
	"sync"
	"sync/atomic"
	"unsafe"
)

// overflowPage is a page of a value too long for its record (record.go),
// upstream's Overflow (overflow.rs): its link to the next page, then value
// bytes, every page full but, perhaps, the last.
//
//	| next | value bytes … |
//
// The pages are their record's: a record that moves (leaf_heap.go) takes
// its link to them along, and leaves them where they are; they go back when
// it dies. No chain holds them, and only their record links them, so a
// goroutine reads them only while it holds the record's leaf, as it reads
// the record. As a heap page's, the link is a typed pointer and the bytes
// follow it in the same object (heap_page.go).
type overflowPage struct {
	// next is the value's next page; nil in the last.
	next *overflowPage
}

// overflowOffset is where an overflow page's value bytes start.
const overflowOffset = int(unsafe.Sizeof(overflowPage{}))

// data is the page's value bytes, all g.overflowRoom of them.
func (p *overflowPage) data(g *geometry) []byte {
	return unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(p), overflowOffset)), g.overflowRoom)
}

// overflowLink is what a record keeps of its value's overflow pages: their
// id in its store's overflow table; ok is false for a value its record
// keeps whole.
type overflowLink struct {
	id uint32
	ok bool
}

// overflowTable holds the first overflow page of each value that has
// some, by the id its record keeps in place of upstream's address: no Go
// pointer lies in a page's bytes. Ids go back when their record dies, and
// are used again; a reader looks an id up only while it holds its record's
// leaf, which the record's writer held after it took the id and before it
// gave it back, so the id names the record's pages whenever it is read.
type overflowTable struct {
	mu   sync.Mutex
	free []uint32
	// pages is indexed by id, replaced by a longer copy as it fills: a
	// reader may still read an old copy, whose ids it reads are the same.
	pages atomic.Pointer[[]*overflowPage]
}

// add takes an id for first, a value's first page.
func (t *overflowTable) add(first *overflowPage) uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var pages []*overflowPage
	if p := t.pages.Load(); p != nil {
		pages = *p
	}
	if n := len(t.free); n > 0 {
		id := t.free[n-1]
		t.free = t.free[:n-1]
		pages[id] = first
		return id
	}
	id := uint32(len(pages))
	if len(pages) == cap(pages) {
		grown := make([]*overflowPage, len(pages), max(64, 2*cap(pages)))
		copy(grown, pages)
		pages = grown
	}
	pages = append(pages, first)
	t.pages.Store(&pages)
	return id
}

// page is the first page of id, which the caller's record links.
func (t *overflowTable) page(id uint32) *overflowPage { return (*t.pages.Load())[id] }

// remove gives id back, and returns its first page.
func (t *overflowTable) remove(id uint32) *overflowPage {
	t.mu.Lock()
	defer t.mu.Unlock()
	pages := *t.pages.Load()
	first := pages[id]
	pages[id] = nil
	t.free = append(t.free, id)
	return first
}

// writeOverflow writes bytes, not empty, into new overflow pages, taking
// each from the budget, and returns the first; false, with the pages it
// took given back, if the budget refuses one. From the last page back to
// the first, each links the next.
func (h *recordHeap) writeOverflowPages(bytes []byte) (*overflowPage, bool) {
	g := &h.g
	var next *overflowPage
	for end := len(bytes); end > 0; {
		start := (end - 1) / g.overflowRoom * g.overflowRoom
		if !h.budget.TakePages(1) {
			h.freeOverflowPages(next)
			return nil, false
		}
		page := (*overflowPage)(reflect.New(g.overflowType).UnsafePointer())
		page.next = next
		copy(page.data(g), bytes[start:end])
		next, end = page, start
	}
	return next, true
}

// appendOverflow appends the n bytes the pages from first hold to buf.
func (g *geometry) appendOverflow(first *overflowPage, n int, buf []byte) []byte {
	for page := first; n > 0; page = page.next {
		part := min(n, g.overflowRoom)
		buf = append(buf, page.data(g)[:part]...)
		n -= part
	}
	return buf
}

// freeOverflowPages gives the pages from first back to the budget: their
// record is dead, or never went in, and no goroutine reads them again.
func (h *recordHeap) freeOverflowPages(first *overflowPage) {
	for page := first; page != nil; {
		next := page.next
		page.next = nil
		if scribble {
			fill(page.data(&h.g))
		}
		h.budget.GivePages(1)
		page = next
	}
}
