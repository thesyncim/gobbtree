package gobbtree

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// The byte-identity gate: for the same entries, NewFileWriter writes the
// bytes upstream's writer does (bbtree-mmap's BBTreeMapFile::create, through
// the pinned Rust file oracle's write-entries mode), and reads them back.
// The one exception is bytes upstream's writer leaves stale: with keys or
// values of bytes, the root page keeps the slots of the leaf it held
// before the tree was raised, which hold that writer's pointers into its
// arena (ArenaBytes) and differ from run to run; Go's writer leaves zeros
// there (upstreamPointerMask).

// fileOracle is the Rust file oracle, or a skip.
func fileOracle(t *testing.T) string {
	t.Helper()
	oracle := os.Getenv("GOBBTREE_FILE_ORACLE")
	if oracle == "" {
		t.Skip("set GOBBTREE_FILE_ORACLE to the pinned Rust file-oracle binary")
	}
	return oracle
}

// appendEntryPart appends a key or value as the oracle's Decode reads it:
// a plain type's native-endian bytes; bytes as a u32 length and the bytes.
func appendEntryPart[T any](b []byte, v T, d diskType) []byte {
	if d.variable {
		var s []byte
		if d.kind == reflect.String {
			s = []byte(*(*string)(unsafe.Pointer(&v)))
		} else {
			s = *(*[]byte)(unsafe.Pointer(&v))
		}
		b = binary.NativeEndian.AppendUint32(b, uint32(len(s)))
		return append(b, s...)
	}
	return append(b, unsafe.Slice((*byte)(unsafe.Pointer(&v)), d.size)...)
}

