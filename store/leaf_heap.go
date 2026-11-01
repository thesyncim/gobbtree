package store

import (
	"encoding/binary"
	"slices"
	"sync/atomic"
	"unsafe"

	"github.com/thesyncim/gobbtree"
)

// leafHeap is each leaf's annex in a store's map (gobbtree.LeafAnnex),
// upstream's LeafHeap (leaf_heap.rs): the chain of heap pages its records
// go into (heap_page.go), and its generation. recordHeap runs what a store
// does to a leaf's records, while the map holds the leaf exclusively.
//
//   - A record goes after the last one in the leaf's newest page, the
//     chain's head; a page goes in front when the head lacks room.
//   - A split moves no record: the new leaf starts with no page, and its
//     records lie in the old leaf's chain. Both leaves take the next
//     generation.
//   - A merge splices the leaving leaf's chain in behind the head.
//   - A record that dies leaves a hole. Once a page of the leaf's own chain
//     is down to half its bytes live, and the chain too, or a record finds
//     no room in a chain a quarter holes, the leaf compacts, in place: in
//     each page where only its own records live, it slides them together;
//     it gives back the pages where none of its records live, or orphans
//     them while other leaves' do. A page where other leaves' records live
//     keeps its holes until they go. Compacting takes no page, so a store
//     whose budget refuses pages still takes its holes back.
//   - A record two generations behind its leaf that lies in another leaf's
//     chain moves into its own leaf's head, once a generation: the leaf's
//     key range split twice since it was written, so its page holds the
//     records of leaves further and further off. One generation behind, it
//     shares a page with a neighbour's.
//   - A value too long for its record goes into overflow pages
//     (overflow.go), written before its leaf is held, which its record
//     links: a record that moves takes the link along, and one that dies
//     gives the pages back.
//
// Only a goroutine holding a leaf exclusively moves, changes or kills its
// records, and reads or changes its chain: a record's own leaf, that is,
// the leaf of the key that points to it, which may lie in another leaf's
// chain. Pages hold a count of their live bytes for that (heap_page.go).
type leafHeap struct {
	// heap is the store's, for Release, which takes no allocator, unlike
	// upstream's: set as the leaf first takes a page, and carried by
	// splits and merges.
	heap *recordHeap
	// head is the chain's newest page, where records go; nil without one.
	head *heapPage
	// generation is how many times the leaf split, with the leaves it split
	// from, wrapping, and pulled the generation it last pulled its records
	// in at.
	generation, pulled uint8
}

// storeMap is a store's map, and storeLeaf one of its leaves, held
// exclusively.
type (
	storeMap  = gobbtree.AnnexedMapOf[storeKey, struct{}, leafHeap, *leafHeap]
	storeLeaf = gobbtree.AnnexedLeaf[storeKey, leafHeap]
)

// Split gives both leaves the next generation.
func (a *leafHeap) Split(right *leafHeap) {
	a.generation++
	right.generation = a.generation
	right.pulled = a.pulled
	right.heap = a.heap
}

// Merge splices right's chain in behind the head.
func (a *leafHeap) Merge(right *leafHeap) {
	if a.heap == nil {
		a.heap = right.heap
	}
	if right.head == nil {
		return
	}
	if a.head == nil {
		a.head = right.head
		return
	}
	tail := right.head
	for tail.next != nil {
		tail = tail.next
	}
	tail.next = a.head.next
	a.head.next = right.head
}

// Release gives the chain's pages back, as the map goes: no goroutine
// reads a record again, and each page is in this chain alone.
func (a *leafHeap) Release() {
	for page := a.head; page != nil; {
		next := page.next
		a.heap.freePage(page)
		page = next
	}
}

// room makes room for a record of size bytes, live, in the chain's head
// page, else in a page of the chain that has it, which becomes the head;
// false without one.
func (a *leafHeap) room(g *geometry, size int) (*heapPage, int, bool) {
	var before *heapPage
	for page := a.head; page != nil; before, page = page, page.next {
		if page.roomLeft(g) >= size {
			if before != nil {
				before.next = page.next
				page.next = a.head
				a.head = page
			}
			return page, page.appendRecord(g, size), true
		}
	}
	return nil, 0, false
}

// chainUse is how much of a chain's pages records took, and how much of
// that is live.
type chainUse struct{ used, live int }

func (a *leafHeap) chainUse() chainUse {
	var u chainUse
	for page := a.head; page != nil; page = page.next {
		u.used += page.used()
		u.live += page.liveBytes()
	}
	return u
}

