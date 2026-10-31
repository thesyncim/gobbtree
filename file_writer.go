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
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
)

type diskFragment struct {
	offset int
	data   []byte
}

// FileWriter owns a compact snapshot built from strictly ascending entries.
// It can be written repeatedly; subsequent changes to the input do not affect it.
type FileWriter[K cmp.Ordered, V any] struct {
	layout          diskLayout
	length, byteLen int
	header          [fileHeaderSize]byte
	pages           []diskFragment
}

type diskBuildNode[K cmp.Ordered] struct {
	data          []byte
	first         K
	children      []*diskBuildNode[K]
	count, offset int
}

func diskEncoding[K cmp.Ordered](key K) []byte {
	v := reflect.ValueOf(key)
	if v.Kind() == reflect.String {
		return []byte(v.String())
	}
	var b [8]byte
	var word uint64
	if v.Kind() == reflect.Int64 {
		word = uint64(v.Int()) ^ (1 << 63)
	} else {
		word = v.Uint()
	}
	binary.BigEndian.PutUint64(b[:], word)
	return b[:]
}

func diskCommon(a, b []byte, limit int) int {
	n := min(len(a), len(b), limit)
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func diskInline[K cmp.Ordered](data []byte, slot, width, prefix int, key K) {
	encoded := diskEncoding(key)
	var word uint64
	for i := 0; i < width; i++ {
		word <<= 8
		if prefix+i < len(encoded) {
			word |= uint64(encoded[prefix+i])
		}
	}
	switch width {
	case 2:
		binary.NativeEndian.PutUint16(data[slot:], uint16(word))
	case 4:
		binary.NativeEndian.PutUint32(data[slot:], uint32(word))
	case 8:
		binary.NativeEndian.PutUint64(data[slot:], word)
	}
}

// NewFileWriter copies entries once, rejecting duplicates and descending keys
// before any destination file is touched. Integer/string keys and plain numeric,
// array, string, byte-slice or empty-struct values use the upstream disk layouts.
func NewFileWriter[K cmp.Ordered, V any](entries iter.Seq2[K, V]) (*FileWriter[K, V], error) {
	l, err := newDiskLayout[K, V]()
	if err != nil {
		return nil, err
	}
	w := &FileWriter[K, V]{layout: l, byteLen: 64}
	var leaves []*diskBuildNode[K]
	var current *diskBuildNode[K]
	var previous K
	for key, value := range entries {
		if w.length > 0 && cmp.Compare(previous, key) >= 0 {
			return nil, &NotAscendingError{index: w.length}
		}
		previous = key
		if current == nil || current.count == l.leaf.capacity+int(current.data[2]) {
			current = &diskBuildNode[K]{data: make([]byte, l.leaf.size), first: key}
			if len(leaves) > 0 {
				current.data[2] = 1
			}
			leaves = append(leaves, current)
		}
		index := current.count - int(current.data[2])
		ks := l.leaf.keys + index*l.leaf.keyStride
		if index < 0 {
			ks = l.leaf.headKey
		}
		if ks >= 0 {
			if err = diskPut(&current.data, ks, key, l.key); err != nil {
				return nil, err
			}
		}
		if err = diskPut(&current.data, l.leaf.trailing+(index+1)*l.leaf.stride, value, l.value); err != nil {
			return nil, err
		}
		current.count++
		w.length++
		binary.NativeEndian.PutUint16(current.data, uint16(current.count-int(current.data[2])))
	}
	copy(w.header[:8], "bbtreeIM")
	binary.LittleEndian.PutUint32(w.header[8:], 1)
	binary.LittleEndian.PutUint64(w.header[16:], l.fingerprint)
	binary.LittleEndian.PutUint64(w.header[24:], uint64(w.length))
	if len(leaves) == 0 {
		binary.LittleEndian.PutUint64(w.header[40:], 64)
		return w, nil
	}
	levels := [][]*diskBuildNode[K]{leaves}
	for len(levels[len(levels)-1]) > 1 {
		children := levels[len(levels)-1]
		var parents []*diskBuildNode[K]
		for start := 0; start < len(children); start += l.internal.capacity + 1 {
			group := children[start:min(start+l.internal.capacity+1, len(children))]
			n := &diskBuildNode[K]{data: make([]byte, l.internal.size), first: group[0].first, children: group, count: len(group) - 1}
			binary.NativeEndian.PutUint16(n.data, uint16(n.count))
			parents = append(parents, n)
		}
		levels = append(levels, parents)
	}
	if len(levels) > fileMaxDepth+1 {
		return nil, fmt.Errorf("%w: tree depth", ErrFileFormat)
	}
	root := levels[len(levels)-1][0]
	oldSize := l.internal.size
	if len(levels) == 1 {
		oldSize = l.leaf.size
	}
	extra := l.rootSize - oldSize
	rootData := make([]byte, len(root.data)+extra)
	copy(rootData, root.data[:oldSize])
	copy(rootData[l.rootSize:], root.data[oldSize:])
	root.data = rootData
	if len(levels) == 1 {
		for i := 1; i <= root.count; i++ {
			for _, slot := range []struct {
				pos int
				typ diskType
			}{{l.leaf.keys + (i-1)*l.leaf.keyStride, l.key}, {l.leaf.trailing + i*l.leaf.stride, l.value}} {
				if slot.typ.variable {
					off := binary.NativeEndian.Uint32(root.data[slot.pos+4:])
					if uint64(off)+uint64(extra) > 1<<32-1 {
						return nil, ErrFileFormat
					}
					binary.NativeEndian.PutUint32(root.data[slot.pos+4:], off+uint32(extra))
				}
			}
		}
	}
	var shared []byte
	if l.leaf.width > 0 {
		first, last := diskEncoding(leaves[0].first), diskEncoding(previous)
		shared = first[:diskCommon(first, last, 64)]
		root.data[l.area] = byte(len(shared))
		copy(root.data[l.area+1:], shared)
	}
	// Lay out all pages before encoding edges: offsets are relative to parents.
	position := 64
	for depth := len(levels) - 1; depth >= 0; depth-- {
		for i, n := range levels[depth] {
			nl := l.internal
			if depth == 0 {
				nl = l.leaf
			}
			align := nl.align
			if n == root {
				align = l.rootAlign
			}
			if position > int(^uint(0)>>1)-align-len(n.data) {
				return nil, ErrFileFormat
			}
			position = diskAlign(position, align)
			n.offset = position
			w.pages = append(w.pages, diskFragment{position, n.data})
			position += len(n.data)
			prefix := len(shared)
			if nl.width > 0 {
				if i > 0 && i+1 < len(levels[depth]) {
					prefix = diskCommon(diskEncoding(n.first), diskEncoding(levels[depth][i+1].first), 65535)
				}
				binary.NativeEndian.PutUint16(n.data[4:], uint16(prefix))
				for j := nl.inline; j < nl.inline+nl.capacity*nl.width; j++ {
					n.data[j] = 255
				}
			}
			length := int(binary.NativeEndian.Uint16(n.data))
			for j := 0; j < length; j++ {
				var key K
				if depth == 0 {
					key = diskRead[K](n.data, nl.keys+j*nl.keyStride, l.key)
				} else {
					key = n.children[j+1].first
				}
				if nl.width > 0 {
					diskInline(n.data, nl.inline+j*nl.width, nl.width, prefix, key)
				} else if depth != 0 {
					if err = diskPut(&n.data, nl.keys+j*nl.keyStride, key, l.key); err != nil {
						return nil, err
					}
				}
				if nl.tags >= 0 {
					n.data[nl.tags+j] = byte(min(len(diskEncoding(key)), 255))
				}
			}
		}
	}
	for depth := 1; depth < len(levels); depth++ {
		for _, n := range levels[depth] {
			for i, child := range n.children {
				edge := child.offset - n.offset
				if depth == 1 {
					edge |= 1
				}
				binary.NativeEndian.PutUint64(n.data[l.internal.trailing+i*l.internal.stride:], uint64(edge))
			}
		}
	}
	tagged := root.offset
	if len(levels) == 1 {
		tagged |= 1
	}
	w.byteLen = position
	binary.LittleEndian.PutUint64(w.header[32:], uint64(tagged))
	binary.LittleEndian.PutUint64(w.header[40:], uint64(position))
	return w, nil
}

func (w *FileWriter[K, V]) Len() int       { return w.length }
func (w *FileWriter[K, V]) IsEmpty() bool  { return w.length == 0 }
func (w *FileWriter[K, V]) ByteLen() int   { return w.byteLen }
func (w *FileWriter[K, V]) Alignment() int { return w.layout.rootAlign }

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
	if !put(w.header[:]) {
		return
	}
	var zero [4096]byte
	for _, page := range w.pages {
		for written < int64(page.offset) {
			if !put(zero[:min(int64(len(zero)), int64(page.offset)-written)]) {
				return
			}
		}
		if !put(page.data) {
			return
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
