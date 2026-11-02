//go:build unix

package store

import (
	"runtime"
	"testing"
	"time"
)

// chunkCount is how many chunks a holds mapped.
func (a *pageArena) chunkCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.chunks)
}

// Each chunk an arena maps is twice the one before, from minChunk, up to
// maxChunk, however many it maps.
func TestChunksGrowToTheirLargest(t *testing.T) {
	for n := range 200 {
		want := maxChunk
		if n < 4 {
			want = minChunk << n
		}
		if got := chunkSize(n); got != want {
			t.Fatalf("chunk %d: %d bytes, want %d", n, got, want)
		}
	}
}

// A store that maps its pages keeps them out of Go's heap: what the heap
// holds of it, once collected, is its nodes, not its pages.
func TestMappedPagesLieOutsideGoHeap(t *testing.T) {
	if mapEveryStore {
		t.Skip("every store maps its pages")
	}
	const entries = 20000
	heldBy := func(mapped bool) (held int64, pages int) {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		budget := &counting{}
		s := NewWithConfig(Config{MappedPages: mapped}, budget)
		for i := range entries {
			mustInsert(t, s, key(i), value(i, 0, 32))
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(s)
		return int64(after.HeapAlloc) - int64(before.HeapAlloc), budget.out()
	}
	goHeld, pages := heldBy(false)
	mappedHeld, mappedPages := heldBy(true)
	if mappedPages != pages {
		t.Fatalf("the same entries took %d mapped pages, %d of Go's", mappedPages, pages)
	}
	if gone := goHeld - mappedHeld; gone < int64(pages*page)/2 {
		t.Fatalf("Go's heap held %d bytes of a store of Go's pages, %d of one that maps its %d pages", goHeld, mappedHeld, pages)
	}
}

// A store that maps its pages unmaps them all as it clears, overflow
// pages among them, and maps more as it fills again.
func TestMappedStoreUnmapsAsItClears(t *testing.T) {
	pages := &counting{}
	s := NewWithConfig(Config{MappedPages: true}, pages)
	a := s.heap.arena
	for round := range 3 {
		for i := range 3000 {
			mustInsert(t, s, key(i), value(i, round, [3]int{24, 300, 3 * page}[i%3]))
		}
		if a.chunkCount() == 0 {
			t.Fatal("a store that maps its pages mapped none")
		}
		for i := range 3000 {
			assertGet(t, s, key(i), value(i, round, [3]int{24, 300, 3 * page}[i%3]), true)
		}
		checkLive(t, s)
		assertCleared(t, s, pages)
		if n := a.chunkCount(); n != 0 {
			t.Fatalf("round %d: a cleared store holds %d chunks mapped", round, n)
		}
	}
}

// The collector unmaps the pages of a store it finds unreachable.
func TestMappedStoreUnmapsOnceUnreachable(t *testing.T) {
	a := func() *pageArena {
		s := NewWithConfig(Config{MappedPages: true}, nil)
		for i := range 3000 {
			mustInsert(t, s, key(i), value(i, 0, 40))
		}
		return s.heap.arena
	}()
	if a.chunkCount() == 0 {
		t.Fatal("a store that maps its pages mapped none")
	}
	for range 200 {
		runtime.GC()
		if a.chunkCount() == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("an unreachable store holds %d chunks mapped", a.chunkCount())
}
