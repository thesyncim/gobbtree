package gobbtree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"unsafe"
)

var (
	ErrFileFormat      = errors.New("gobbtree: invalid file map")
	ErrFileLayout      = errors.New("gobbtree: incompatible file layout")
	ErrFileType        = errors.New("gobbtree: unsupported file type")
	ErrNotAscending    = errors.New("gobbtree: file entries must be strictly ascending")
	ErrMmapUnavailable = errors.New("gobbtree: mmap unavailable on this platform")
)

// NotAscendingError reports entries given to NewFileWriter whose keys don't
// ascend strictly, as upstream's NotAscending does. It matches
// ErrNotAscending under errors.Is.
type NotAscendingError struct{ index int }

// Index is the index of the first entry whose key is not greater than the
// key before it, as upstream's NotAscending::index.
func (e *NotAscendingError) Index() int { return e.index }

func (e *NotAscendingError) Error() string {
	return fmt.Sprintf("gobbtree: entry %d has a key not greater than the key before it", e.index)
}

func (e *NotAscendingError) Is(target error) bool { return target == ErrNotAscending }

const fileHeaderSize = 64
const fileMaxDepth = 23

// The layout follows bbtree's version 1 FileMap format. Page words have
// native byte order; the outer header and its fingerprint use little endian.
type diskType struct {
	size, align int
	kind        reflect.Kind
	variable    bool
}