// isHalfHoles reports whether at least half of what records took is holes.
func (u chainUse) isHalfHoles() bool { return u.used > 0 && u.live*2 <= u.used }

// isQuarterHoles reports whether at least a quarter of what records took is
// holes: worth a compaction, which moves at most three live bytes a byte of
// holes, to save a page.
func (u chainUse) isQuarterHoles() bool { return u.used > 0 && u.live*4 <= u.used*3 }

// holds reports whether the chain holds page.
func (a *leafHeap) holds(page *heapPage) bool {
	for p := a.head; p != nil; p = p.next {
		if p == page {
			return true
		}
	}
	return false
}

// holdsAt reports whether the chain holds the page the record at p lies
// in, by its address against the pages', each a heap object of
// g.heapObject bytes: as upstream rounds a record's address down to its
// page, it reads nothing of the record.
func (a *leafHeap) holdsAt(g *geometry, p unsafe.Pointer) bool {
	for page := a.head; page != nil; page = page.next {
		if uintptr(p)-uintptr(unsafe.Pointer(page)) < uintptr(g.heapObject) {
			return true
		}
	}
	return false
}

// recordHeap is where a store's records go: its leaves' heaps, whose pages
// its budget counts, upstream's RecordHeap.
type recordHeap struct {
	g      geometry
	budget Budget
	// orphans is how many orphans hold live records (heap_page.go).
	orphans atomic.Int64
	// overflowed is whether a value took overflow pages since the store was
	// made or cleared.
	overflowed atomic.Bool
	table      overflowTable
}

// scribble, which tests set, overwrites with 0xA5 each page the store
// gives back, and each record's bytes as it dies, before its page counts
// it dead, so that a read of either after fails visibly, as upstream's
// tests scribble over the pages their allocator takes back.
var scribble bool

// slidFirst counts, while scribble is set, the records compactions slid
// that were their leaf's first key's, for tests to check they slide some.
var slidFirst atomic.Int64

func fill(b []byte) {
	for i := range b {
		b[i] = 0xA5
	}
}

// adopt makes leaf's annex this heap's, before the leaf takes a page.
func (h *recordHeap) adopt(leaf storeLeaf) *leafHeap {
	a := leaf.Annex()
	if a.heap == nil {
		a.heap = h
	}
	return a
}

// newPage is a new heap page, false if the budget refuses it.
func (h *recordHeap) newPage() (*heapPage, bool) {
	if !h.budget.TakePages(1) {
		return nil, false
	}
	return h.g.newHeapPage(), true
}

// freePage gives page back to the budget: no record lives in it, and no
// chain holds it. The collector frees it once nothing points into it.
func (h *recordHeap) freePage(page *heapPage) {
	page.next = nil
	if scribble {
		fill(page.records(&h.g))
	}
	h.budget.GivePages(1)
}

// roomInNewPage makes room for a record of size bytes, live, in a new head
// page; false if the budget refuses it.
func (h *recordHeap) roomInNewPage(a *leafHeap, size int) (*heapPage, int, bool) {
	page, ok := h.newPage()
	if !ok {
		return nil, 0, false
	}
	page.next = a.head
	a.head = page
	return page, page.appendRecord(&h.g, size), true
}

// killsAtClear reports whether clearing the store must kill its records,
// for pages outside the chains to go back: orphans that hold live records,
// or overflow pages, once any value took some.
func (h *recordHeap) killsAtClear() bool {
	return h.orphans.Load() > 0 || h.overflowed.Load()
}

// writeOverflow writes the overflow pages of the record of a key of keyLen
// and value, for write: the value's bytes the record doesn't keep; none if
// it keeps them all; false if the budget refuses a page.
func (h *recordHeap) writeOverflow(keyLen int, value []byte) (overflowLink, bool) {
	kept := h.g.keptLen(keyLen, len(value))
	if kept == len(value) {
		return overflowLink{}, true
	}
	first, ok := h.writeOverflowPages(value[kept:])
	if !ok {
		return overflowLink{}, false
	}
	if !h.overflowed.Load() {
		h.overflowed.Store(true)
	}
	return overflowLink{id: h.table.add(first), ok: true}, true
}

// freeOverflow gives back the overflow pages link names: their record is
// dead, or never went in, and no goroutine reads them.
func (h *recordHeap) freeOverflow(link overflowLink) {
	if link.ok {
		h.freeOverflowPages(h.table.remove(link.id))
	}
}

