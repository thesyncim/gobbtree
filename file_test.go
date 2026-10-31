package gobbtree

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fileEntries(n int) iter.Seq2[uint64, uint64] {
	return func(yield func(uint64, uint64) bool) {
		for i := 0; i < n; i++ {
			if !yield(uint64(i)*3, uint64(i)^0x5555555555555555) {
				return
			}
		}
	}
}

func checkFileEntries(t *testing.T, m *FileMap[uint64, uint64], n int) {
	t.Helper()
	if m.Len() != n || m.IsEmpty() != (n == 0) {
		t.Fatalf("length: %d want %d", m.Len(), n)
	}
	step := max(1, n/1000)
	for i := 0; i < n; i += step {
		k := uint64(i) * 3
		want := uint64(i) ^ 0x5555555555555555
		if v, ok := m.Get(k); !ok || v != want {
			t.Fatalf("Get(%d)=(%d,%v), want %d", k, v, ok, want)
		}
		if _, ok := m.Get(k + 1); ok {
			t.Fatalf("unexpected hit %d", k+1)
		}
	}
	for _, key := range []uint64{uint64(n) * 3, math.MaxUint64} {
		if _, ok := m.Get(key); ok {
			t.Fatalf("unexpected hit %d", key)
		}
	}
	i := 0
	for k, v := range m.All() {
		if k != uint64(i)*3 || v != uint64(i)^0x5555555555555555 {
			t.Fatalf("forward entry %d: %d %d", i, k, v)
		}
		i++
	}
	if i != n {
		t.Fatalf("forward count %d", i)
	}
	i = n - 1
	for k, v := range m.Reverse() {
		if k != uint64(i)*3 || v != uint64(i)^0x5555555555555555 {
			t.Fatalf("reverse entry %d: %d %d", i, k, v)
		}
		i--
	}
	if i != -1 {
		t.Fatalf("reverse count %d", i)
	}
	for _, bounds := range [][2]uint64{{0, 0}, {0, 1}, {1, 170}, {160, 190}, {1, math.MaxUint64}} {
		var forward, reverse []uint64
		visitRange(m.Range(bounds[0], bounds[1]), func(k, v uint64) bool { forward = append(forward, k); return true })
		visitRange(m.RangeReverse(bounds[0], bounds[1]), func(k, v uint64) bool { reverse = append(reverse, k); return true })
		slices.Reverse(reverse)
		var want []uint64
		for j := 0; j < n; j++ {
			k := uint64(j) * 3
			if k >= bounds[0] && k < bounds[1] {
				want = append(want, k)
			}
		}
		if !slices.Equal(forward, want) || !slices.Equal(reverse, want) {
			t.Fatalf("range %v: forward %d reverse %d want %d", bounds, len(forward), len(reverse), len(want))
		}
	}
}

func TestFileMapPages(t *testing.T) {
	for _, n := range []int{0, 1, 54, 55, 56, 57, 110, 111, 112, 2799, 2800, 2801, 150001} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			w, err := NewFileWriter(fileEntries(n))
			if err != nil {
				t.Fatal(err)
			}
			data := w.Bytes()
			if len(data) != w.ByteLen() {
				t.Fatal("byte length")
			}
			m, err := OpenFileMap[uint64, uint64](data)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			checkFileEntries(t, m, n)
			clear(data)
			checkFileEntries(t, m, n) // OpenFileMap owns its bytes.
		})
	}
}

func TestFileMapMmapAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "map.bbt")
	if err := CreateFile(path, fileEntries(3001)); err != nil {
		t.Fatal(err)
	}
	m, err := Mmap[uint64, uint64](path).Open()
	if errors.Is(err, ErrMmapUnavailable) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	checkFileEntries(t, m, 3001)
	if err := CreateFile(path, fileEntries(3)); err != nil {
		t.Fatal(err)
	}
	checkFileEntries(t, m, 3001)
	r, err := ReadFile[uint64, uint64](path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	checkFileEntries(t, r, 3)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				m.Get(6)
			}
		}()
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(6); ok {
		t.Fatal("closed hit")
	}
	if m.Len() != 0 {
		t.Fatal("closed length")
	}
}

