package store

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"slices"
	"unsafe"

	"github.com/thesyncim/gobbtree"
)

// A record is a key and its value, in a heap page (heap_page.go),
// upstream's Record (record.rs):
//
//	| key_len: u16 | offset: u16 | value_len: u32 | generation: u8 | key … | value … |
//
// little-endian and unaligned, offset its place in its page and generation
// that of the leaf whose heap it was written into (leaf_heap.go). A value
// too long for its record to fit in a heap page's room goes into overflow
// pages (overflow.go), the record keeping their id in the store's overflow
// table after the key, then the value's start:
//
//	| key_len: u16 | offset: u16 | value_len: u32 | generation: u8 | key … | link: u32 | start … |
//
// The pages are all full, the start what is left over, as SQLite does, if
// it fits; else the record keeps none of the value, and the last page holds
// what is left. Which of them a record is, and how much it keeps, its
// lengths tell (keptLen). The map orders its keys, and windows them, by
// their key bytes, as it does a byte string's.
//
// Upstream's record holds a 4-byte key length and finds its page by
// rounding its address down; Go doesn't align a page to its size, so the
// record holds its offset instead, in 2 of those bytes, as a key is at most
// a quarter of a page of at most 64 KiB: the header is upstream's 9 bytes.
//
// The map compares a key, and so reads its record, only while it holds the
// key's leaf (gobbtree.AnnexedMapOf). So a record, and its overflow pages,
// stay put while any goroutine reads them, as the store moves, changes or
// kills them only while it holds their leaf exclusively (store.go).
type record struct{ at unsafe.Pointer }

const (
	// The header's fields, and where the key starts.
	keyLenAt     = 0
	offsetAt     = 2
	valueLenAt   = 4
	generationAt = 8
	recordHeader = 9
	// linkSize is the size of a record's link to its overflow pages.
	linkSize = 4
	// queryMarker is the key length no record has, which a queryKey starts
	// with; its high byte, the record's second, tells them apart.
	queryMarker = 0xFFFF
)

// bytesAt is the n bytes offset bytes from p, which lie within one page;
// nil for none, with no address taken, as an empty slice at a page's end
// would point past it: Go doesn't let a pointer leave its object.
func bytesAt(p unsafe.Pointer, offset, n int) []byte {
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Add(p, offset)), n)
}

func (r record) header() []byte { return unsafe.Slice((*byte)(r.at), recordHeader) }

func (r record) keyLen() int { return int(binary.LittleEndian.Uint16(r.header()[keyLenAt:])) }

// offset is the record's place in its page, from the page's start.
func (r record) offset() int { return int(binary.LittleEndian.Uint16(r.header()[offsetAt:])) }

// valueLen is the value's length, overflow pages and all.
func (r record) valueLen() int { return int(binary.LittleEndian.Uint32(r.header()[valueLenAt:])) }

// generation is that of the leaf whose heap the record was written into.
func (r record) generation() uint8 { return r.header()[generationAt] }

// page is the heap page the record lies in.
func (r record) page() *heapPage { return (*heapPage)(unsafe.Add(r.at, -r.offset())) }

// key is the key's bytes, for as long as the caller reads the record.
func (r record) key() []byte {
	return bytesAt(r.at, recordHeader, r.keyLen())
}

// keptLen is how many of the bytes of a value of valueLen its record keeps,
// with a key of keyLen: all of them if the record fits in a heap page's
// room; else, past the link to its overflow pages, what is left over once
// they are full, if it fits; else none.
func (g *geometry) keptLen(keyLen, valueLen int) int {
	if recordHeader+keyLen+valueLen <= g.room {
		return valueLen
	}
	leftOver := valueLen % g.overflowRoom
	if recordHeader+keyLen+linkSize+leftOver <= g.room {
		return leftOver
	}
	return 0
}

// sizeOf is the size of the record of a key and a value of these lengths:
// at most a heap page's room, for a key no longer than a quarter page.
func (g *geometry) sizeOf(keyLen, valueLen int) int {
	kept := g.keptLen(keyLen, valueLen)
	link := 0
	if kept < valueLen {
		link = linkSize
	}
	return recordHeader + keyLen + link + kept
}

// size is the record's size, in its page.
func (r record) size(g *geometry) int { return g.sizeOf(r.keyLen(), r.valueLen()) }