// write writes the record of key and value, whose overflow pages
// writeOverflow wrote, into leaf's heap, for a key of leaf to point to;
// false if a page it needs is not to be had. The leaf's records may move
// first, for room, or to their own leaf (leafHeap's documentation).
func (h *recordHeap) write(leaf storeLeaf, key, value []byte, link overflowLink) (record, bool) {
	a := h.adopt(leaf)
	h.pullStale(leaf)
	page, offset, ok := h.roomFor(leaf, h.g.sizeOf(len(key), len(value)))
	if !ok {
		return record{}, false
	}
	return h.g.writeRecord(page, offset, key, value, link, a.generation), true
}

// overwrite gives leaf's key i, whose bytes are key, value in place of its
// own: in its record if it keeps its value whole and the new one is no
// longer, else in a new record (write); false if a page it needs is not to
// be had, the leaf as it was.
func (h *recordHeap) overwrite(leaf storeLeaf, i int, key, value []byte, link overflowLink) bool {
	r := leaf.Keys()[i].record()
	// Its header once: a record keeps its value whole, with no link, where
	// the whole fits a page's room (keptLen).
	w := r.headWord()
	keyLen, valueLen := int(uint16(w)), int(w>>32)
	if recordHeader+keyLen+valueLen <= h.g.room && len(value) <= valueLen {
		r.overwriteValue(keyLen, valueLen, value)
		if len(value) < valueLen {
			// The bytes past the new value are a hole.
			h.killIn(r.page(), unsafe.Add(r.at, recordHeader+keyLen+len(value)), valueLen-len(value))
		}
		return true
	}
	fresh, ok := h.write(leaf, key, value, link)
	if !ok {
		return false
	}
	// The write may have moved the key's record: the key holds where.
	old := leaf.Keys()[i].record()
	leaf.ReplaceKey(i, storeKey{fresh.at})
	h.kill(leaf, old)
	return true
}

// beforeRemoval runs as removed, a key of leaf, is about to go. If its
// record lies in the leaf's chain, and the page, and the chain, are half
// holes once it dies, the leaf compacts, the record left as it is: the map
// reads the key until the removal returns, and the record dies only then
// (killRecord). Its page counts it live meanwhile, so no compaction slides
// over it or gives the page back.
func (h *recordHeap) beforeRemoval(leaf storeLeaf, removed storeKey) {
	a := h.adopt(leaf)
	r := removed.record()
	// Its header once: its page and its size.
	w := r.headWord()
	page, size := (*heapPage)(unsafe.Add(r.at, -int(uint16(w>>16)))), h.g.sizeOf(int(uint16(w)), int(w>>32))
	// A page more than half live once the record dies is not half holes,
	// whichever chain holds it: its live bytes, an atomic, tell before the
	// chain is walked for whether it is this leaf's, whose top only its
	// holder reads.
	if (page.liveBytes()-size)*2 > h.g.room {
		return
	}
	if !a.holds(page) || (page.liveBytes()-size)*2 > page.used() {
		return
	}
	u := a.chainUse()
	if after := (chainUse{used: u.used, live: u.live - size}); after.isHalfHoles() {
		h.compact(leaf, removed.p)
	}
}

// killRecord kills r, which no key the store reads again points to, and no
// goroutine reads: of a key whose removal returned, of a key that points
// elsewhere now, or of a store that clears. Its overflow pages go back, and
// its bytes die in its page, which may be any leaf's then, or an orphan;
// whether that was an orphan they emptied, which went back too.
func (h *recordHeap) killRecord(r record) bool {
	g := &h.g
	// Its header once: its page, its size, and its link, if it has one.
	w := r.headWord()
	keyLen, offset, valueLen := int(uint16(w)), int(uint16(w>>16)), int(w>>32)
	page := (*heapPage)(unsafe.Add(r.at, -offset))
	kept := g.keptLen(keyLen, valueLen)
	size := recordHeader + keyLen + kept
	if kept < valueLen {
		size += linkSize
		h.freeOverflow(overflowLink{id: r.linkID(keyLen), ok: true})
	}
	return h.killIn(page, r.at, size)
}

// kill kills r, of a key of leaf that points elsewhere now (killRecord).
// If it lay in the leaf's own chain, the leaf compacts once half the page,
// and half the chain, are holes.
func (h *recordHeap) kill(leaf storeLeaf, r record) {
	page := r.page()
	// Before the kill: once it is done, another leaf's page may go.
	own := leaf.Annex().holds(page)
	if h.killRecord(r) || !own {
		return
	}
	if page.liveBytes()*2 <= page.used() && leaf.Annex().chainUse().isHalfHoles() {
		h.compact(leaf, nil)
	}
}

