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

// OpenFileMap validates and takes an owned copy of a serialized snapshot.
func OpenFileMap[K cmp.Ordered, V any](data []byte) (*FileMap[K, V], error) {
	return openFileMap[K, V](append([]byte(nil), data...), nil)
}

func openFileMap[K cmp.Ordered, V any](data []byte, unmap func() error) (*FileMap[K, V], error) {
	l, err := newDiskLayout[K, V]()
	if err != nil {
		return nil, err
	}
	root, count, err := l.validate(data)
	if err != nil {
		return nil, err
	}
	m := &FileMap[K, V]{data: data[:int(binary.LittleEndian.Uint64(data[40:]))], layout: l, root: root, length: count, unmap: unmap}
	if unmap != nil {
		// The cleanup argument holds the mapping, never m. Explicit Close
		// releases it promptly; unreachable maps also release OS resources.
		m.cleanup = runtime.AddCleanup(m, func(release func() error) { _ = release() }, unmap)
	}
	return m, nil
}

// ReadFile loads and validates a snapshot without rebuilding its tree.
func ReadFile[K cmp.Ordered, V any](path string) (*FileMap[K, V], error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return openFileMap[K, V](data, nil)
}

// ReadFileRange loads a snapshot embedded at offset in a larger file.
func ReadFileRange[K cmp.Ordered, V any](file *os.File, offset, length int64) (*FileMap[K, V], error) {
	if err := checkFileRange(file, offset, length); err != nil {
		return nil, err
	}
	data := make([]byte, int(length))
	if _, err := file.ReadAt(data, offset); err != nil {
		return nil, err
	}
	return openFileMap[K, V](data, nil)
}

func checkFileRange(file *os.File, offset, length int64) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if offset < 0 || length < fileHeaderSize || uint64(length) > uint64(^uint(0)>>1) || offset > info.Size() || length > info.Size()-offset {
		return fmt.Errorf("%w: file range", ErrFileFormat)
	}
	return nil
}

// MmapFile validates a read-only mapping. The file must not be modified or
// truncated while mapped; CreateFile publishes a replacement by renaming.
func MmapFile[K cmp.Ordered, V any](path string) (*FileMap[K, V], error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return MmapFileRange[K, V](f, 0, info.Size())
}

// MmapFileRange maps an embedded snapshot. The caller may close file after
// this returns; FileMap.Close releases the independent mapping.
func MmapFileRange[K cmp.Ordered, V any](file *os.File, offset, length int64) (*FileMap[K, V], error) {
	if err := checkFileRange(file, offset, length); err != nil {
		return nil, err
	}
	data, unmap, err := mapFileReadOnly(file, offset, int(length))
	if err != nil {
		return nil, err
	}
	m, err := openFileMap[K, V](data, unmap)
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
	for tagged&1 == 0 {
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
	hasLower, hasUpper     bool
	lower, upper           K
}

func (c *fileCursor[K, V]) descend(tagged int, seek bool) {
	m := c.mapFile
	key := c.lower
	if c.reverse {
		key = c.upper
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
		c.descend(m.root, c.reverse && c.hasUpper || !c.reverse && c.hasLower)
	}
	for c.index < 0 || c.index >= c.count() {
		if !c.advanceLeaf() {
			c.done = true
			return
		}
	}
	key = c.key(c.index)
	if c.hasLower && cmp.Compare(key, c.lower) < 0 || c.hasUpper && cmp.Compare(key, c.upper) >= 0 {
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
	c := fileCursor[K, V]{mapFile: m, lower: key}
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
	c := fileCursor[K, V]{mapFile: m}
	return c.next()
}

func (m *FileMap[K, V]) Last() (K, V, bool) {
	c := fileCursor[K, V]{mapFile: m, reverse: true}
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

// scan visits keys in [lower, upper); a nil bound is open.
func (m *FileMap[K, V]) scan(lower, upper *K, reverse bool, visit func(K, V) bool) {
	if lower != nil && upper != nil && cmp.Compare(*lower, *upper) >= 0 {
		return
	}
	c := fileCursor[K, V]{mapFile: m, reverse: reverse, hasLower: lower != nil, hasUpper: upper != nil}
	if lower != nil {
		c.lower = *lower
	}
	if upper != nil {
		c.upper = *upper
	}
	for {
		k, v, ok := c.next()
		if !ok || !visit(k, v) {
			return
		}
	}
}

// Range iterates the entries with keys in [lower, upper), ascending.
func (m *FileMap[K, V]) Range(lower, upper K) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) { m.scan(&lower, &upper, false, yield) }
}

// RangeReverse iterates the entries with keys in [lower, upper), descending.
func (m *FileMap[K, V]) RangeReverse(lower, upper K) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) { m.scan(&lower, &upper, true, yield) }
}

// Ascend iterates the entries with keys at or above from, ascending.
func (m *FileMap[K, V]) Ascend(from K) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) { m.scan(&from, nil, false, yield) }
}

// Descend iterates the entries with keys at or below from, descending.
func (m *FileMap[K, V]) Descend(from K) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if k, v, ok := m.GetKeyValue(from); ok && !yield(k, v) {
			return
		}
		m.scan(nil, &from, true, yield)
	}
}

func (m *FileMap[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) { m.scan(nil, nil, false, yield) }
}
func (m *FileMap[K, V]) Reverse() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) { m.scan(nil, nil, true, yield) }
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
