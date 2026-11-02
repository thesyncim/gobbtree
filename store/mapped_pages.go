//go:build unix

package store

import (
	"math/bits"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// pageArena is the pages of a store whose config asks for MappedPages,
// heap pages and overflow pages alike: memory the store maps itself, in
// chunks, outside Go's heap, which the collector neither scans nor counts
// toward its next collection. No Go pointer lies in a page (heap_page.go,
// overflow.go): a page's link to the next is to the arena's memory too.
//
//   - A page the store gives back goes on one of the arena's free lists,
//     for the store's next page, and stays mapped: a lookup racing the
//     write that freed it may still read it, and its leaf's version, which
//     that write moved, rejects what it read, as it rejects a record that
//     moved (peekValue). A racing read reads only within a record its
//     leaf's version vouched for, so within a page.
//   - Clear, which no other goroutine runs alongside, unmaps every chunk,
//     and so does the collector once the store is unreachable: each of the
//     store's methods keeps it reachable until its last read of a page, a
//     scan by its walk, which it advances or closes after each loop body.
type pageArena struct {
	// free is the pages given back, on lists goroutines pick at random, so
	// that they rarely wait on one another's; first, for the lists to start
	// lines of their own.
	free [freeLists]freeList
	size int
	// mu maps chunks, one goroutine at a time: chunks is them, and current
	// the newest, whose pages goroutines carve out.
	mu      sync.Mutex
	chunks  []*chunk
	current atomic.Pointer[chunk]
}

// chunk is memory an arena mapped, its pages handed out from the start:
// those before used have been, or are being.
type chunk struct {
	mem  []byte
	used atomic.Int64
}

// freeList is pages given back, as many as count says, for a goroutine
// to check without the lock; padded to a cache line of its own.
type freeList struct {
	mu    sync.Mutex
	count atomic.Int64
	pages []unsafe.Pointer
	_     [cacheLine - (unsafe.Sizeof(sync.Mutex{})+unsafe.Sizeof(atomic.Int64{})+unsafe.Sizeof([]unsafe.Pointer(nil)))%cacheLine]byte
}

const (
	// A store's first chunk is minChunk bytes, each next one twice the
	// last, up to maxChunk: a small store maps little.
	minChunk = 64 << 10
	maxChunk = 1 << 20
	// freeLists is how many free lists an arena keeps.
	freeLists = 16
	// cacheLine is the largest of the targets' cache lines.
	cacheLine = 128
)

// handedAgain counts, while scribble is set, the pages arenas handed out
// again after they were given back.
var handedAgain atomic.Int64

// systemPage is the system's page size.
var systemPage = syscall.Getpagesize()

// chunkSize is the size of the chunk an arena maps after n.
func chunkSize(n int) int { return minChunk << min(n, bits.Len(maxChunk/minChunk)-1) }

// newPageArena is an arena of pages of size bytes; nil where the system
// can't map them.
func newPageArena(size int) *pageArena { return &pageArena{size: size} }

// take is a page's memory: one given back, holding what it held last, from
// the first list with one, from a list picked at random; else the current
// chunk's next; nil where the system maps no more.
func (a *pageArena) take() unsafe.Pointer {
	from := rand.Uint32()
	for i := range uint32(freeLists) {
		l := &a.free[(from+i)%freeLists]
		if l.count.Load() == 0 {
			continue
		}
		l.mu.Lock()
		if n := len(l.pages) - 1; n >= 0 {
			p := l.pages[n]
			l.pages = l.pages[:n]
			l.count.Store(int64(n))
			l.mu.Unlock()
			if scribble {
				handedAgain.Add(1)
			}
			return p
		}
		l.mu.Unlock()
	}
	return a.carve()
}

// carve is the current chunk's next page, mapping a chunk where it has
// none; nil where the system maps no more.
func (a *pageArena) carve() unsafe.Pointer {
	size := int64(a.size)
	for {
		c := a.current.Load()
		if c != nil {
			if at := c.used.Add(size) - size; at < int64(len(c.mem)) {
				return unsafe.Pointer(&c.mem[at])
			}
		}
		a.mu.Lock()
		if a.current.Load() == c {
			size := chunkSize(len(a.chunks))
			mem, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
			if err != nil {
				a.mu.Unlock()
				return nil
			}
			// Faulted in now, a system page at a time: the arena's pages
			// are smaller than the system's, and handed to goroutines that,
			// writing their first records at once, would fault the same
			// page together.
			for off := 0; off < size; off += systemPage {
				mem[off] = 0
			}
			c = &chunk{mem: mem}
			a.chunks = append(a.chunks, c)
			a.current.Store(c)
		}
		a.mu.Unlock()
	}
}

// give takes back the page at p, for the store's next page, onto a list
// picked at random.
func (a *pageArena) give(p unsafe.Pointer) {
	l := &a.free[rand.Uint32()%freeLists]
	l.mu.Lock()
	l.pages = append(l.pages, p)
	l.count.Store(int64(len(l.pages)))
	l.mu.Unlock()
}

// release unmaps every chunk: no goroutine reads a page of the arena's
// again, nor holds one, as it clears or goes.
func (a *pageArena) release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.chunks {
		if err := syscall.Munmap(c.mem); err != nil {
			panic("store: unmapping a store's pages: " + err.Error())
		}
	}
	a.chunks = nil
	a.current.Store(nil)
	for i := range a.free {
		l := &a.free[i]
		l.pages = nil
		l.count.Store(0)
	}
}
