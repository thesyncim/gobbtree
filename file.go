package gobbtree

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"os"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// FileMap reads a bbtree snapshot directly from its pages. It is immutable;
// concurrent reads and Close are safe, and reads take no lock. Returned keys
// and values are owned copies and remain valid after Close. A FileMap must
// not be copied.
type FileMap[K cmp.Ordered, V any] struct {
	image  atomic.Pointer[fileImage]
	layout diskLayout
}

// fileImage is a snapshot's bytes, and where its tree starts. Each read holds
// the image it loaded until it returns, as an iteration does until it ends:
// a mapping's cleanup unmaps it once no read holds it, as upstream's file
// map unmaps once nothing borrows it.
type fileImage struct {
	data         []byte
	root, length int
	// For integer keys of 8 bytes, the shared prefix as the top bits of a
	// word, past sharedShift: a key that differs from shared there sorts
	// before or after every key (findWordFast).
	shared      uint64
	sharedShift uint8
	// fastGet tells an image whose Get takes findWordFast: integer keys
	// of 8 bytes in its pages, plain values, and a root.
	fastGet bool
	// For string keys, the shared prefix, copied (findTextFast), and
	// fastText, fastGet's counterpart for them.
	sharedText string
	fastText   bool
}

// OpenFileMap checks every page of a serialized snapshot and takes an owned
// copy of it, as upstream's FileMap::open does.
func OpenFileMap[K cmp.Ordered, V any](data []byte) (*FileMap[K, V], error) {
	return openFileMap[K, V](append([]byte(nil), data...), true, nil)
}

// OpenFileMapUnchecked takes an owned copy of a serialized snapshot after
// checking its header alone, as upstream's open_unchecked does. The bytes
// should be what a FileWriter wrote, or what OpenFileMap accepted. A corrupt
// snapshot is never read out of bounds, but lookups and iteration on it may
// give wrong answers or panic.
func OpenFileMapUnchecked[K cmp.Ordered, V any](data []byte) (*FileMap[K, V], error) {
	return openFileMap[K, V](append([]byte(nil), data...), false, nil)
}

func openFileMap[K cmp.Ordered, V any](data []byte, checked bool, unmap func() error) (*FileMap[K, V], error) {
	l, err := newDiskLayout[K, V]()
	if err != nil {
		return nil, err
	}
	check := l.header
	if checked {
		check = l.validate
	}
	root, count, err := check(data)
	if err != nil {
		return nil, err
	}
	n := int(binary.LittleEndian.Uint64(data[40:]))
	img := &fileImage{data: data[:n:n], root: root, length: count, sharedShift: 64}
	if l.wordPages && root != 0 {
		// The header's check put the root page, and its area, in the map:
		// the prefix's length, then its bytes, at most 8 for these keys.
		start := (root &^ 1) + l.area
		img.shared = binary.BigEndian.Uint64(data[start+1 : start+9])
		img.sharedShift = uint8(64 - 8*min(int(data[start]), 8))
		img.fastGet = l.wordFast && !l.value.variable
	}
	if l.textFast && root != 0 {
		start := (root &^ 1) + l.area
		img.sharedText = string(data[start+1 : start+1+int(data[start])])
		img.fastText = !l.value.variable
	}
	if unmap != nil {
		// The cleanup holds the mapping's release, never the image.
		runtime.AddCleanup(img, func(release func() error) { _ = release() }, unmap)
	}
	m := &FileMap[K, V]{layout: l}
	m.image.Store(img)
	return m, nil
}

// ReadFile reads a snapshot into memory and checks every page, as upstream's
// read does.
func ReadFile[K cmp.Ordered, V any](path string) (*FileMap[K, V], error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return openFileMap[K, V](data, true, nil)
}

// ReadFileRange reads a snapshot embedded in a larger file, as WriteAt
// placed it, and checks every page, as upstream's read_range does.
func ReadFileRange[K cmp.Ordered, V any](file *os.File, r FileRange) (*FileMap[K, V], error) {
	if err := checkFileRange(file, r); err != nil {
		return nil, err
	}
	data := make([]byte, int(r.Length))
	if _, err := file.ReadAt(data, r.Offset); err != nil {
		return nil, err
	}
	return openFileMap[K, V](data, true, nil)
}

