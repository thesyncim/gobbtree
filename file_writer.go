package gobbtree

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"unsafe"
)

// FileWriter owns a compact snapshot built from strictly ascending entries.
// It can be written repeatedly; subsequent changes to the input do not affect it.
type FileWriter[K cmp.Ordered, V any] struct {
	layout          diskLayout
	tree            *diskTree
	length, byteLen int
	header          [fileHeaderSize]byte
	// Where the root page is, where each height's first page is, and with
	// heap pages, where each leaf is.
	rootOffset  int
	levelOffset []int
	leafOffset  []int
}

// NewFileWriter copies entries once, rejecting duplicates and descending keys
// before any destination file is touched. Integer/string keys and plain numeric,
// array, string, byte-slice or empty-struct values use the upstream disk layouts.
//
// It builds the tree upstream's FileWriter builds for the same entries
// (file_build.go), so that the file map's bytes are upstream's: full nodes,
// the right border topped up past the minimum, the same prefixes, inline
// keys, hash tags and heap pages.
func NewFileWriter[K cmp.Ordered, V any](entries iter.Seq2[K, V]) (*FileWriter[K, V], error) {
	l, err := newDiskLayout[K, V]()
	if err != nil {
		return nil, err
	}
	w := &FileWriter[K, V]{layout: l, byteLen: fileHeaderSize}
	t := newDiskTree(&w.layout)
	w.tree = t
	ks := l.key.size
	stringValue := l.value.kind == reflect.String
	var previous K
	var valueRef uint64
	for key, value := range entries {
		if t.length > 0 && cmp.Compare(previous, key) >= 0 {
			return nil, &NotAscendingError{index: t.length}
		}
		previous = key
		// What the key's and value's slots hold while the tree is built:
		// their own bytes, or a byte string's arena reference, where
		// upstream's writer holds an ArenaBytes.
		var k diskKey
		if t.varKey {
			// The only ordered keys of a string's size are strings.
			s := *(*string)(unsafe.Pointer(&key))
			ref, err := t.arena.push(unsafe.Slice(unsafe.StringData(s), len(s)))
			if err != nil {
				return nil, err
			}
			binary.NativeEndian.PutUint64(k[:], ref)
		} else if ks == 8 {
			binary.NativeEndian.PutUint64(k[:], binary.NativeEndian.Uint64(unsafe.Slice((*byte)(unsafe.Pointer(&key)), 8)))
		} else {
			copy(k[:ks], unsafe.Slice((*byte)(unsafe.Pointer(&key)), ks))
		}
		v := unsafe.Pointer(&value)
		if t.varValue {
			b := *(*[]byte)(v)
			if stringValue {
				s := *(*string)(v)
				b = unsafe.Slice(unsafe.StringData(s), len(s))
			}
			if valueRef, err = t.arena.push(b); err != nil {
				return nil, err
			}
			v = unsafe.Pointer(&valueRef)
		}
		t.push(&k, v)
	}
	if err = t.finish(); err != nil {
		return nil, err
	}
	w.length = t.length
	return w, w.layOut()
}

// layOut places the tree's pages as upstream's FilePages::of does, in
// level order: the root page, then each height's pages below it, left to
// right, each at a multiple of its alignment, every leaf followed by its
// heap pages. It then writes each edge as its child's offset from its
// node, tagged with the child's kind (D52), and the header.
func (w *FileWriter[K, V]) layOut() error {
	t, l := w.tree, &w.layout
	copy(w.header[:8], "bbtreeIM")
	binary.LittleEndian.PutUint32(w.header[8:], 1)
	binary.LittleEndian.PutUint64(w.header[16:], l.fingerprint)
	binary.LittleEndian.PutUint64(w.header[24:], uint64(w.length))
	if t.root == nil {
		binary.LittleEndian.PutUint64(w.header[40:], fileHeaderSize)
		return nil
	}
	// The root area, which upstream keeps in the root page all along.
	copy(t.root[l.area:], t.shared[:])
	tooLarge := fmt.Errorf("%w: file map exceeds the address space", ErrFileFormat)
	heap := func(p []byte) (int, error) {
		n := t.heapPages(p, nil, nil)
		if uint64(len(p))+uint64(n) > math.MaxUint32 {
			return 0, fmt.Errorf("%w: byte span exceeds uint32", ErrFileFormat)
		}
		return n, nil
	}
	w.rootOffset = diskAlign(fileHeaderSize, l.rootAlign)
	position := w.rootOffset + l.rootSize
	if t.height == 0 && (t.varKey || t.varValue) {
		n, err := heap(t.root)
		if err != nil {
			return err
		}
		position += n
	}
	w.levelOffset = make([]int, t.height)
	for h := t.height - 1; h >= 0; h-- {
		nl, _ := t.layoutAt(h)
		pages := &t.levels[h]
		if h == 0 && (t.varKey || t.varValue) {
			w.leafOffset = make([]int, pages.count)
			for i := range pages.count {
				n, err := heap(pages.page(i))
				if err != nil {
					return err
				}
				if position > math.MaxInt-nl.align-nl.size-n {
					return tooLarge
				}
				w.leafOffset[i] = diskAlign(position, nl.align)
				position = w.leafOffset[i] + nl.size + n
			}
			continue
		}
		if position > math.MaxInt-nl.align-pages.count*nl.size {
			return tooLarge
		}
		w.levelOffset[h] = diskAlign(position, nl.align)
		position = w.levelOffset[h] + pages.count*nl.size
	}
	in := &l.internal
	for h := t.height; h > 0; h-- {
		count := 1
		if h < t.height {
			count = t.levels[h].count
		}
		tag := 0
		if h == 1 {
			tag = 1
		}
		for i := range count {
			p, offset := t.page(h, i), w.offset(h, i)
			n := nodeLen(p)
			// offset_edges: every edge slot, zeros past the last edge.
			for e := 0; e <= in.capacity; e++ {
				edge := 0
				if e <= n {
					edge = (w.offset(h-1, t.child(h, i, e)) - offset) | tag
				}
				binary.NativeEndian.PutUint64(p[in.trailing+e*8:], uint64(edge))
			}
		}
	}
	root := w.rootOffset
	if t.height == 0 {
		root |= 1
	}
	w.byteLen = position
	binary.LittleEndian.PutUint64(w.header[32:], uint64(root))
	binary.LittleEndian.PutUint64(w.header[40:], uint64(position))
	return nil
}