// killIn tells page that the size bytes at dead died; whether it was an
// orphan they emptied, which goes back to the budget then.
func (h *recordHeap) killIn(page *heapPage, dead unsafe.Pointer, size int) bool {
	if scribble {
		fill(unsafe.Slice((*byte)(dead), size))
	}
	if !page.kill(size) {
		return false
	}
	h.orphans.Add(-1)
	// An orphan a kill emptied is that kill's alone.
	h.freePage(page)
	return true
}

// roomFor makes room for a record of size bytes, live, in leaf's chain: in
// its head page, or another page with room, which becomes the head; else,
// once a compaction slid the records together if a quarter of the chain is
// holes, there; else in a new head page; else, if the budget has none to
// give, once a compaction took back whatever holes there are.
func (h *recordHeap) roomFor(leaf storeLeaf, size int) (*heapPage, int, bool) {
	a := leaf.Annex()
	if page, offset, ok := a.room(&h.g, size); ok {
		return page, offset, true
	}
	if a.chainUse().isQuarterHoles() {
		h.compact(leaf, nil)
		if page, offset, ok := a.room(&h.g, size); ok {
			return page, offset, true
		}
	}
	if page, offset, ok := h.roomInNewPage(a, size); ok {
		return page, offset, true
	}
	if u := a.chainUse(); u.live < u.used {
		h.compact(leaf, nil)
	}
	return a.room(&h.g, size)
}

// ownRecord is a record of a compacting leaf's key, in its chain, packed
// to sort as a word: its page's place in the chain, then its offset in the
// page, so that sorted, the records lie page by page, each page's by
// address, as upstream sorts them by address; then its key's index and its
// size, which the slide needs. A page's offsets and a record's size are
// below 64 KiB (maxHeapPageSize).
type ownRecord uint64

func packOwn(page, offset, index, size int) ownRecord {
	return ownRecord(uint64(page)<<48 | uint64(offset)<<32 | uint64(index)<<16 | uint64(size))
}

func (o ownRecord) page() int   { return int(o >> 48) }
func (o ownRecord) offset() int { return int(uint16(o >> 32)) }
func (o ownRecord) index() int  { return int(uint16(o >> 16)) }
func (o ownRecord) size() int   { return int(uint16(o)) }

// sortOwn sorts own, a leaf's records, at most a leaf's keys of them, by
// insertion, in place: as upstream's sort_unstable_by_key inlines its
// comparisons, where slices.Sort runs pdqsort through a dictionary.
func sortOwn(own []ownRecord) {
	for i := 1; i < len(own); i++ {
		o, j := own[i], i
		for ; j > 0 && own[j-1] > o; j-- {
			own[j] = own[j-1]
		}
		own[j] = o
	}
}

// placeIn is where the record at p lies in chain, its page's index and its
// offset in the page, or -1 if it lies in none of them: by its address
// against the pages', each a heap object of size bytes, as upstream rounds
// a record's address down to its page, reading nothing of the record.
func placeIn(chain []*heapPage, p unsafe.Pointer, size int) (int, int) {
	for j, page := range chain {
		if d := uintptr(p) - uintptr(unsafe.Pointer(page)); d < uintptr(size) {
			return j, int(d)
		}
	}
	return -1, 0
}