// writeRecord writes the record of key and value, in a leaf of
// generation, at offset in page, which has room for it, link naming the
// overflow pages that hold the bytes of the value it doesn't keep, if any.
func (g *geometry) writeRecord(page *heapPage, offset int, key, value []byte, link overflowLink, generation uint8) record {
	kept := g.keptLen(len(key), len(value))
	if link.ok != (kept < len(value)) {
		panic("store: overflow pages hold what a record doesn't keep of its value")
	}
	size := recordHeader + len(key) + kept
	if link.ok {
		size += linkSize
	}
	r := record{page.at(offset)}
	b := unsafe.Slice((*byte)(r.at), size)
	binary.LittleEndian.PutUint16(b[keyLenAt:], uint16(len(key)))
	binary.LittleEndian.PutUint16(b[offsetAt:], uint16(offset))
	binary.LittleEndian.PutUint32(b[valueLenAt:], uint32(len(value)))
	b[generationAt] = generation
	at := recordHeader + copy(b[recordHeader:], key)
	if link.ok {
		binary.LittleEndian.PutUint32(b[at:], link.id)
		at += linkSize
	}
	copy(b[at:], value[:kept])
	return r
}

// link is the record's link to its overflow pages; ok is false if it keeps
// its value whole.
func (r record) link(g *geometry) overflowLink {
	keyLen, valueLen := r.keyLen(), r.valueLen()
	if g.keptLen(keyLen, valueLen) == valueLen {
		return overflowLink{}
	}
	return overflowLink{id: r.linkID(keyLen), ok: true}
}

// linkID is the id of the record's overflow pages, past its key of keyLen
// bytes; the record has one.
func (r record) linkID(keyLen int) uint32 {
	return binary.LittleEndian.Uint32(unsafe.Slice((*byte)(unsafe.Add(r.at, recordHeader+keyLen)), linkSize))
}

// slideTo copies the record, of size bytes, to offset in page, as a
// compaction slides it along its leaf's chain, and returns the copy, which
// keeps the record's generation and links its overflow pages, its then. The
// bytes there may overlap the record's, as a slide down its own page does.
func (r record) slideTo(page *heapPage, offset, size int) record {
	to := record{page.at(offset)}
	copy(unsafe.Slice((*byte)(to.at), size), unsafe.Slice((*byte)(r.at), size))
	binary.LittleEndian.PutUint16(to.header()[offsetAt:], uint16(offset))
	return to
}

// moveTo copies the record, of size bytes, to offset in page, with
// generation, as a pull moves it into its own leaf's chain, and returns the
// copy, which links the record's overflow pages, its then.
func (r record) moveTo(page *heapPage, offset, size int, generation uint8) record {
	to := r.slideTo(page, offset, size)
	to.header()[generationAt] = generation
	return to
}

// overwriteValue gives the record, whose key is keyLen bytes and whose
// value, of valueLen, it keeps whole, value in place of its own, which is
// no shorter: the bytes past the new value are a hole. The caller holds
// the record's leaf exclusively: no goroutine reads it.
func (r record) overwriteValue(keyLen, valueLen int, value []byte) {
	if len(value) > valueLen {
		panic("store: a value overwrites one no shorter")
	}
	binary.LittleEndian.PutUint32(r.header()[valueLenAt:], uint32(len(value)))
	copy(bytesAt(r.at, recordHeader+keyLen, len(value)), value)
}

// recordValue is a record's value: the bytes its record keeps, then its
// overflow pages'.
type recordValue struct {
	// start is the value's start, in the record: all of it, without
	// overflow pages.
	start []byte
	// first is the first overflow page, nil without, and rest how many of
	// the value's bytes the pages hold.
	first *overflowPage
	rest  int
}

// value is the record's value, for as long as the caller reads the record.
func (h *recordHeap) value(r record) recordValue {
	return h.valueOf(r, r.keyLen(), r.valueLen())
}

// headWord is the record header's first 8 bytes, in one load: the key's
// length, its offset, and the value's length (keyLenAt, offsetAt,
// valueLenAt).
func (r record) headWord() uint64 { return binary.LittleEndian.Uint64((*[8]byte)(r.at)[:]) }

// entry is the record's key and value, for a scan's loop, which it inlines
// into: its key's and value's lengths read in one load. ok is false for a
// value too long for its record, which overflowEntry gathers.
func (h *recordHeap) entry(r record) (key, value []byte, ok bool) {
	w := r.headWord()
	keyLen, valueLen := int(uint16(w)), int(w>>32)
	if recordHeader+keyLen+valueLen > h.g.room {
		return nil, nil, false
	}
	return bytesAt(r.at, recordHeader, keyLen), bytesAt(r.at, recordHeader+keyLen, valueLen), true
}

// overflowEntry is the record's key and value, the value gathered from its
// overflow pages into *gathered.
func (h *recordHeap) overflowEntry(r record, gathered *[]byte) (key, value []byte) {
	return r.key(), h.value(r).gather(&h.g, gathered)
}