// offset is where page i at height h lies in the file map.
func (w *FileWriter[K, V]) offset(h, i int) int {
	t := w.tree
	if h == t.height {
		return w.rootOffset
	}
	if h == 0 && w.leafOffset != nil {
		return w.leafOffset[i]
	}
	return w.levelOffset[h] + i*t.levels[h].size
}

// heapPages lays out the records of the leaf src's entries in its heap
// pages, as upstream's HeapPages::lay_out: each entry's key's bytes, then
// its value's, whichever a span keeps, in key order, the head's first. A
// record stays in one page, starting the next if it doesn't fit in what is
// left of one; one larger than a page starts on a page and runs on. With
// dst, a copy of src as the file map keeps it, its slots zeroed, it writes
// each span there, its offset from its own slot, and the bytes into heap.
// Returns the pages' length, a multiple of their size.
func (t *diskTree) heapPages(src, dst, heap []byte) int {
	nl := &t.l.leaf
	top := 0
	n := nodeLen(src)
	for j := -1; j < n; j++ {
		keySlot, valueSlot := nl.keys+j*nl.keyStride, nl.trailing+(j+1)*nl.stride
		if j < 0 {
			if src[2] == 0 {
				continue
			}
			// A head keeps its key only with inline keys (D49).
			keySlot = nl.headKey
		}
		var key, value []byte
		if t.varKey {
			key = t.arena.bytes(binary.NativeEndian.Uint64(src[keySlot:]))
		}
		if t.varValue {
			value = t.arena.bytes(binary.NativeEndian.Uint64(src[valueSlot:]))
		}
		size := len(key) + len(value)
		if size > diskHeapPage-top%diskHeapPage {
			top = diskAlign(top, diskHeapPage)
		}
		if dst != nil {
			if t.varKey {
				putSpan(dst, keySlot, top, key, heap)
			}
			if t.varValue {
				putSpan(dst, valueSlot, top+len(key), value, heap)
			}
		}
		top += size
	}
	return diskAlign(top, diskHeapPage)
}

// putSpan writes record into heap at at, and into page's slot its span,
// {len, offset}, native-endian, the offset from the slot, as upstream's
// VarBytes::encode.
func putSpan(page []byte, slot, at int, record, heap []byte) {
	binary.NativeEndian.PutUint32(page[slot:], uint32(len(record)))
	binary.NativeEndian.PutUint32(page[slot+4:], uint32(len(page)+at-slot))
	copy(heap[at:], record)
}

func (w *FileWriter[K, V]) Len() int       { return w.length }
func (w *FileWriter[K, V]) IsEmpty() bool  { return w.length == 0 }
func (w *FileWriter[K, V]) ByteLen() int   { return w.byteLen }
func (w *FileWriter[K, V]) Alignment() int { return w.layout.rootAlign }