func diskTypeOf(t reflect.Type) (diskType, error) {
	d := diskType{size: int(t.Size()), align: t.Align(), kind: t.Kind()}
	switch t.Kind() {
	case reflect.String:
		d.size, d.align, d.variable = 8, 8, true
	case reflect.Slice:
		if t.Elem().Kind() != reflect.Uint8 {
			return d, fmt.Errorf("%w: %v", ErrFileType, t)
		}
		d.size, d.align, d.variable = 8, 8, true
	case reflect.Array:
		e, err := diskTypeOf(t.Elem())
		if err != nil || e.variable {
			return d, fmt.Errorf("%w: %v", ErrFileType, t)
		}
	case reflect.Struct:
		if t.NumField() != 0 {
			return d, fmt.Errorf("%w: %v", ErrFileType, t)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr,
		reflect.Float32, reflect.Float64:
	default:
		return d, fmt.Errorf("%w: %v", ErrFileType, t)
	}
	return d, nil
}

func diskRead[T any](data []byte, slot int, d diskType) T {
	if !d.variable {
		return diskReadPlain[T](data, slot, d.size)
	}
	return diskReadVariable[T](data, slot, d)
}

// diskReadPlain reads a plain T, of size bytes, from slot. Plain types hold
// no pointers, which lets slot be unaligned.
func diskReadPlain[T any](data []byte, slot, size int) (out T) {
	if size != 0 {
		out = *(*T)(unsafe.Pointer(unsafe.SliceData(data[slot : slot+size])))
	}
	return
}

// diskReadVariable reads a string or byte slice T, a copy of the bytes the
// span in slot points to.
func diskReadVariable[T any](data []byte, slot int, d diskType) (out T) {
	n := int(binary.NativeEndian.Uint32(data[slot:]))
	start := slot + int(binary.NativeEndian.Uint32(data[slot+4:]))
	b := data[start : start+n]
	if d.kind == reflect.String {
		*(*string)(unsafe.Pointer(&out)) = string(b)
	} else {
		*(*[]byte)(unsafe.Pointer(&out)) = append([]byte(nil), b...)
	}
	return
}

func diskPut[T any](data *[]byte, slot int, value T, d diskType) error {
	if !d.variable {
		if d.size != 0 {
			copy((*data)[slot:slot+d.size], unsafe.Slice((*byte)(unsafe.Pointer(&value)), d.size))
		}
		return nil
	}
	v := reflect.ValueOf(value)
	if uint64(v.Len()) >= uint64(1)<<32-1 || uint64(len(*data)-slot) > uint64(1)<<32-1 {
		return fmt.Errorf("%w: byte span exceeds uint32", ErrFileFormat)
	}
	binary.NativeEndian.PutUint32((*data)[slot:], uint32(v.Len()))
	binary.NativeEndian.PutUint32((*data)[slot+4:], uint32(len(*data)-slot))
	if d.kind == reflect.String {
		*data = append(*data, v.String()...)
	} else {
		*data = append(*data, v.Bytes()...)
	}
	return nil
}

type diskNodeLayout struct {
	size, align, capacity           int
	width, inline, keys, keyStride  int
	trailing, stride, tags, headKey int
}

type diskLayout struct {
	key, value                diskType
	leaf, internal            diskNodeLayout
	rootSize, rootAlign, area int
	fingerprint               uint64
}

func diskAlign(n, a int) int { return (n + a - 1) & -a }

func diskNode(key, value diskType, width, tagSize int, leaf bool) diskNodeLayout {
	page, trailing, keep := 512, diskType{size: 8, align: 8}, width == 0
	if leaf {
		page, trailing, keep = 1024, value, true
	} else if keep {
		page = 1024
	}
	align := max(page, trailing.align)
	if keep {
		align = max(align, key.align)
	}
	pair := leaf && width != 0
	entryAlign := max(key.align, trailing.align)
	valueOffset := diskAlign(key.size, trailing.align)
	entrySize := diskAlign(valueOffset+trailing.size, entryAlign)
	columns := func(n int) int {
		if width != 0 {
			return 8 + n*(width+tagSize)
		}
		return 4
	}
	needed := func(n int) int {
		start := columns(n)
		if pair {
			return diskAlign(start, entryAlign) + (n+1)*entrySize
		}
		if keep {
			return diskAlign(diskAlign(start, key.align)+n*key.size, trailing.align) + (n+1)*trailing.size
		}
		return diskAlign(start, trailing.align) + (n+1)*trailing.size
	}
	size := diskAlign(needed(12), align)
	capacity := 1023
	if width > 0 {
		capacity = min(capacity, max(page, 1024)/width)
	}
	for needed(capacity) > size {
		capacity--
	}
	n := diskNodeLayout{size: size, align: align, capacity: capacity, width: width, inline: -1, tags: -1, headKey: -1}
	if width > 0 {
		n.inline = 8
	}
	start := columns(capacity)
	if pair {
		a := entryAlign
		if entrySize > 0 && entrySize&(entrySize-1) == 0 && entrySize <= 64 {
			a = entrySize
		}
		entries := diskAlign(start, a)
		n.keys, n.keyStride, n.trailing, n.stride = entries+entrySize, entrySize, entries+valueOffset, entrySize
		n.headKey = entries
	} else if keep {
		n.keys, n.keyStride = diskAlign(start, key.align), key.size
		n.trailing, n.stride = diskAlign(n.keys+capacity*key.size, trailing.align), trailing.size
	} else {
		n.keys, n.trailing, n.stride = start, diskAlign(start, trailing.align), trailing.size
	}
	return n
}

func newDiskLayout[K, V any]() (diskLayout, error) {
	var l diskLayout
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return l, fmt.Errorf("%w: requires 64-bit pointers", ErrFileLayout)
	}
	var err error
	if l.key, err = diskTypeOf(reflect.TypeFor[K]()); err != nil {
		return l, err
	}
	if l.value, err = diskTypeOf(reflect.TypeFor[V]()); err != nil {
		return l, err
	}
	if l.key.kind == reflect.Float32 || l.key.kind == reflect.Float64 {
		return l, fmt.Errorf("%w: floating-point keys", ErrFileType)
	}
	lw, iw, source := 0, 0, 0
	switch l.key.kind {
	case reflect.Int64, reflect.Uint64:
		lw, iw = 2, 2
	case reflect.String:
		lw, iw, source = 8, 4, 1
	}
	l.leaf = diskNode(l.key, l.value, lw, 0, true)
	if l.key.variable {
		tags := 8 + lw*l.leaf.capacity
		if tags+l.leaf.capacity <= l.leaf.headKey {
			l.leaf.tags = tags
		} else if tags+l.leaf.capacity <= l.leaf.trailing {
			l.leaf = diskNode(l.key, l.value, lw, 1, true)
			l.leaf.tags = 8 + lw*l.leaf.capacity
		}
	}
	l.internal = diskNode(l.key, l.value, iw, 0, false)
	l.area, l.rootAlign = max(l.leaf.size, l.internal.size), max(l.leaf.align, l.internal.align)
	l.rootSize = l.area + 65
	b := l.leaf
	in := l.internal
	flag := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	fields := []int{64, flag(binary.NativeEndian.Uint16([]byte{1, 0}) == 1), l.key.size, l.key.align, l.value.size, l.value.align,
		flag(l.key.variable), flag(l.value.variable), source, lw, flag(b.tags >= 0), iw,
		b.size, b.align, b.capacity, b.inline, b.keys, b.keyStride, b.trailing, b.stride, b.tags, b.headKey,
		in.size, in.align, in.capacity, in.inline, in.keys, in.keyStride, in.trailing, in.stride, l.rootSize, l.rootAlign, l.area}
	l.fingerprint = 0xcbf29ce484222325
	for _, field := range fields {
		v := uint64(field)
		for range 8 {
			l.fingerprint ^= v & 255
			l.fingerprint *= 0x100000001b3
			v >>= 8
		}
	}
	return l, nil
}