// rustFileBytes has the oracle write the map of keys and values, whose
// types it names kname and vname, and returns the file.
func rustFileBytes[K cmp.Ordered, V any](t *testing.T, oracle, kname, vname string, keys []K, values []V) []byte {
	t.Helper()
	l, err := newDiskLayout[K, V]()
	if err != nil {
		t.Fatal(err)
	}
	in := binary.NativeEndian.AppendUint64(nil, uint64(len(keys)))
	for i := range keys {
		in = appendEntryPart(in, keys[i], l.key)
		in = appendEntryPart(in, values[i], l.value)
	}
	dir := t.TempDir()
	entries, out := filepath.Join(dir, "entries"), filepath.Join(dir, "rust.bbt")
	if err = os.WriteFile(entries, in, 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, err := exec.Command(oracle, "write-entries", kname, vname, entries, out).CombinedOutput(); err != nil {
		t.Fatalf("Rust write-entries %s %s: %v\n%s", kname, vname, err, msg)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// upstreamPointerMask marks the bytes of a file map whose keys or values
// have bytes where upstream's writer leaves its arena pointers: in the root
// page, when it is internal, and in each height's first page, which held
// the root before a raise, the slots for bytes of the leaf the root page
// held, past what an internal node writes over them (its header, inline
// keys and edges).
func upstreamPointerMask(l *diskLayout, data []byte) []bool {
	mask := make([]bool, len(data))
	if !l.key.variable && !l.value.variable {
		return mask
	}
	root := int(binary.LittleEndian.Uint64(data[32:]))
	if root == 0 || root&1 != 0 {
		return mask
	}
	in, lf := &l.internal, &l.leaf
	written := func(b int) bool {
		return b < 8 || b >= in.inline && b < in.inline+in.capacity*in.width ||
			b >= in.trailing && b < in.trailing+(in.capacity+1)*8
	}
	var slots []int
	for j := 0; j <= lf.capacity; j++ {
		if l.key.variable && j < lf.capacity {
			slots = append(slots, lf.keys+j*lf.keyStride)
		}
		if l.value.variable {
			slots = append(slots, lf.trailing+j*lf.stride)
		}
	}
	if l.key.variable && lf.headKey >= 0 {
		slots = append(slots, lf.headKey)
	}
	for page, size := root, l.rootSize; ; size = in.size {
		for _, s := range slots {
			for b := s; b < s+8 && b < size; b++ {
				if !written(b) {
					mask[page+b] = true
				}
			}
		}
		edge := int(binary.NativeEndian.Uint64(data[page+in.trailing:]))
		if edge&1 != 0 {
			return mask
		}
		page += edge
	}
}

// describeOffset names what lies at off in a file map Go wrote: which page,
// at which depth, and which of its fields.
func describeOffset(l *diskLayout, data []byte, off int) string {
	if off < fileHeaderSize {
		return fmt.Sprintf("header byte %d", off)
	}
	root := int(binary.LittleEndian.Uint64(data[32:]))
	level := []int{root}
	for depth := 0; len(level) > 0; depth++ {
		var next []int
		for index, tagged := range level {
			page, leaf := tagged&^1, tagged&1 != 0
			nl, size := &l.internal, l.internal.size
			if leaf {
				nl, size = &l.leaf, l.leaf.size
			}
			if depth == 0 {
				size = l.rootSize
			}
			if off >= page && off < page+size {
				return fmt.Sprintf("depth %d page %d (leaf %v) byte %d: %s", depth, index, leaf, off-page, describeField(l, nl, leaf, depth == 0, off-page))
			}
			if leaf {
				continue
			}
			n := int(binary.NativeEndian.Uint16(data[page:]))
			for e := 0; e <= n; e++ {
				next = append(next, page+int(binary.NativeEndian.Uint64(data[page+nl.trailing+e*8:])))
			}
		}
		if len(next) == 0 {
			for index, tagged := range level {
				if page := tagged &^ 1; off >= page && (index+1 == len(level) || off < level[index+1]&^1) {
					return fmt.Sprintf("heap pages of leaf %d, byte %d", index, off-page)
				}
			}
		}
		level = next
	}
	return "beyond the pages"
}

func describeField(l *diskLayout, nl *diskNodeLayout, leaf, root bool, b int) string {
	switch {
	case b < 8:
		return "node header"
	case root && b >= l.area:
		return "root area"
	case nl.width > 0 && b >= nl.inline && b < nl.inline+nl.capacity*nl.width:
		return fmt.Sprintf("inline key %d", (b-nl.inline)/nl.width)
	case leaf && nl.tags >= 0 && b >= nl.tags && b < nl.tags+nl.capacity:
		return fmt.Sprintf("hash tag %d", b-nl.tags)
	case !leaf && b >= nl.trailing && b < nl.trailing+(nl.capacity+1)*8:
		return fmt.Sprintf("edge %d", (b-nl.trailing)/8)
	case leaf && nl.headKey >= 0 && b >= nl.headKey && b < nl.headKey+nl.keyStride:
		if b-nl.headKey < l.key.size {
			return "head key"
		}
		return "head value"
	case leaf && nl.width > 0 && b >= nl.keys:
		j, at := (b-nl.keys)/nl.keyStride, (b-nl.keys)%nl.keyStride
		if at < l.key.size {
			return fmt.Sprintf("key %d", j)
		}
		return fmt.Sprintf("value %d", j)
	case leaf && b >= nl.trailing:
		if j := (b - nl.trailing) / nl.stride; j > 0 {
			return fmt.Sprintf("value %d", j-1)
		}
		return "head value"
	case b >= nl.keys:
		return fmt.Sprintf("key %d", (b-nl.keys)/max(nl.keyStride, 1))
	}
	return "padding"
}

// checkFileBytes compares the file map Go writes for keys and values with
// the one the oracle writes, outside upstreamPointerMask, inside which Go
// writes zeros and every difference between two upstream runs falls.
func checkFileBytes[K cmp.Ordered, V any](t *testing.T, oracle, kname, vname string, keys []K, values []V) {
	t.Helper()
	w, err := NewFileWriter(func(yield func(K, V) bool) {
		for i := range keys {
			if !yield(keys[i], values[i]) {
				return
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := w.Bytes()
	if len(got) != w.ByteLen() {
		t.Fatalf("Bytes gives %d bytes, ByteLen says %d", len(got), w.ByteLen())
	}
	want := rustFileBytes(t, oracle, kname, vname, keys, values)
	l := &w.layout
	if len(got) != len(want) {
		t.Fatalf("%s->%s, %d entries: Go wrote %d bytes, Rust %d", kname, vname, len(keys), len(got), len(want))
	}
	mask := upstreamPointerMask(l, got)
	if l.key.variable || l.value.variable {
		again := rustFileBytes(t, oracle, kname, vname, keys, values)
		for i := range want {
			if want[i] != again[i] && !mask[i] {
				t.Fatalf("two upstream runs differ outside the mask at %d: %s", i, describeOffset(l, got, i))
			}
		}
	}
	for i := range got {
		if mask[i] {
			if got[i] != 0 {
				t.Fatalf("Go wrote %#x where upstream leaves a pointer, at %d: %s", got[i], i, describeOffset(l, got, i))
			}
			continue
		}
		if got[i] != want[i] {
			t.Fatalf("%s->%s, %d entries: first difference at %d, Go %#x, Rust %#x: %s",
				kname, vname, len(keys), i, got[i], want[i], describeOffset(l, got, i))
		}
	}
	checkFileReads(t, got, keys, values)
}

// checkFileReads opens a file map and reads every entry back: each key's
// Get, and the iteration, in order.
func checkFileReads[K cmp.Ordered, V any](t *testing.T, data []byte, keys []K, values []V) {
	t.Helper()
	m, err := OpenFileMap[K, V](data)
	if err != nil {
		t.Fatalf("file map refused: %v", err)
	}
	defer m.Close()
	equal := func(a, b V) bool {
		if x, ok := any(a).([]byte); ok {
			return bytes.Equal(x, any(b).([]byte))
		}
		return reflect.DeepEqual(a, b)
	}
	for i, k := range keys {
		if v, ok := m.Get(k); !ok || !equal(v, values[i]) {
			t.Fatalf("Get of key %d, %v: %v %v", i, k, v, ok)
		}
	}
	i := 0
	for k, v := range m.All() {
		if i >= len(keys) || k != keys[i] || !equal(v, values[i]) {
			t.Fatalf("iteration's entry %d: %v", i, k)
		}
		i++
	}
	if i != len(keys) || m.Len() != len(keys) {
		t.Fatalf("iteration of %d entries, Len %d, of %d", i, m.Len(), len(keys))
	}
}

type fileTestInt interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

// intKeys is n distinct ascending keys of type K: dense, spread, or random;
// nil if K has fewer than n.
func intKeys[K fileTestInt](n int, mode string, rng *rand.Rand) []K {
	signed := ^K(0) < 0
	bits := int(unsafe.Sizeof(K(0))) * 8
	if bits < 64 && n > 1<<bits {
		return nil
	}
	keys := make([]K, 0, n)
	switch mode {
	case "dense":
		for i := range n {
			if signed {
				keys = append(keys, K(int64(i)-int64(n/2)))
			} else {
				keys = append(keys, K(uint64(i)))
			}
		}
	case "spread":
		// From the top bit up, by 7 where they fit: the oracle's prefix keys.
		base, step := uint64(1)<<(bits-1), uint64(7)
		if uint64(n)*step >= base {
			step = 1
		}
		for i := range n {
			keys = append(keys, K(base+uint64(i)*step))
		}
	default:
		for len(keys) < n {
			for len(keys) < n {
				keys = append(keys, K(rng.Uint64()))
			}
			slices.Sort(keys)
			keys = slices.Compact(keys)
		}
	}
	slices.Sort(keys)
	if keys = slices.Compact(keys); len(keys) != n {
		return nil
	}
	return keys
}

// stringKeys is n distinct ascending string keys of a shape that exercises
// shared prefixes: hex counters; the oracle's text keys, the empty string
// first; keys sharing 70 bytes; random keys of a few letters and zeros.
func stringKeys(n int, mode string, rng *rand.Rand) []string {
	keys := make([]string, 0, n)
	switch mode {
	case "hex":
		for i := range n {
			keys = append(keys, fmt.Sprintf("%016x", uint64(i)*7))
		}
	case "text":
		for i := range n {
			if i == 0 {
				keys = append(keys, "")
			} else {
				keys = append(keys, fmt.Sprintf("shared/prefix/%016x/é\x00", i))
			}
		}
	case "long":
		for i := range n {
			keys = append(keys, strings.Repeat("p", 70)+fmt.Sprintf("%08d", i))
		}
	case "zeros":
		// Keys equal but for trailing zeros, whose zero-padded encodings,
		// and so fences, share as much as a prefix records (65535 bytes).
		for i := range n {
			keys = append(keys, "k"+strings.Repeat("\x00", i))
		}
	default:
		const letters = "\x00\x00ab\x01z"
		for len(keys) < n {
			for len(keys) < n {
				b := make([]byte, rng.IntN(12))
				for j := range b {
					b[j] = letters[rng.IntN(len(letters))]
				}
				keys = append(keys, "pre"+string(b))
			}
			slices.Sort(keys)
			keys = slices.Compact(keys)
		}
		keys = keys[:n]
	}
	slices.Sort(keys)
	return keys
}

// byteValues is n values of lengths that exercise heap pages: empty, short,
// a whole page, more than a page.
func byteValues(n int, rng *rand.Rand) [][]byte {
	sizes := []int{0, 1, 7, 33, 1023, 1024, 1025, 1500, 4000}
	values := make([][]byte, n)
	for i := range values {
		size := sizes[rng.IntN(len(sizes))]
		if rng.IntN(4) != 0 {
			size = rng.IntN(48)
		}
		values[i] = make([]byte, size)
		for j := range values[i] {
			values[i][j] = byte(i + j)
		}
	}
	return values
}

func valuesOf[V any](n int, f func(int) V) []V {
	values := make([]V, n)
	for i := range values {
		values[i] = f(i)
	}
	return values
}

var fileBytesSizes = []int{0, 1, 2, 56, 57, 58, 2800, 2801, 5000, 140055, 150001}

func TestUpstreamFileBytesOracle(t *testing.T) {
	oracle := fileOracle(t)
	rng := rand.New(rand.NewPCG(1, 2))
	for _, n := range fileBytesSizes {
		for _, mode := range []string{"dense", "spread", "random"} {
			name := fmt.Sprintf("%d/%s", n, mode)
			intCase(t, oracle, "u64", "u64", name, intKeys[uint64](n, mode, rng), func(i int) uint64 { return uint64(i) * 0x9e3779b97f4a7c15 })
			intCase(t, oracle, "i64", "i64", name, intKeys[int64](n, mode, rng), func(i int) int64 { return -int64(i) })
			intCase(t, oracle, "u32", "u32", name, intKeys[uint32](n, mode, rng), func(i int) uint32 { return uint32(i) ^ 0xa5a5a5a5 })
			intCase(t, oracle, "i32", "b13", name, intKeys[int32](n, mode, rng), func(i int) [13]byte { return [13]byte{byte(i), 1, 2, 12: byte(i >> 8)} })
			intCase(t, oracle, "u16", "u8", name, intKeys[uint16](n, mode, rng), func(i int) uint8 { return uint8(i) })
			intCase(t, oracle, "i16", "f32", name, intKeys[int16](n, mode, rng), func(i int) float32 { return float32(i) / 3 })
			intCase(t, oracle, "u8", "i8", name, intKeys[uint8](n, mode, rng), func(i int) int8 { return int8(i) })
			intCase(t, oracle, "i8", "u64", name, intKeys[int8](n, mode, rng), func(i int) uint64 { return uint64(i) })
			intCase(t, oracle, "usize", "u64", name, intKeys[uint](n, mode, rng), func(i int) uint64 { return uint64(i) << 3 })
			intCase(t, oracle, "isize", "i64", name, intKeys[int](n, mode, rng), func(i int) int64 { return int64(i) })
			intCase(t, oracle, "u64", "b0", name, intKeys[uint64](n, mode, rng), func(int) struct{} { return struct{}{} })
			intCase(t, oracle, "u64", "f64", name, intKeys[uint64](n, mode, rng), func(i int) float64 { return float64(i) * 1.5 })
			intCase(t, oracle, "i64", "b16", name, intKeys[int64](n, mode, rng), func(i int) [16]byte { return [16]byte{byte(i), 15: 9} })
			if n <= 5000 || mode == "dense" {
				values := byteValues(n, rng)
				intCase(t, oracle, "u64", "bytes", name, intKeys[uint64](n, mode, rng), func(i int) []byte { return values[i] })
				// Go's string values are Rust's Vec<u8>, of the same layout.
				intCase(t, oracle, "u64", "bytes", name+"/string", intKeys[uint64](n, mode, rng), func(i int) string { return string(values[i]) })
				intCase(t, oracle, "u32", "bytes", name, intKeys[uint32](n, mode, rng), func(i int) []byte { return values[i] })
				intCase(t, oracle, "isize", "bytes", name, intKeys[int](n, mode, rng), func(i int) string { return string(values[i]) })
			}
		}
		for _, mode := range []string{"hex", "text", "long", "random"} {
			name := fmt.Sprintf("%d/%s", n, mode)
			keys := stringKeys(n, mode, rng)
			stringCase(t, oracle, "bytes", "u64", name, keys, func(i int) uint64 { return uint64(i) })
			stringCase(t, oracle, "bytes", "b0", name, keys, func(int) struct{} { return struct{}{} })
			stringCase(t, oracle, "bytes", "w3", name, keys, func(i int) [3]uint64 { return [3]uint64{uint64(i), ^uint64(i), 7} })
			stringCase(t, oracle, "bytes", "bytes", name, keys, func(i int) []byte { return []byte(keys[i] + "!") })
			if n <= 5000 {
				values := byteValues(n, rng)
				stringCase(t, oracle, "bytes", "bytes", name+"/heap", keys, func(i int) []byte { return values[i] })
			}
		}
	}
	// Two leaf levels of keys equal but for trailing zeros.
	for _, n := range []int{100, 2000} {
		keys := stringKeys(n, "zeros", rng)
		stringCase(t, oracle, "bytes", "u64", fmt.Sprintf("%d/zeros", n), keys, func(i int) uint64 { return uint64(i) })
		stringCase(t, oracle, "bytes", "bytes", fmt.Sprintf("%d/zeros", n), keys, func(i int) []byte { return []byte{byte(i)} })
	}
	// The oracle's text map, of 4000 entries, and one of 4000-byte values.
	text := stringKeys(4000, "text", rng)
	stringCase(t, oracle, "bytes", "bytes", "text4000", text, func(i int) []byte {
		return append([]byte(text[i]), 0, 255, byte(i%256))
	})
	stringCase(t, oracle, "bytes", "bytes", "text4000/4000-byte values", text, func(i int) []byte {
		return bytes.Repeat([]byte{byte(i)}, 4000)
	})
}

func intCase[K fileTestInt, V any](t *testing.T, oracle, kname, vname, name string, keys []K, value func(int) V) {
	t.Helper()
	if keys == nil {
		return
	}
	t.Run(kname+"-"+vname+"/"+name, func(t *testing.T) {
		checkFileBytes(t, oracle, kname, vname, keys, valuesOf(len(keys), value))
	})
}

func stringCase[V any](t *testing.T, oracle, kname, vname, name string, keys []string, value func(int) V) {
	t.Helper()
	t.Run(kname+"-"+vname+"/"+name, func(t *testing.T) {
		checkFileBytes(t, oracle, kname, vname, keys, valuesOf(len(keys), value))
	})
}

// TestUpstreamRandomFileBytesOracle checks maps of random sizes, keys and
// values, from a seed it logs, or GOBBTREE_FILE_SEED's to repeat a run.
func TestUpstreamRandomFileBytesOracle(t *testing.T) {
	oracle := fileOracle(t)
	seed := rand.Uint64()
	if s := os.Getenv("GOBBTREE_FILE_SEED"); s != "" {
		var err error
		if seed, err = strconv.ParseUint(s, 10, 64); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewPCG(seed, 3))
	for round := range 60 {
		n := rng.IntN(6000)
		if rng.IntN(8) == 0 {
			n = rng.IntN(60000)
		}
		name := fmt.Sprintf("%d/%d", round, n)
		switch rng.IntN(5) {
		case 0:
			intCase(t, oracle, "u64", "u64", name, intKeys[uint64](n, "random", rng), func(i int) uint64 { return uint64(i) })
		case 1:
			intCase(t, oracle, "i64", "i64", name, intKeys[int64](n, []string{"dense", "spread", "random"}[rng.IntN(3)], rng), func(i int) int64 { return int64(i) })
		case 2:
			intCase(t, oracle, "u32", "u32", name, intKeys[uint32](n, "random", rng), func(i int) uint32 { return uint32(i) })
		case 3:
			keys := stringKeys(n, []string{"hex", "text", "long", "random"}[rng.IntN(4)], rng)
			values := byteValues(n, rng)
			stringCase(t, oracle, "bytes", "bytes", name, keys, func(i int) []byte { return values[i] })
		default:
			keys := stringKeys(n, "random", rng)
			stringCase(t, oracle, "bytes", "u64", name, keys, func(i int) uint64 { return uint64(i) })
		}
	}
}