func TestFileMapRangesAndCloseInIterator(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "embedded"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	prefix := bytes.Repeat([]byte{0xaa}, 127)
	if _, err = f.Write(prefix); err != nil {
		t.Fatal(err)
	}
	w, err := NewFileWriter(fileEntries(1000))
	if err != nil {
		t.Fatal(err)
	}
	// Stale bytes past the offset must not survive as alignment padding.
	if _, err = f.Write(bytes.Repeat([]byte{0xbb}, 3000)); err != nil {
		t.Fatal(err)
	}
	r, err := w.WriteAt(f, 127)
	if err != nil {
		t.Fatal(err)
	}
	padding := make([]byte, r.Offset-127)
	if _, err = f.ReadAt(padding, 127); err != nil || bytes.Count(padding, []byte{0}) != len(padding) {
		t.Fatal("alignment padding not zeroed", err)
	}
	if info, err := f.Stat(); err != nil || info.Size() != r.Offset+r.Length {
		t.Fatal("file not ended after the map", err)
	}
	if r.Offset%int64(w.Alignment()) != 0 {
		t.Fatal("alignment")
	}
	got := make([]byte, 127)
	if _, err = f.ReadAt(got, 0); err != nil || !bytes.Equal(got, prefix) {
		t.Fatal("prefix changed", err)
	}
	for _, mmap := range []bool{false, true} {
		var m *FileMap[uint64, uint64]
		if mmap {
			m, err = Mmap[uint64, uint64](f.Name()).Range(r).Pages(OsPagesHuge).Open()
			if errors.Is(err, ErrMmapUnavailable) {
				continue
			}
		} else {
			m, err = ReadFileRange[uint64, uint64](f, r)
		}
		if err != nil {
			t.Fatal(err)
		}
		checkFileEntries(t, m, 1000)
		count := 0
		for range m.All() {
			count++
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if count != 1 {
			t.Fatalf("close in iterator: %d", count)
		}
	}
	if _, err = ReadFileRange[uint64, uint64](f, FileRange{math.MaxInt64, 100}); err == nil {
		t.Fatal("invalid range")
	}
}

func TestFileMapBatchAndWriterErrors(t *testing.T) {
	w, err := NewFileWriter(fileEntries(100))
	if err != nil {
		t.Fatal(err)
	}
	m, err := OpenFileMap[uint64, uint64](w.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	keys := []uint64{0, 3, 4, 3, math.MaxUint64, 297}
	got := make([]Result[uint64], len(keys))
	m.GetMany(keys, got)
	for i, k := range keys {
		v, ok := m.Get(k)
		if got[i] != (Result[uint64]{v, ok}) {
			t.Fatalf("batch %d", i)
		}
	}
	path := filepath.Join(t.TempDir(), "existing")
	if err = os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, keys := range [][]uint64{{1, 1}, {2, 1}, {0, 5, 9, 9}} {
		err = CreateFile(path, func(yield func(uint64, uint64) bool) {
			for _, k := range keys {
				if !yield(k, k) {
					return
				}
			}
		})
		var notAscending *NotAscendingError
		if !errors.Is(err, ErrNotAscending) || !errors.As(err, &notAscending) || notAscending.Index() != len(keys)-1 {
			t.Fatalf("ascending: %v", err)
		}
		b, _ := os.ReadFile(path)
		if string(b) != "keep" {
			t.Fatal("invalid input changed destination")
		}
	}
	if _, err = NewFileWriter(func(yield func(uint64, *int) bool) {}); !errors.Is(err, ErrFileType) {
		t.Fatal(err)
	}
	if _, err = OpenFileMap[uint32, uint64](w.Bytes()); !errors.Is(err, ErrFileLayout) {
		t.Fatal(err)
	}
}

func TestFileMapCorruption(t *testing.T) {
	w, _ := NewFileWriter(fileEntries(3000))
	valid := w.Bytes()
	for _, n := range []int{0, 7, 63, 64, 1024, len(valid) - 1} {
		if _, err := OpenFileMap[uint64, uint64](valid[:n]); err == nil {
			t.Fatalf("accepted truncation %d", n)
		}
	}
	l := w.layout
	root := int(binary.LittleEndian.Uint64(valid[32:])) &^ 1
	for name, mutate := range map[string]func([]byte){
		"magic":       func(b []byte) { b[0] ^= 1 },
		"version":     func(b []byte) { b[8] = 2 },
		"count":       func(b []byte) { binary.LittleEndian.PutUint64(b[24:], math.MaxUint64) },
		"root":        func(b []byte) { binary.LittleEndian.PutUint64(b[32:], math.MaxUint64) },
		"zero_length": func(b []byte) { binary.LittleEndian.PutUint64(b[40:], 0) },
		"prefix":      func(b []byte) { b[root+l.area] = 65 },
		"capacity":    func(b []byte) { binary.NativeEndian.PutUint16(b[root:], 65535) },
		"head":        func(b []byte) { b[root+2] = 2 },
		"back_edge":   func(b []byte) { binary.NativeEndian.PutUint64(b[root+l.internal.trailing:], 0) },
		"huge_edge":   func(b []byte) { binary.NativeEndian.PutUint64(b[root+l.internal.trailing:], math.MaxUint64) },
		"duplicate_edge": func(b []byte) {
			copy(b[root+l.internal.trailing+8:], b[root+l.internal.trailing:root+l.internal.trailing+8])
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := bytes.Clone(valid)
			mutate(b)
			if m, err := OpenFileMap[uint64, uint64](b); err == nil {
				m.Close()
				t.Fatal("accepted corrupt map")
			}
		})
	}
}

func TestUpstreamFileOracle(t *testing.T) {
	oracle := os.Getenv("GOBBTREE_FILE_ORACLE")
	if oracle == "" {
		t.Skip("set GOBBTREE_FILE_ORACLE to the pinned Rust file-oracle binary")
	}
	run := func(t *testing.T, args ...string) []byte {
		t.Helper()
		out, err := exec.Command(oracle, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("Rust %v: %v\n%s", args, err, out)
		}
		return out
	}
	for _, n := range []int{1, 25, 3000} {
		t.Run(fmt.Sprintf("tagged/%d", n), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rust.bbt")
			run(t, "write-tagged", path, fmt.Sprint(n))
			m, err := ReadFile[string, [3]uint64](path)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if m.layout.leaf.tags < 0 {
				t.Fatal("fixture does not exercise tags")
			}
			var dump strings.Builder
			fmt.Fprintf(&dump, "len %d\n", m.Len())
			for k, v := range m.All() {
				fmt.Fprintf(&dump, "%s %d %d %d\n", k, v[0], v[1], v[2])
				if actual, ok := m.Get(k); !ok || actual != v {
					t.Fatal("tagged get", k)
				}
			}
			if dump.String() != string(run(t, "dump-tagged", path)) {
				t.Fatal("tagged iteration")
			}
			goPath := filepath.Join(t.TempDir(), "go.bbt")
			if err = CreateFile(goPath, m.All()); err != nil {
				t.Fatal(err)
			}
			if dump.String() != string(run(t, "dump-tagged", goPath)) {
				t.Fatal("Rust reading Go tagged file")
			}
		})
	}
	for _, n := range []int{0, 1, 55, 56, 57, 2800, 5000} {
		for _, mode := range []string{"dense", "prefix", "mixed"} {
			t.Run(fmt.Sprintf("%d/%s", n, mode), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "rust.bbt")
				run(t, "write", path, fmt.Sprint(n), mode)
				m, err := Mmap[uint64, uint64](path).Open()
				if errors.Is(err, ErrMmapUnavailable) {
					m, err = ReadFile[uint64, uint64](path)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer m.Close()
				var dump strings.Builder
				fmt.Fprintf(&dump, "len %d\n", m.Len())
				for k, v := range m.All() {
					fmt.Fprintf(&dump, "%016x %016x\n", k, v)
					if actual, ok := m.Get(k); !ok || actual != v {
						t.Fatalf("Rust file lookup %x", k)
					}
				}
				if want := string(run(t, "dump", path)); dump.String() != want {
					t.Fatal("Rust file iteration differs")
				}
				goPath := filepath.Join(t.TempDir(), "go.bbt")
				if err = CreateFile(goPath, m.All()); err != nil {
					t.Fatal(err)
				}
				if actual := string(run(t, "dump", goPath)); actual != dump.String() {
					t.Fatal("Rust reading Go file differs")
				}
			})
		}
	}
	for _, n := range []int{0, 1, 42, 43, 4000} {
		t.Run(fmt.Sprintf("text/%d", n), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rust.bbt")
			run(t, "write-text", path, fmt.Sprint(n))
			m, err := Mmap[string, []byte](path).Open()
			if errors.Is(err, ErrMmapUnavailable) {
				m, err = ReadFile[string, []byte](path)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			var dump strings.Builder
			fmt.Fprintf(&dump, "len %d\n", m.Len())
			for k, v := range m.All() {
				fmt.Fprintf(&dump, "%x %x\n", k, v)
				actual, ok := m.Get(k)
				if !ok || !bytes.Equal(actual, v) {
					t.Fatal("text get", k)
				}
			}
			if dump.String() != string(run(t, "dump-text", path)) {
				t.Fatal("text iteration differs")
			}
			goPath := filepath.Join(t.TempDir(), "go.bbt")
			if err = CreateFile(goPath, m.All()); err != nil {
				t.Fatal(err)
			}
			if dump.String() != string(run(t, "dump-text", goPath)) {
				t.Fatal("Rust reading Go text differs")
			}
		})
	}
	t.Run("plain", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rust.bbt")
		run(t, "write-small", path, "5000")
		m, err := ReadFile[int32, [13]byte](path)
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		var dump strings.Builder
		fmt.Fprintf(&dump, "len %d\n", m.Len())
		for k, v := range m.All() {
			fmt.Fprintf(&dump, "%d %x\n", k, v)
			if actual, ok := m.Get(k); !ok || actual != v {
				t.Fatalf("plain get %d", k)
			}
		}
		if dump.String() != string(run(t, "dump-small", path)) {
			t.Fatal("plain iteration differs")
		}
		goPath := filepath.Join(t.TempDir(), "go.bbt")
		if err = CreateFile(goPath, m.All()); err != nil {
			t.Fatal(err)
		}
		if dump.String() != string(run(t, "dump-small", goPath)) {
			t.Fatal("Rust reading Go plain differs")
		}
	})
}