// compact slides the records of leaf's keys, but gone's, which stays where
// it is, together in the pages of its chain where no other leaf's record
// lives, in place, as one run from the first such page on; gives back
// those pages it empties, and the pages where none of its records live, or
// orphans them while other leaves' do (heap_page.go). The page the run
// ends in becomes the head. Then the leaf's records in other leaves' pages
// move in, where there is room, or pages to be had: those pages' leaves can
// slide them then, and orphans empty.
//
// A page's live bytes that its leaf's records don't account for are other
// leaves': a page whose live bytes they all account for has no other live
// record, and gets none, as only its leaf writes it.
func (h *recordHeap) compact(leaf storeLeaf, gone unsafe.Pointer) {
	g := &h.g
	a := leaf.Annex()
	var chainBuf [8]*heapPage
	chain := chainBuf[:0]
	for page := a.head; page != nil; page = page.next {
		chain = append(chain, page)
	}
	// The leaf's records in its chain, by address, each page's together,
	// and the keys whose records lie elsewhere, which stay where they are
	// through the slide. A record's address tells its page; its header is
	// read once, for its size.
	var ownBuf [64]ownRecord
	var awayBuf [64]uint16
	own, away := ownBuf[:0], awayBuf[:0]
	for i, k := range leaf.Keys() {
		if k.p == gone {
			continue
		}
		j, offset := placeIn(chain, k.p, g.heapObject)
		if j < 0 {
			away = append(away, uint16(i))
			continue
		}
		own = append(own, packOwn(j, offset, i, record{k.p}.size(g)))
	}
	sortOwn(own)
	// Page j's records are own[seg[j]:seg[j+1]].
	var segBuf [9]int
	seg := append(segBuf[:0], 0)
	for j := range chain {
		next := seg[j]
		for next < len(own) && own[next].page() == j {
			next++
		}
		seg = append(seg, next)
	}
	// The pages the run slides over, and those others' records keep, by
	// their places in the chain.
	var runBuf, keptBuf [8]int
	run, kept := runBuf[:0], keptBuf[:0]
	for j, page := range chain {
		ownBytes := 0
		for _, o := range own[seg[j]:seg[j+1]] {
			ownBytes += o.size()
		}
		switch {
		case ownBytes == 0:
			h.letGo(page)
		case page.liveBytes() == ownBytes:
			run = append(run, j)
		default:
			kept = append(kept, j)
		}
	}
	// The writer, a page of the run and where in it, never passes the
	// record it moves next: a record that doesn't fit after the writer in
	// its page lies in a later page.
	to, used := 0, 0
	for _, j := range run {
		from := chain[j]
		for _, o := range own[seg[j]:seg[j+1]] {
			size := o.size()
			if used+size > g.room {
				chain[run[to]].setUsed(used)
				to, used = to+1, 0
			}
			// The record, and its new place, which only records moved
			// already held, lie in pages where only the leaf's records
			// live; the map holds the leaf exclusively, so no goroutine
			// reads them.
			dst, at := chain[run[to]], from.at(o.offset())
			moved := record{at}.slideTo(dst, recordsOffset+used, size)
			leaf.ReplaceKey(o.index(), storeKey{moved.at})
			if scribble && o.index() == 0 && moved.at != at {
				slidFirst.Add(1)
			}
			from.moveLive(dst, size)
			used += size
		}
	}
	a.head = nil
	for i := len(kept) - 1; i >= 0; i-- {
		page := chain[kept[i]]
		page.next = a.head
		a.head = page
	}
	if to < len(run) {
		chain[run[to]].setUsed(used)
		for _, j := range run[to+1:] {
			// Every record of the page moved out, and no chain holds it
			// any more.
			h.freePage(chain[j])
		}
		for _, j := range run[:to+1] {
			page := chain[j]
			page.next = a.head
			a.head = page
		}
	}
	for _, i := range away {
		if !h.pull(leaf, int(i)) {
			return
		}
	}
}

// letGo lets page go from its chain: gives it back if no record lives in
// it, else orphans it.
func (h *recordHeap) letGo(page *heapPage) {
	if page.liveBytes() > 0 {
		h.orphans.Add(1)
		if !page.orphan() {
			return
		}
		// Another leaf killed its last record meanwhile.
		h.orphans.Add(-1)
	}
	h.freePage(page)
}

// pullStale moves the records of leaf's keys that lie in another leaf's
// chain and are two generations or more behind the leaf into its head,
// once a generation. If a page is not to be had, it stops, to try again.
func (h *recordHeap) pullStale(leaf storeLeaf) {
	a := leaf.Annex()
	generation := a.generation
	if a.pulled == generation {
		return
	}
	for i, k := range leaf.Keys() {
		// The chain first, from the record's address: only a record
		// elsewhere is read.
		if a.holdsAt(&h.g, k.p) || generation-k.record().generation() < 2 {
			continue
		}
		if !h.pull(leaf, i) {
			return
		}
	}
	a.pulled = generation
}

// pull moves the record of leaf's key i, which lies in another leaf's
// page, into a page of leaf's chain with room, or a new one; false if no
// page is to be had.
func (h *recordHeap) pull(leaf storeLeaf, i int) bool {
	r := leaf.Keys()[i].record()
	size := r.size(&h.g)
	a := leaf.Annex()
	page, offset, ok := a.room(&h.g, size)
	if !ok {
		if page, offset, ok = h.roomInNewPage(a, size); !ok {
			return false
		}
	}
	from := r.page()
	moved := r.moveTo(page, offset, size, a.generation)
	leaf.ReplaceKey(i, storeKey{moved.at})
	// Its bytes die where they were; its overflow pages are the copy's.
	h.killIn(from, r.at, size)
	return true
}