func checkFileRange(file *os.File, r FileRange) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	offset, length := r.Offset, r.Length
	if offset < 0 || length < fileHeaderSize || uint64(length) > uint64(^uint(0)>>1) || offset > info.Size() || length > info.Size()-offset {
		return fmt.Errorf("%w: file range", ErrFileFormat)
	}
	return nil
}

// OsPages is the size of OS pages a snapshot is mapped in, as upstream's
// OsPages.
type OsPages uint8

const (
	// OsPagesSmall maps in the system's own pages: a lookup that reaches a
	// page not in memory reads a few KiB of the file.
	OsPagesSmall OsPages = iota
	// OsPagesHuge advises 2 MiB pages on Linux (MADV_HUGEPAGE): once in
	// memory, lookups miss the TLB less, but a lookup that reaches a page not
	// in memory reads 2 MiB of the file. Elsewhere, as OsPagesSmall.
	OsPagesHuge
)

// MmapOptions says how Mmap maps a snapshot: the whole file or the range
// WriteAt returned, in small or huge OS pages, as upstream's MmapOptions.
type MmapOptions[K cmp.Ordered, V any] struct {
	path     string
	r        FileRange
	hasRange bool
	pages    OsPages
}

// Mmap maps the whole file at path, in small OS pages, unless set otherwise;
// Open maps it, as upstream's mmap does.
func Mmap[K cmp.Ordered, V any](path string) MmapOptions[K, V] {
	return MmapOptions[K, V]{path: path}
}

// Pages sets the OS pages to map the file in.
func (o MmapOptions[K, V]) Pages(pages OsPages) MmapOptions[K, V] {
	o.pages = pages
	return o
}

// Range sets the range of the file the snapshot lies in, as WriteAt
// returned it.
func (o MmapOptions[K, V]) Range(r FileRange) MmapOptions[K, V] {
	o.r, o.hasRange = r, true
	return o
}