func FuzzFileMap(f *testing.F) {
	for _, n := range []int{0, 1, 56, 3000} {
		w, _ := NewFileWriter(fileEntries(n))
		f.Add(w.Bytes())
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		m, err := OpenFileMap[uint64, uint64](data)
		if err != nil {
			return
		}
		defer m.Close()
		m.Get(0)
		m.Get(math.MaxUint64)
		count := 0
		for range m.All() {
			count++
			if count > m.Len() {
				t.Fatal("unbounded iteration")
			}
		}
		if count != m.Len() {
			t.Fatal("iteration count")
		}
		for range m.Reverse() {
		}
	})
}

func TestFileInlineSearch(t *testing.T) {
	rng := rand.New(rand.NewPCG(51, 97))
	for _, width := range []int{2, 4, 8} {
		for n := 0; n <= 1023; n++ {
			values := make([]uint64, n)
			for i := range values {
				values[i] = rng.Uint64() >> (64 - width*8)
			}
			slices.Sort(values)
			data := make([]byte, n*width)
			for i, v := range values {
				switch width {
				case 2:
					binary.NativeEndian.PutUint16(data[i*width:], uint16(v))
				case 4:
					binary.NativeEndian.PutUint32(data[i*width:], uint32(v))
				case 8:
					binary.NativeEndian.PutUint64(data[i*width:], v)
				}
			}
			queries := []uint64{0, math.MaxUint64 >> (64 - width*8), rng.Uint64() >> (64 - width*8)}
			if n > 0 {
				queries = append(queries, values[n/2])
			}
			for _, q := range queries {
				a, b := diskInlineRange(data, width, q)
				x, y := diskInlineRangeScalar(data, width, q)
				if a != x || b != y {
					t.Fatalf("width %d n %d key %d: %d:%d want %d:%d", width, n, q, a, b, x, y)
				}
			}
		}
	}
}

