package gobbtree

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"os"
	"runtime"
	"sync"
)

// FileMap reads a bbtree snapshot directly from its pages. It is immutable;
// concurrent reads and Close are safe. Returned keys and values are owned
// copies and remain valid after Close. A FileMap must not be copied.
type FileMap[K cmp.Ordered, V any] struct {
	mu           sync.RWMutex
	data         []byte
	layout       diskLayout
	root, length int
	unmap        func() error
	cleanup      runtime.Cleanup
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
	m := &FileMap[K, V]{data: data[:n:n], layout: l, root: root, length: count, unmap: unmap}
	if unmap != nil {
		// The cleanup argument holds the mapping, never m. Explicit Close
		// releases it promptly; unreachable maps also release OS resources.
		m.cleanup = runtime.AddCleanup(m, func(release func() error) { _ = release() }, unmap)
	}
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
// modified or truncated while mapped; WriteFile publishes a replacement by
// renaming. A corrupt snapshot is never read out of bounds, but lookups and
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

// Close releases the snapshot. Subsequent reads report an empty map.
func (m *FileMap[K, V]) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanup.Stop()
	m.data = nil
	m.root = 0
	m.length = 0
	if m.unmap != nil {
		f := m.unmap
		m.unmap = nil
		return f()
	}
	return nil
}

func (m *FileMap[K, V]) Len() int      { m.mu.RLock(); defer m.mu.RUnlock(); return m.length }
func (m *FileMap[K, V]) IsEmpty() bool { return m.Len() == 0 }
func (m *FileMap[K, V]) ByteLen() int  { m.mu.RLock(); defer m.mu.RUnlock(); return len(m.data) }

// Alignment is what the snapshot's offset in a file must be a multiple of,
// as upstream's FileMap::alignment: what WriteAt rounds up to.
func (m *FileMap[K, V]) Alignment() int { return m.layout.rootAlign }

func (m *FileMap[K, V]) child(node, index int) int {
	l := m.layout.internal
	return node + int(binary.NativeEndian.Uint64(m.data[node+l.trailing+index*l.stride:]))
}

func (m *FileMap[K, V]) separatorSlot(node, index int) int {
	l := m.layout.internal
	if l.width == 0 {
		return node + l.keys + index*l.keyStride
	}
	tagged := m.child(node, index+1)
	for depth := 0; tagged&1 == 0; depth++ {
		if depth == fileMaxDepth {
			panic("gobbtree: corrupt file map: tree too deep")
		}
		tagged = m.child(tagged, 0)
	}
	return (tagged &^ 1) + m.layout.leaf.headKey
}