// WriteTo hands the file map to dst as upstream's FilePages::write does: the
// header, then each page in level order after the zeros that align it, each
// leaf followed by its heap pages.
func (w *FileWriter[K, V]) WriteTo(dst io.Writer) (written int64, err error) {
	put := func(data []byte) bool {
		var n int
		n, err = dst.Write(data)
		written += int64(n)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		return err == nil
	}
	var zero [4096]byte
	pad := func(to int) bool {
		for written < int64(to) {
			if !put(zero[:min(int64(len(zero)), int64(to)-written)]) {
				return false
			}
		}
		return true
	}
	if !put(w.header[:]) {
		return
	}
	t := w.tree
	if t.root == nil {
		return
	}
	heaped := t.varKey || t.varValue
	// Leaves of keys or values with bytes are written from a copy: their
	// slots hold arena references, which become spans to their heap pages.
	var page, heap []byte
	leafBytes := func(src []byte) bool {
		page = append(page[:0], src...)
		t.zeroVarSlots(page)
		n := t.heapPages(src, nil, nil)
		if cap(heap) < n {
			heap = make([]byte, n)
		}
		heap = heap[:n]
		clear(heap)
		t.heapPages(src, page, heap)
		return put(page) && put(heap)
	}
	if !pad(w.rootOffset) {
		return
	}
	if t.height == 0 && heaped {
		if !leafBytes(t.root) {
			return
		}
	} else if !put(t.root) {
		return
	}
	for h := t.height - 1; h >= 0; h-- {
		pages := &t.levels[h]
		if h == 0 && heaped {
			for i := range pages.count {
				if !pad(w.leafOffset[i]) || !leafBytes(pages.page(i)) {
					return
				}
			}
			continue
		}
		if !pad(w.levelOffset[h]) {
			return
		}
		for c := range pages.chunks {
			if !put(pages.chunk(c)) {
				return
			}
		}
	}
	return
}

func (w *FileWriter[K, V]) Bytes() []byte {
	var b bytes.Buffer
	b.Grow(w.byteLen)
	_, _ = w.WriteTo(&b)
	return b.Bytes()
}

// FileRange identifies an embedded snapshot by its offset and byte length.
type FileRange struct{ Offset, Length int64 }

// WriteAt writes the snapshot into file at offset rounded up to its
// alignment, after zeros, in place of what lay there, and ends the file
// after it, as upstream's write_at does: it truncates the file at offset,
// seeks there and writes, so a file opened to append takes it too. As
// upstream's, it doesn't sync the file: Sync it for the snapshot to be
// durable. On an error the range is zero, and the file may end anywhere
// past offset.
func (w *FileWriter[K, V]) WriteAt(file *os.File, offset int64) (FileRange, error) {
	align := int64(w.Alignment())
	if offset < 0 || offset > int64(^uint64(0)>>1)-align-int64(w.byteLen) {
		return FileRange{}, ErrFileFormat
	}
	start := (offset + align - 1) & -align
	if err := file.Truncate(offset); err != nil {
		return FileRange{}, err
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return FileRange{}, err
	}
	out := bufio.NewWriterSize(file, 1<<20)
	if _, err := out.Write(make([]byte, start-offset)); err != nil {
		return FileRange{}, err
	}
	if _, err := w.WriteTo(out); err != nil {
		return FileRange{}, err
	}
	if err := out.Flush(); err != nil {
		return FileRange{}, err
	}
	return FileRange{start, int64(w.byteLen)}, nil
}

// WriteFile publishes a complete snapshot atomically: it writes a new file
// in the destination's directory, syncs it, renames it over the destination
// and syncs the directory, so that a crash leaves the old file or the new
// one. A destination that is a symbolic link has its target replaced, and a
// file it replaces keeps its permissions; a new one takes the umask's, as
// os.Create does. An existing mapped snapshot remains valid across
// replacement. An error from syncing the directory comes after the rename:
// the new file is in place, though perhaps not yet durably.
func (w *FileWriter[K, V]) WriteFile(path string) (err error) {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	perm, keep := os.FileMode(0o666), false
	if info, err := os.Stat(path); err == nil {
		perm, keep = info.Mode().Perm(), true
	}
	dir := filepath.Dir(path)
	f, err := createTempIn(dir, perm)
	if err != nil {
		return err
	}
	name, renamed := f.Name(), false
	defer func() {
		if !renamed {
			_ = f.Close()
			_ = os.Remove(name)
		}
	}()
	if keep {
		// The umask doesn't apply to an explicit mode.
		if err = f.Chmod(perm); err != nil {
			return err
		}
	}
	out := bufio.NewWriterSize(f, 1<<20)
	if _, err = w.WriteTo(out); err != nil {
		return err
	}
	if err = out.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	renamed = true
	return syncDir(dir)
}

// createTempIn creates a new file in dir for WriteFile, with mode perm less
// the umask, as os.CreateTemp does with 0600.
func createTempIn(dir string, perm os.FileMode) (*os.File, error) {
	for range 10000 {
		name := filepath.Join(dir, ".bbtree-"+strconv.FormatUint(rand.Uint64(), 36))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
	return nil, &fs.PathError{Op: "createtemp", Path: filepath.Join(dir, ".bbtree-*"), Err: fs.ErrExist}
}

func CreateFile[K cmp.Ordered, V any](path string, entries iter.Seq2[K, V]) error {
	w, err := NewFileWriter(entries)
	if err != nil {
		return err
	}
	return w.WriteFile(path)
}

func (t *Tree[K, V]) WriteFile(path string) error { return CreateFile(path, t.All()) }

// WriteFile writes the entries a scan visits; under concurrent writes the
// file is not a snapshot.
func (m *Map[K, V]) WriteFile(path string) error { return CreateFile(path, m.All()) }