func TestFileMapNumericReadAllocations(t *testing.T) {
	w, _ := NewFileWriter(fileEntries(4000))
	m, err := OpenFileMap[uint64, uint64](w.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if n := testing.AllocsPerRun(1000, func() {
		v, ok := m.Get(6000)
		if !ok || v != uint64(2000)^0x5555555555555555 {
			panic("lookup")
		}
	}); n != 0 {
		t.Fatalf("numeric Get allocated %g times", n)
	}
}

func TestFileMapOwnedResultsAndCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.bbt")
	if err := CreateFile(path, func(yield func(string, []byte) bool) { yield("key", []byte("value")) }); err != nil {
		t.Fatal(err)
	}
	m, err := Mmap[string, []byte](path).Open()
	if errors.Is(err, ErrMmapUnavailable) {
		m, err = ReadFile[string, []byte](path)
	}
	if err != nil {
		t.Fatal(err)
	}
	k, v, ok := m.GetKeyValue("key")
	if !ok {
		t.Fatal("lookup")
	}
	v[0] = 'X'
	original, _ := m.Get("key")
	if string(original) != "value" {
		t.Fatal("borrowed value escaped")
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	if k != "key" || string(v) != "Xalue" || string(original) != "value" {
		t.Fatal("results changed after close")
	}
	w, _ := NewFileWriter(fileEntries(1))
	var releases atomic.Int32
	mock, err := openFileMap[uint64, uint64](w.Bytes(), false, func() error { releases.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	mock.Close()
	mock.Close()
	// The mapping goes once the collector finds its image unreachable, once.
	waitReleases(t, &releases, 1)
}

// waitReleases collects until releases reaches want, and checks that it
// goes no further.
func waitReleases(t *testing.T, releases *atomic.Int32, want int32) {
	t.Helper()
	for i := 0; releases.Load() < want && i < 200; i++ {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	if got := releases.Load(); got != want {
		t.Fatalf("released %d times, want %d", got, want)
	}
}

// Reads take no lock: a read in progress, as an iteration is, holds the
// mapping it reads through Close until it ends.
func TestFileMapCloseWaitsForReads(t *testing.T) {
	w, _ := NewFileWriter(fileEntries(5000))
	var releases atomic.Int32
	m, err := openFileMap[uint64, uint64](w.Bytes(), false, func() error { releases.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	next, stop := iter.Pull2(m.All())
	first, _, ok := next()
	if !ok {
		t.Fatal("empty iteration")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(first); ok || m.Len() != 0 {
		t.Fatal("a closed map still answers")
	}
	waitReleases(t, &releases, 0)
	// The iteration began before Close, and ends at its next entry.
	if _, _, ok := next(); ok {
		t.Fatal("iteration went on past Close")
	}
	stop()
	waitReleases(t, &releases, 1)
}

func TestFileMapStringReadAllocations(t *testing.T) {
	w, err := NewFileWriter(func(yield func(string, uint64) bool) {
		for i := 0; i < 1000; i++ {
			if !yield(fmt.Sprintf("prefix/%08d", i), uint64(i)) {
				return
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := OpenFileMap[string, uint64](w.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if n := testing.AllocsPerRun(1000, func() {
		v, ok := m.Get("prefix/00000400")
		if !ok || v != 400 {
			panic("lookup")
		}
	}); n != 0 {
		t.Fatalf("string lookup allocated %g times", n)
	}
}

func checkDiskTypes[K cmp.Ordered, V any](t *testing.T, entries []Pair[K, V]) {
	t.Helper()
	w, err := NewFileWriter(func(yield func(K, V) bool) {
		for _, e := range entries {
			if !yield(e.Key, e.Value) {
				return
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := OpenFileMap[K, V](w.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	i := 0
	for k, v := range m.All() {
		if cmp.Compare(k, entries[i].Key) != 0 || !reflect.DeepEqual(v, entries[i].Value) {
			t.Fatalf("entry %d: %v %v", i, k, v)
		}
		i++
	}
	if i != len(entries) {
		t.Fatal("length")
	}
	i--
	for k, v := range m.Reverse() {
		if cmp.Compare(k, entries[i].Key) != 0 || !reflect.DeepEqual(v, entries[i].Value) {
			t.Fatalf("reverse %d", i)
		}
		i--
	}
	for _, e := range entries {
		v, ok := m.Get(e.Key)
		if !ok || !reflect.DeepEqual(v, e.Value) {
			t.Fatalf("Get(%v)", e.Key)
		}
	}
}

func TestFileMapTypes(t *testing.T) {
	var small []Pair[int8, int16]
	for i := -128; i <= 127; i++ {
		small = append(small, Pair[int8, int16]{int8(i), int16(i * 5)})
	}
	checkDiskTypes(t, small)
	var signed []Pair[int64, string]
	signed = append(signed, Pair[int64, string]{math.MinInt64, "min"})
	for i := -1000; i <= 1000; i++ {
		signed = append(signed, Pair[int64, string]{int64(i) * 10000000000, fmt.Sprint(i)})
	}
	signed = append(signed, Pair[int64, string]{math.MaxInt64, "max"})
	checkDiskTypes(t, signed)
	var units []Pair[uint64, struct{}]
	for i := 0; i < 1000; i++ {
		units = append(units, Pair[uint64, struct{}]{Key: uint64(i)})
	}
	checkDiskTypes(t, units)
	checkDiskTypes(t, []Pair[uint16, [3]float64]{{0, [3]float64{1, 2, 3}}, {math.MaxUint16, [3]float64{-1, math.Inf(1), math.Inf(-1)}}})
	checkDiskTypes(t, []Pair[string, []byte]{{"", []byte{255}}, {"\x00", []byte{0, 1}}, {"a", []byte{2}}, {"a\x00", []byte{3}}, {"é", []byte{4}}})
	type key uint64
	type value string
	checkDiskTypes(t, []Pair[key, value]{{1, "one"}, {math.MaxUint64, "last"}})
}

// Generated by the pinned 8adae97 Rust file-oracle, including Rust's own dumps.
// These fixtures run even when the optional Rust executable is unavailable.
func TestRustFileFixtures(t *testing.T) {
	check := func(name, actual string) {
		t.Helper()
		want, err := os.ReadFile("testdata/files/" + name + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		if actual != string(want) {
			t.Fatalf("Rust %s fixture differs", name)
		}
	}
	{
		m, err := ReadFile[uint64, uint64]("testdata/files/u64.bbt")
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		var b strings.Builder
		fmt.Fprintf(&b, "len %d\n", m.Len())
		for k, v := range m.All() {
			fmt.Fprintf(&b, "%016x %016x\n", k, v)
			if actual, ok := m.Get(k); !ok || actual != v {
				t.Fatal("u64 get")
			}
		}
		check("u64", b.String())
	}
	{
		m, err := ReadFile[string, []byte]("testdata/files/text.bbt")
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		var b strings.Builder
		fmt.Fprintf(&b, "len %d\n", m.Len())
		for k, v := range m.All() {
			fmt.Fprintf(&b, "%x %x\n", k, v)
			if actual, ok := m.Get(k); !ok || !bytes.Equal(actual, v) {
				t.Fatal("text get")
			}
		}
		check("text", b.String())
	}
	{
		m, err := ReadFile[int32, [13]byte]("testdata/files/plain.bbt")
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		var b strings.Builder
		fmt.Fprintf(&b, "len %d\n", m.Len())
		for k, v := range m.All() {
			fmt.Fprintf(&b, "%d %x\n", k, v)
			if actual, ok := m.Get(k); !ok || actual != v {
				t.Fatal("plain get")
			}
		}
		check("plain", b.String())
	}
}

func FuzzFileBytes(f *testing.F) {
	w, _ := NewFileWriter(func(yield func(string, []byte) bool) {
		for i := 0; i < 200; i++ {
			if !yield(fmt.Sprint(i+1000), bytes.Repeat([]byte{byte(i)}, i)) {
				return
			}
		}
	})
	f.Add(w.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		m, err := OpenFileMap[string, []byte](data)
		if err != nil {
			return
		}
		defer m.Close()
		m.Get("")
		m.Get("1100")
		m.Get("\xff\xff")
		count := 0
		for range m.All() {
			count++
			if count > m.Len() {
				t.Fatal("unbounded")
			}
		}
		if count != m.Len() {
			t.Fatal("count")
		}
		for range m.Reverse() {
		}
	})
}