// peekValue appends key's value to buf, its record among the candidates p
// read racing, and reports whether the store holds key: settled where the
// leaf held still from the peek to the reads' end, as a sequence lock's
// reader reads. A candidate's record header, read racing, is trusted once
// the leaf held still after it: its lengths are then the record's, whose
// bytes lie in its page, which no collector frees while read. A writer that
// moves or rewrites a record holds its leaf exclusively (beforeRemoval,
// compact, pull, overwrite), and moves the leaf's version; a record a stale
// slot names is still Go memory, which the reader's pointer keeps. The key
// and the value are read a word at a time, and every word feeds the address
// of the last check's load, which so loads after them, with no barrier. Not
// settled where the leaf changed, or the value has overflow pages, which
// only a held leaf reads.
func (h *recordHeap) peekValue(p *gobbtree.LeafPeek[storeKey, struct{}], key, buf []byte) ([]byte, bool, bool) {
	n := len(buf)
	var acc uint64
	for j := range p.Len() {
		k, ok := p.Key(j)
		if !ok {
			continue
		}
		// A slot read racing may be one a removal cleared: no candidate's
		// record is nil at the version the peek read.
		r := k.record()
		if r.at == nil {
			return buf[:n], false, false
		}
		w := r.headWord()
		if !p.UnchangedAfter(w) {
			return buf[:n], false, false
		}
		keyLen, valueLen := int(uint16(w)), int(w>>32)
		if keyLen != len(key) {
			continue
		}
		equal, read := equalWords(unsafe.Add(r.at, recordHeader), key)
		acc ^= read
		if !equal {
			continue
		}
		if recordHeader+keyLen+valueLen > h.g.room {
			return buf[:n], false, false
		}
		buf = slices.Grow(buf, valueLen)[:n+valueLen]
		acc ^= loadWords(buf[n:], unsafe.Add(r.at, recordHeader+keyLen))
		if p.UnchangedAfter(acc) {
			return buf, true, true
		}
		return buf[:n], false, false
	}
	if !p.Edge() && p.UnchangedAfter(acc) {
		return buf[:n], false, true
	}
	return buf[:n], false, false
}

// loadWords copies the len(dst) bytes at at to dst, and folds every word it
// read from at by XOR, for a check that loads after them. The last word
// ends where the bytes do, overlapping the one before, or, where they are
// fewer than 8, starting before at, in the record that holds them, after its
// header.
func loadWords(dst []byte, at unsafe.Pointer) (acc uint64) {
	n := len(dst)
	if n < 8 {
		if n > 0 {
			w := wordAt(at, n-8) >> (8 * uint(8-n))
			acc = w
			for j := range dst {
				dst[j] = byte(w >> (8 * uint(j)))
			}
		}
		return acc
	}
	for i := 0; i < n-8; i += 8 {
		w := wordAt(at, i)
		binary.LittleEndian.PutUint64(dst[i:], w)
		acc ^= w
	}
	w := wordAt(at, n-8)
	binary.LittleEndian.PutUint64(dst[n-8:], w)
	return acc ^ w
}

// equalWords reports whether the len(key) bytes at at are key's, read as
// loadWords reads them, and the words read, folded.
func equalWords(at unsafe.Pointer, key []byte) (equal bool, acc uint64) {
	n := len(key)
	if n < 8 {
		if n == 0 {
			return true, 0
		}
		w := wordAt(at, n-8) >> (8 * uint(8-n))
		var k uint64
		for j, b := range key {
			k |= uint64(b) << (8 * uint(j))
		}
		return w == k, w
	}
	var diff uint64
	for i := 0; i < n-8; i += 8 {
		w := wordAt(at, i)
		acc ^= w
		diff |= w ^ binary.LittleEndian.Uint64(key[i:])
	}
	w := wordAt(at, n-8)
	diff |= w ^ binary.LittleEndian.Uint64(key[n-8:])
	return diff == 0, acc ^ w
}

// wordAt is the little-endian word of the 8 bytes at off from at.
func wordAt(at unsafe.Pointer, off int) uint64 {
	return binary.LittleEndian.Uint64((*[8]byte)(unsafe.Add(at, off))[:])
}