func (m *FileMap[K, V]) route(node int, key K) int {
	l := m.layout.internal
	lo, hi := 0, int(binary.NativeEndian.Uint16(m.data[node:]))
	if l.width > 0 {
		prefix := int(binary.NativeEndian.Uint16(m.data[node+4:]))
		start := node + l.inline
		lo, hi = diskInlineRange(m.data[start:start+hi*l.width], l.width, diskWindow(key, prefix, l.width))
	}
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if diskCompare(m.data, m.separatorSlot(node, mid), m.layout.key, key) > 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

type diskFrame struct{ node, index int }
type fileCursor[K cmp.Ordered, V any] struct {
	mapFile                *FileMap[K, V]
	path                   [fileMaxDepth]diskFrame
	depth, leaf, index     int
	started, done, reverse bool
	bounds                 *Bounds[K]
}

func (c *fileCursor[K, V]) descend(tagged int, seek bool) {
	m := c.mapFile
	key := c.bounds.lower
	if c.reverse {
		key = c.bounds.upper
	}
	outside := 0
	if seek {
		outside = m.compareShared(key)
	}
	for tagged&1 == 0 {
		index := 0
		if seek {
			if outside > 0 {
				index = int(binary.NativeEndian.Uint16(m.data[tagged:]))
			} else if outside == 0 {
				index = m.route(tagged, key)
			}
		} else if c.reverse {
			index = int(binary.NativeEndian.Uint16(m.data[tagged:]))
		}
		c.path[c.depth] = diskFrame{tagged, index}
		c.depth++
		tagged = m.child(tagged, index)
	}
	c.leaf = tagged &^ 1
	n := c.count()
	c.index = 0
	if seek {
		lo, hi := 0, n
		l := m.layout.leaf
		switch {
		case outside < 0:
			hi = 0
		case outside > 0:
			lo = n
		case int(m.data[c.leaf+2]) != 0 && diskCompare(m.data, c.keySlot(0), m.layout.key, key) >= 0:
			hi = 0
		case l.width > 0:
			head := int(m.data[c.leaf+2])
			start := c.leaf + l.inline
			prefix := int(binary.NativeEndian.Uint16(m.data[c.leaf+4:]))
			lo, hi = diskInlineRange(m.data[start:start+(n-head)*l.width], l.width, diskWindow(key, prefix, l.width))
			lo += head
			hi += head
		}
		for lo < hi {
			mid := int(uint(lo+hi) >> 1)
			if diskCompare(m.data, c.keySlot(mid), m.layout.key, key) < 0 {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		// lo is the first key at or above key; step past an equal key the
		// bounds exclude ascending, or include descending.
		if lo < n && (c.reverse && c.bounds.upperKind == included || !c.reverse && c.bounds.lowerKind == excluded) &&
			diskCompare(m.data, c.keySlot(lo), m.layout.key, key) == 0 {
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

func (c *fileCursor[K, V]) count() int {
	m := c.mapFile
	return int(binary.NativeEndian.Uint16(m.data[c.leaf:])) + int(m.data[c.leaf+2])
}
func (c *fileCursor[K, V]) keySlot(index int) int {
	m := c.mapFile
	l := m.layout.leaf
	index -= int(m.data[c.leaf+2])
	if index >= 0 {
		return c.leaf + l.keys + index*l.keyStride
	}
	if l.headKey >= 0 {
		return c.leaf + l.headKey
	}
	for i := c.depth - 1; i >= 0; i-- {
		f := c.path[i]
		if f.index > 0 {
			return m.separatorSlot(f.node, f.index-1)
		}
	}
	panic("gobbtree: validated leaf has no head key")
}

func (c *fileCursor[K, V]) key(index int) K {
	return diskRead[K](c.mapFile.data, c.keySlot(index), c.mapFile.layout.key)
}

func (c *fileCursor[K, V]) advanceLeaf() bool {
	m := c.mapFile
	for c.depth > 0 {
		f := &c.path[c.depth-1]
		n := int(binary.NativeEndian.Uint16(m.data[f.node:]))
		if (!c.reverse && f.index < n) || (c.reverse && f.index > 0) {
			if c.reverse {
				f.index--
			} else {
				f.index++
			}
			c.descend(m.child(f.node, f.index), false)
			return true
		}
		c.depth--
	}
	return false
}

func (c *fileCursor[K, V]) next() (key K, value V, ok bool) {
	m := c.mapFile
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c.done || m.root == 0 {
		return
	}
	if !c.started {
		c.started = true
		c.descend(m.root, c.reverse && c.bounds.upperKind != unbounded || !c.reverse && c.bounds.lowerKind != unbounded)
	}
	for c.index < 0 || c.index >= c.count() {
		if !c.advanceLeaf() {
			c.done = true
			return
		}
	}
	key = c.key(c.index)
	if !aboveLower[K, natural[K]](c.bounds, key) || !belowUpper[K, natural[K]](c.bounds, key) {
		c.done = true
		return key, value, false
	}
	l := m.layout.leaf
	i := c.index + 1 - int(m.data[c.leaf+2])
	value = diskRead[V](m.data, c.leaf+l.trailing+i*l.stride, m.layout.value)
	if c.reverse {
		c.index--
	} else {
		c.index++
	}
	return key, value, true
}

func (m *FileMap[K, V]) find(key K) (keySlot, valueSlot int, ok bool) {
	if m.root == 0 || m.compareShared(key) != 0 {
		return
	}
	c := fileCursor[K, V]{mapFile: m, bounds: &Bounds[K]{lower: key, lowerKind: included}}
	c.descend(m.root, true)
	if c.index >= c.count() {
		return
	}
	keySlot = c.keySlot(c.index)
	if diskCompare(m.data, keySlot, m.layout.key, key) != 0 {
		return keySlot, 0, false
	}
	l := m.layout.leaf
	i := c.index + 1 - int(m.data[c.leaf+2])
	return keySlot, c.leaf + l.trailing + i*l.stride, true
}

func (m *FileMap[K, V]) Get(key K) (value V, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, slot, ok := m.find(key)
	if ok {
		value = diskRead[V](m.data, slot, m.layout.value)
	}
	return
}

func (m *FileMap[K, V]) GetKeyValue(key K) (stored K, value V, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ks, vs, ok := m.find(key)
	if ok {
		stored = diskRead[K](m.data, ks, m.layout.key)
		value = diskRead[V](m.data, vs, m.layout.value)
	}
	return
}

func (m *FileMap[K, V]) Contains(key K) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, _, ok := m.find(key)
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i, key := range keys {
		_, slot, ok := m.find(key)
		var value V
		if ok {
			value = diskRead[V](m.data, slot, m.layout.value)
		}
		dst[i] = Result[V]{value, ok}
	}
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.data == nil {
		return 0, os.ErrClosed
	}
	n, err := w.Write(m.data)
	if err == nil && n != len(m.data) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}