// Open maps the snapshot read-only and checks its header alone, as upstream's
// mmap does: opening reads the first page, lookups read the rest as they
// reach it. The file should hold what a FileWriter wrote and must not be
// modified or truncated while mapped, the contract upstream's unsafe open
// states: a read past the end of a truncated file faults, which ends the
// program. WriteFile publishes a replacement by renaming. A corrupt snapshot is never read out of bounds, but lookups and
// iteration on it may give wrong answers or panic. ReadFile checks every page.
func (o MmapOptions[K, V]) Open() (*FileMap[K, V], error) {
	f, err := os.Open(o.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := o.r
	if !o.hasRange {
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		r = FileRange{0, info.Size()}
	}
	if err := checkFileRange(f, r); err != nil {
		return nil, err
	}
	data, unmap, err := mapFileReadOnly(f, r.Offset, int(r.Length), o.pages)
	if err != nil {
		return nil, err
	}
	m, err := openFileMap[K, V](data, false, unmap)
	if err != nil {
		_ = unmap()
		return nil, err
	}
	return m, nil
}

// Close releases the snapshot: later reads find the map empty. Reads in
// progress, and iterations, finish on it; a mapping is unmapped once none
// holds it, when the collector next finds it unreachable.
func (m *FileMap[K, V]) Close() error {
	m.image.Store(nil)
	return nil
}

func (m *FileMap[K, V]) Len() int {
	if img := m.image.Load(); img != nil {
		return img.length
	}
	return 0
}

func (m *FileMap[K, V]) IsEmpty() bool { return m.Len() == 0 }

func (m *FileMap[K, V]) ByteLen() int {
	if img := m.image.Load(); img != nil {
		return len(img.data)
	}
	return 0
}

// Alignment is what the snapshot's offset in a file must be a multiple of,
// as upstream's FileMap::alignment: what WriteAt rounds up to.
func (m *FileMap[K, V]) Alignment() int { return m.layout.rootAlign }

// child is the tagged offset of internal page node's child index.
func (m *FileMap[K, V]) child(data []byte, node, index int) int {
	l := &m.layout.internal
	return node + int(binary.NativeEndian.Uint64(data[node+l.trailing+index*l.stride:]))
}

// separatorSlot is the slot of internal page node's separator index: in the
// page, or, for keys with inline windows, the head key of the leftmost leaf
// right of it (D49).
func (m *FileMap[K, V]) separatorSlot(data []byte, node, index int) int {
	l := &m.layout.internal
	if l.width == 0 {
		return node + l.keys + index*l.keyStride
	}
	tagged := m.child(data, node, index+1)
	for depth := 0; tagged&1 == 0; depth++ {
		if depth == fileMaxDepth {
			panic("gobbtree: corrupt file map: tree too deep")
		}
		tagged = m.child(data, tagged, 0)
	}
	return (tagged &^ 1) + m.layout.leaf.headKey
}

// route is the edge of internal page node that key goes down.
func (m *FileMap[K, V]) route(data []byte, node int, key K) int {
	l := &m.layout.internal
	lo, hi := 0, int(binary.NativeEndian.Uint16(data[node:]))
	if l.width > 0 {
		prefix := int(binary.NativeEndian.Uint16(data[node+4:]))
		start := node + l.inline
		lo, hi = diskSlotRange(data[start:start+l.capacity*l.width], l.width, hi, m.window(key, prefix, l.width))
	}
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if m.compareAt(data, m.separatorSlot(data, node, mid), key) > 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// headSlot is the slot of the key of leaf's head entry, the first, which a
// leaf right of another keeps (D49): in the leaf, or, for keys without
// inline windows, the separator left of the path to it, internal page
// left's separator leftIndex. -1 for a leaf without one.
func (m *FileMap[K, V]) headSlot(data []byte, leaf, left, leftIndex int) int {
	if data[leaf+2] == 0 {
		return -1
	}
	if m.layout.leaf.headKey >= 0 {
		return leaf + m.layout.leaf.headKey
	}
	if left < 0 {
		panic("gobbtree: corrupt file map: leftmost leaf has a head")
	}
	return m.separatorSlot(data, left, leftIndex)
}

// leafKeySlot is the slot of leaf's key index; head is its head key's slot.
func (m *FileMap[K, V]) leafKeySlot(data []byte, leaf, head, index int) int {
	if index -= int(data[leaf+2]); index >= 0 {
		return leaf + m.layout.leaf.keys + index*m.layout.leaf.keyStride
	}
	return head
}

// leafCount is how many entries leaf holds, its head's included.
func leafCount(data []byte, leaf int) int {
	return int(binary.NativeEndian.Uint16(data[leaf:])) + int(data[leaf+2])
}

// seekLeaf is the index of leaf's first entry whose key is at or above key;
// head is its head key's slot.
func (m *FileMap[K, V]) seekLeaf(data []byte, leaf, head int, key K) int {
	l := &m.layout.leaf
	n, h := leafCount(data, leaf), int(data[leaf+2])
	lo, hi := 0, n
	switch {
	case h != 0 && m.compareAt(data, head, key) >= 0:
		hi = 0
	case l.width > 0:
		start := leaf + l.inline
		prefix := int(binary.NativeEndian.Uint16(data[leaf+4:]))
		lo, hi = diskSlotRange(data[start:start+l.capacity*l.width], l.width, n-h, m.window(key, prefix, l.width))
		lo += h
		hi += h
	}
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if m.compareAt(data, m.leafKeySlot(data, leaf, head, mid), key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// valueSlot is the slot of leaf's value index.
func (m *FileMap[K, V]) valueSlot(data []byte, leaf, index int) int {
	l := &m.layout.leaf
	return leaf + l.trailing + (index+1-int(data[leaf+2]))*l.stride
}

type diskFrame struct{ node, index int }
type fileCursor[K cmp.Ordered, V any] struct {
	mapFile *FileMap[K, V]
	// image is what the iteration reads, from its first entry to its last.
	image              *fileImage
	path               [fileMaxDepth]diskFrame
	depth, leaf, index int
	// last is the leaf visited before this one: leaves come in the order of
	// their offsets, as a FileWriter lays them out, which bounds the leaves
	// a corrupt snapshot's edges can lead an iteration through.
	last                   int
	started, done, reverse bool
	bounds                 *Bounds[K]
}

func (c *fileCursor[K, V]) descend(tagged int, seek bool) {
	m, data := c.mapFile, c.image.data
	key := c.bounds.lower
	if c.reverse {
		key = c.bounds.upper
	}
	outside := 0
	if seek {
		outside = m.compareShared(data, c.image.root, key)
	}
	for tagged&1 == 0 {
		if c.depth == fileMaxDepth {
			panic("gobbtree: corrupt file map: tree too deep")
		}
		index := 0
		if seek {
			if outside > 0 {
				index = int(binary.NativeEndian.Uint16(data[tagged:]))
			} else if outside == 0 {
				index = m.route(data, tagged, key)
			}
		} else if c.reverse {
			index = int(binary.NativeEndian.Uint16(data[tagged:]))
		}
		c.path[c.depth] = diskFrame{tagged, index}
		c.depth++
		tagged = m.child(data, tagged, index)
	}
	c.leaf = tagged &^ 1
	if c.last >= 0 && (c.reverse && c.leaf >= c.last || !c.reverse && c.leaf <= c.last) {
		panic("gobbtree: corrupt file map: leaves out of order")
	}
	c.last = c.leaf
	n := leafCount(data, c.leaf)
	c.index = 0
	if seek {
		lo := 0
		switch {
		case outside < 0:
		case outside > 0:
			lo = n
		default:
			lo = m.seekLeaf(data, c.leaf, c.head(), key)
		}
		// lo is the first key at or above key; step past an equal key the
		// bounds exclude ascending, or include descending.
		if lo < n && (c.reverse && c.bounds.upperKind == included || !c.reverse && c.bounds.lowerKind == excluded) &&
			m.compareAt(data, c.keySlot(lo), key) == 0 {
			lo++
		}
		c.index = lo
		if c.reverse {
			c.index--
		}
	} else if c.reverse {
		c.index = n - 1
	}
}

// head is the slot of the leaf's head key, or -1 (headSlot).
func (c *fileCursor[K, V]) head() int {
	left, leftIndex := -1, 0
	for i := c.depth - 1; i >= 0; i-- {
		if f := c.path[i]; f.index > 0 {
			left, leftIndex = f.node, f.index-1
			break
		}
	}
	return c.mapFile.headSlot(c.image.data, c.leaf, left, leftIndex)
}

func (c *fileCursor[K, V]) keySlot(index int) int {
	head := -1
	if index < int(c.image.data[c.leaf+2]) {
		head = c.head()
	}
	return c.mapFile.leafKeySlot(c.image.data, c.leaf, head, index)
}

func (c *fileCursor[K, V]) advanceLeaf() bool {
	m, data := c.mapFile, c.image.data
	for c.depth > 0 {
		f := &c.path[c.depth-1]
		n := int(binary.NativeEndian.Uint16(data[f.node:]))
		if (!c.reverse && f.index < n) || (c.reverse && f.index > 0) {
			if c.reverse {
				f.index--
			} else {
				f.index++
			}
			c.descend(m.child(data, f.node, f.index), false)
			return true
		}
		c.depth--
	}
	return false
}

func (c *fileCursor[K, V]) next() (key K, value V, ok bool) {
	m := c.mapFile
	if c.done {
		return
	}
	first := !c.started
	if first {
		c.started = true
		c.image, c.last = m.image.Load(), -1
	} else if m.image.Load() != c.image {
		// Closed since the iteration began.
		c.image = nil
	}
	if c.image == nil || c.image.root == 0 {
		c.done = true
		return
	}
	if first {
		c.descend(c.image.root, c.reverse && c.bounds.upperKind != unbounded || !c.reverse && c.bounds.lowerKind != unbounded)
	}
	for c.index < 0 || c.index >= leafCount(c.image.data, c.leaf) {
		if !c.advanceLeaf() {
			c.done = true
			return
		}
	}
	key = diskRead[K](c.image.data, c.keySlot(c.index), m.layout.key)
	if !aboveLower[K, natural[K]](c.bounds, key) || !belowUpper[K, natural[K]](c.bounds, key) {
		c.done = true
		return key, value, false
	}
	value = diskRead[V](c.image.data, m.valueSlot(c.image.data, c.leaf, c.index), m.layout.value)
	if c.reverse {
		c.index--
	} else {
		c.index++
	}
	return key, value, true
}

// find is where key's entry is in img: the slots of its key and value, ok
// if it holds key. It reads the pages a search reaches, from the root down.
func (m *FileMap[K, V]) find(img *fileImage, key K) (keySlot, valueSlot int, ok bool) {
	if unsafe.Sizeof(key) == 8 && m.layout.wordPages && img.root != 0 {
		return m.findWord(img, m.keyWord(key))
	}
	if unsafe.Sizeof(key) == unsafe.Sizeof("") && m.layout.textFast && img.root != 0 {
		keySlot, valueSlot, state := m.findTextFast(img, *(*string)(unsafe.Pointer(&key)))
		if state != wordUnsettled {
			return keySlot, valueSlot, state == wordFound
		}
	}
	return m.findKey(img, key)
}

// findKey is find for every key, by the general search.
func (m *FileMap[K, V]) findKey(img *fileImage, key K) (keySlot, valueSlot int, ok bool) {
	data := img.data
	if img.root == 0 || m.compareShared(data, img.root, key) != 0 {
		return
	}
	tagged, left, leftIndex := img.root, -1, 0
	for depth := 0; tagged&1 == 0; depth++ {
		if depth == fileMaxDepth {
			panic("gobbtree: corrupt file map: tree too deep")
		}
		index := m.route(data, tagged, key)
		if index > 0 {
			left, leftIndex = tagged, index-1
		}
		tagged = m.child(data, tagged, index)
	}
	leaf := tagged &^ 1
	head := m.headSlot(data, leaf, left, leftIndex)
	i := m.seekLeaf(data, leaf, head, key)
	if i >= leafCount(data, leaf) {
		return
	}
	keySlot = m.leafKeySlot(data, leaf, head, i)
	if m.compareAt(data, keySlot, key) != 0 {
		return keySlot, 0, false
	}
	return keySlot, m.valueSlot(data, leaf, i), true
}

// loadImage is the map's image, for a lookup. Where stores stay ordered
// (seqlockPlainStores) it is a plain load: the image is immutable once
// published, and every read through it depends on its address, which
// orders those reads after it on these targets, where an acquire load
// would also hold the next lookup's loads behind it.
func (m *FileMap[K, V]) loadImage() *fileImage {
	if seqlockPlainStores {
		return plainPointer(&m.image)
	}
	return m.image.Load()
}

func (m *FileMap[K, V]) Get(key K) (value V, ok bool) {
	img := m.loadImage()
	if img == nil {
		return
	}
	// A mapping goes once nothing holds its image: Get holds it to its end.
	value, ok = m.getHeld(img, key)
	runtime.KeepAlive(img)
	return
}

// getHeld is Get on img, by findWordFast's or findTextFast's descent where
// the image's layout fits one, and get's otherwise.
func (m *FileMap[K, V]) getHeld(img *fileImage, key K) (value V, ok bool) {
	if unsafe.Sizeof(key) == 8 && img.fastGet {
		_, slot, state := m.findWordFast(img, *(*uint64)(unsafe.Pointer(&key))^m.layout.flip)
		switch state {
		case wordFound:
			// In a page findWordFast checked lies in the map.
			return *(*V)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(img.data)), slot)), true
		case wordAbsent:
			return
		}
	}
	if unsafe.Sizeof(key) == unsafe.Sizeof("") && img.fastText {
		_, slot, state := m.findTextFast(img, *(*string)(unsafe.Pointer(&key)))
		switch state {
		case wordFound:
			// In a page findTextFast checked lies in the map.
			return *(*V)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(img.data)), slot)), true
		case wordAbsent:
			return
		}
	}
	return m.get(img, key)
}