// header checks data's header alone, as upstream's FileHeader::read does:
// the layout, the lengths, and that the root page lies in the map.
func (l diskLayout) header(data []byte) (root, count int, err error) {
	bad := func(reason string) (int, int, error) { return 0, 0, fmt.Errorf("%w: %s", ErrFileFormat, reason) }
	if len(data) < 64 || string(data[:8]) != "bbtreeIM" {
		return bad("header")
	}
	if binary.LittleEndian.Uint32(data[8:]) != 1 {
		return bad("unsupported version")
	}
	if binary.LittleEndian.Uint64(data[16:]) != l.fingerprint {
		return 0, 0, ErrFileLayout
	}
	word := func(off int) uint64 { return binary.LittleEndian.Uint64(data[off:]) }
	if word(24) > uint64(^uint(0)>>1) || word(32) > uint64(^uint(0)>>1) || word(40) > uint64(len(data)) || word(40) < 64 {
		return bad("length")
	}
	root, count = int(word(32)), int(word(24))
	data = data[:int(word(40))]
	if root == 0 {
		if count != 0 {
			return bad("entries without a root")
		}
		return root, count, nil
	}
	ro := root &^ 1
	if ro < 64 || ro%l.rootAlign != 0 || ro > len(data)-l.rootSize || data[ro+l.area] > 64 {
		return bad("root page")
	}
	return root, count, nil
}

// validate checks the header and every page the tree reaches, as upstream's
// FileMap::open does.
func (l diskLayout) validate(data []byte) (root, count int, err error) {
	if root, count, err = l.header(data); err != nil || root == 0 {
		return root, count, err
	}
	bad := func(reason string) (int, int, error) { return 0, 0, fmt.Errorf("%w: %s", ErrFileFormat, reason) }
	data = data[:int(binary.LittleEndian.Uint64(data[40:]))]
	var first, end [fileMaxDepth + 1]int
	leafDepth, entries, leafCount := -1, 0, 0
	checkSpan := func(slot int, d diskType) bool {
		if !d.variable {
			return true
		}
		n, off := uint64(binary.NativeEndian.Uint32(data[slot:])), uint64(binary.NativeEndian.Uint32(data[slot+4:]))
		return n != 1<<32-1 && uint64(slot)+off+n <= uint64(len(data))
	}
	var walk func(int, int) bool
	walk = func(tagged, depth int) bool {
		if depth > fileMaxDepth {
			return false
		}
		off, leaf := tagged&^1, tagged&1 != 0
		nl := l.internal
		if leaf {
			nl = l.leaf
		}
		size, align := nl.size, nl.align
		if depth == 0 {
			size, align = l.rootSize, l.rootAlign
		}
		if off < 64 || off%align != 0 || off > len(data)-size || off < end[depth] {
			return false
		}
		if first[depth] == 0 {
			first[depth] = off
		}
		end[depth] = off + size
		n, head := int(binary.NativeEndian.Uint16(data[off:])), int(data[off+2])
		if n > nl.capacity || head > 1 {
			return false
		}
		if leaf {
			if leafDepth < 0 {
				leafDepth = depth
			}
			if depth != leafDepth {
				return false
			}
			if (leafCount == 0 && head != 0) || (leafCount != 0 && head != 1) {
				return false
			}
			leafCount++
			for i := 1 - head; i <= n; i++ {
				if !checkSpan(off+nl.trailing+i*nl.stride, l.value) {
					return false
				}
				ks := off + nl.keys + (i-1)*nl.keyStride
				if i == 0 {
					ks = off + nl.headKey
				}
				if l.key.variable && !checkSpan(ks, l.key) {
					return false
				}
			}
			if n+head > count-entries {
				return false
			}
			entries += n + head
			return true
		}
		if head != 0 {
			return false
		}
		for i := 0; i <= n; i++ {
			delta := binary.NativeEndian.Uint64(data[off+nl.trailing+i*nl.stride:])
			if delta < 2 || delta > uint64(len(data)-off) {
				return false
			}
			if !walk(off+int(delta), depth+1) {
				return false
			}
		}
		return true
	}
	if !walk(root, 0) || entries != count {
		return bad("page graph or entries")
	}
	for i := 1; i <= leafDepth; i++ {
		if first[i] < end[i-1] {
			return bad("overlapping levels")
		}
	}
	return root, count, nil
}