// valueOf is value, given the record's key and value lengths.
func (h *recordHeap) valueOf(r record, keyLen, valueLen int) recordValue {
	kept := h.g.keptLen(keyLen, valueLen)
	if kept == valueLen {
		return recordValue{start: bytesAt(r.at, recordHeader+keyLen, valueLen)}
	}
	return recordValue{
		start: bytesAt(r.at, recordHeader+keyLen+linkSize, kept),
		first: h.table.page(r.linkID(keyLen)),
		rest:  valueLen - kept,
	}
}

func (v recordValue) len() int { return len(v.start) + v.rest }

// appendTo appends the value to buf.
func (v recordValue) appendTo(g *geometry, buf []byte) []byte {
	if v.first == nil {
		return append(buf, v.start...)
	}
	buf = append(slices.Grow(buf, v.len()), v.start...)
	return g.appendOverflow(v.first, v.rest, buf)
}

// gather is the value's bytes: the record's, or, with overflow pages,
// those of *gathered, emptied and filled with them.
func (v recordValue) gather(g *geometry, gathered *[]byte) []byte {
	if v.first == nil {
		return v.start
	}
	*gathered = v.appendTo(g, (*gathered)[:0])
	return *gathered
}

// storeKey is a store's key: a pointer to its record, which the store
// swaps for another of the same key, through the leaf (ReplaceKey), while
// it holds the key's leaf exclusively, as the value changes or the record
// moves, upstream's StoreKey. It points into its record's page, which it
// keeps alive. A lookup's key points to a queryKey instead.
//
// It implements no Clone: the map keeps the key it is given, which vacant
// points to the record it wrote.
type storeKey struct{ p unsafe.Pointer }

// queryKey is a lookup's key, or a new key's before its record is written,
// where the caller has it, upstream's QueryKey: it starts with a key length
// no record has, which tells it from a record.
type queryKey struct {
	// marker is queryMarker.
	marker uint16
	key    []byte
}

// newQuery is the store key of a query for key, valid while q is. Escape
// analysis moves q to the heap; a scan keeps its bounds' queries there, as
// a scan may outlive its call, and a point lookup's lie in its frame
// (pointQuery).
func newQuery(q *queryKey, key []byte) storeKey {
	*q = queryKey{marker: queryMarker, key: key}
	return storeKey{unsafe.Pointer(q)}
}

// poisonedKey is the key of a released query, under storepoison.
var poisonedKey [1]byte

// bytes is the key's bytes.
func (k storeKey) bytes() []byte {
	if *(*byte)(unsafe.Add(k.p, 1)) == queryMarker>>8 {
		key := (*queryKey)(k.p).key
		if poisonedQueries && unsafe.SliceData(key) == &poisonedKey[0] {
			panic("store: the map kept a released query's key")
		}
		return key
	}
	return record{k.p}.key()
}

func (k storeKey) record() record { return record{k.p} }

// Compare orders keys by their bytes, as bytes.Compare does.
func (k storeKey) Compare(other storeKey) int { return bytes.Compare(k.bytes(), other.bytes()) }

// Window is the key bytes' window at offset, as a byte string's.
func (k storeKey) Window(offset int) uint64 { return gobbtree.BytesWindow(k.bytes(), offset) }

// The rest of upstream's StoreKey InlineKey impl, a byte string's: its maps'
// leaves keep each key's hash tag, and its internal nodes 4 bytes of each
// window, whose ties compare from where keys can differ.

// HashTag is the key's length, saturated at 255, a byte string's tag.
func (k storeKey) HashTag() uint8 { return uint8(min(len(k.bytes()), 255)) }

// CompareFrom orders the keys by their bytes past the first equal, which
// the map knows are equal, zero-padded: where those agree, the shorter key
// first.
func (k storeKey) CompareFrom(other storeKey, equal int) int {
	a, b := k.bytes(), other.bytes()
	if c := bytes.Compare(a[min(equal, len(a)):], b[min(equal, len(b)):]); c != 0 {
		return c
	}
	return cmp.Compare(len(a), len(b))
}

// ComparePrefix orders the key's first len(prefix) bytes, zero-padded,
// against prefix.
func (k storeKey) ComparePrefix(prefix []byte) int {
	b := k.bytes()
	n := min(len(b), len(prefix))
	if c := bytes.Compare(b[:n], prefix[:n]); c != 0 {
		return c
	}
	for _, c := range prefix[n:] {
		if c != 0 {
			return -1
		}
	}
	return 0
}