// get is Get on img. A heap image needs nothing kept alive: its bytes keep
// themselves.
func (m *FileMap[K, V]) get(img *fileImage, key K) (value V, ok bool) {
	_, slot, ok := m.find(img, key)
	if !ok {
		return
	}
	if m.layout.value.variable {
		return m.readVariable(img.data, slot), true
	}
	return diskReadPlain[V](img.data, slot, m.layout.value.size), true
}

// readVariable is the string or byte slice value in slot, out of get's
// line: inlined, its copy would cost every get a larger frame.
//
//go:noinline
func (m *FileMap[K, V]) readVariable(data []byte, slot int) V {
	return diskReadVariable[V](data, slot, m.layout.value)
}

func (m *FileMap[K, V]) GetKeyValue(key K) (stored K, value V, ok bool) {
	img := m.loadImage()
	if img == nil {
		return
	}
	ks, vs, ok := m.find(img, key)
	if ok {
		stored = diskRead[K](img.data, ks, m.layout.key)
		value = diskRead[V](img.data, vs, m.layout.value)
	}
	runtime.KeepAlive(img)
	return
}

func (m *FileMap[K, V]) Contains(key K) bool {
	img := m.loadImage()
	if img == nil {
		return false
	}
	_, _, ok := m.find(img, key)
	runtime.KeepAlive(img)
	return ok
}

func (m *FileMap[K, V]) First() (K, V, bool) {
	c := fileCursor[K, V]{mapFile: m, bounds: &Bounds[K]{}}
	return c.next()
}

func (m *FileMap[K, V]) Last() (K, V, bool) {
	c := fileCursor[K, V]{mapFile: m, reverse: true, bounds: &Bounds[K]{}}
	return c.next()
}

func (m *FileMap[K, V]) Keys() iter.Seq[K] {
	return func(yield func(K) bool) {
		for k := range m.All() {
			if !yield(k) {
				return
			}
		}
	}
}

func (m *FileMap[K, V]) Values() iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, v := range m.All() {
			if !yield(v) {
				return
			}
		}
	}
}

func (m *FileMap[K, V]) GetMany(keys []K, dst []Result[V]) {
	if len(dst) < len(keys) {
		panic("gobbtree: GetMany destination too short")
	}
	img := m.loadImage()
	if img == nil {
		clear(dst[:len(keys)])
		return
	}
	for i, key := range keys {
		_, slot, ok := m.find(img, key)
		var value V
		if ok {
			value = diskRead[V](img.data, slot, m.layout.value)
		}
		dst[i] = Result[V]{value, ok}
	}
	runtime.KeepAlive(img)
}

// scan visits the keys in b.
func (m *FileMap[K, V]) scan(b *Bounds[K], reverse bool, visit func(K, V) bool) {
	c := fileCursor[K, V]{mapFile: m, reverse: reverse, bounds: b}
	for {
		k, v, ok := c.next()
		if !ok || !visit(k, v) {
			return
		}
	}
}

// Range iterates the entries with keys in [lower, upper), ascending, as
// Scan(Between(lower, upper)) does.
func (m *FileMap[K, V]) Range(lower, upper K) iter.Seq2[K, V] { return m.Scan(Between(lower, upper)) }

// RangeReverse is Range in descending order.
func (m *FileMap[K, V]) RangeReverse(lower, upper K) iter.Seq2[K, V] {
	return m.ScanReverse(Between(lower, upper))
}

// Ascend iterates the entries with keys at or above from, ascending.
func (m *FileMap[K, V]) Ascend(from K) iter.Seq2[K, V] { return m.Scan(AtLeast(from)) }

// Descend iterates the entries with keys at or below from, descending.
func (m *FileMap[K, V]) Descend(from K) iter.Seq2[K, V] { return m.ScanReverse(AtMost(from)) }

// Scan iterates the entries with keys in b, ascending, as upstream's range
// does. Iterating a non-empty file map panics on bounds upstream rejects
// (see Bounds), as upstream's range does.
func (m *FileMap[K, V]) Scan(b Bounds[K]) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if m.Len() != 0 {
			checkBounds[K, natural[K]](&b)
		}
		m.scan(&b, false, yield)
	}
}

// ScanReverse is Scan in descending order.
func (m *FileMap[K, V]) ScanReverse(b Bounds[K]) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if m.Len() != 0 {
			checkBounds[K, natural[K]](&b)
		}
		m.scan(&b, true, yield)
	}
}

func (m *FileMap[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) { m.scan(&Bounds[K]{}, false, yield) }
}
func (m *FileMap[K, V]) Reverse() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) { m.scan(&Bounds[K]{}, true, yield) }
}

// WriteTo copies the validated image, preserving its Rust-compatible layout.
func (m *FileMap[K, V]) WriteTo(w io.Writer) (int64, error) {
	img := m.image.Load()
	if img == nil {
		return 0, os.ErrClosed
	}
	n, err := w.Write(img.data)
	if err == nil && n != len(img.data) {
		err = io.ErrShortWrite
	}
	runtime.KeepAlive(img)
	return int64(n), err
}
