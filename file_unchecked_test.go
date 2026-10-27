package gobbtree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileMapUncheckedReadsHeaderOnly(t *testing.T) {
	w, err := NewFileWriter(fileEntries(3000))
	if err != nil {
		t.Fatal(err)
	}
	valid := w.Bytes()
	m, err := OpenFileMapUnchecked[uint64, uint64](valid)
	if err != nil {
		t.Fatal(err)
	}
	checkFileEntries(t, m, 3000)
	m.Close()

	// Header damage is refused either way.
	for _, n := range []int{0, 63, len(valid) - 1} {
		if _, err := OpenFileMapUnchecked[uint64, uint64](valid[:n]); err == nil {
			t.Fatalf("accepted truncation %d", n)
		}
	}
	if _, err := OpenFileMapUnchecked[uint32, uint64](valid); !errors.Is(err, ErrFileLayout) {
		t.Fatal(err)
	}
	bad := bytes.Clone(valid)
	binary.LittleEndian.PutUint64(bad[32:], math.MaxUint64)
	if _, err := OpenFileMapUnchecked[uint64, uint64](bad); err == nil {
		t.Fatal("accepted a root outside the map")
	}

	// Page damage is found by a checked open only.
	root := int(binary.LittleEndian.Uint64(valid[32:])) &^ 1
	bad = bytes.Clone(valid)
	binary.NativeEndian.PutUint64(bad[root+w.layout.internal.trailing:], 0)
	if _, err := OpenFileMap[uint64, uint64](bad); err == nil {
		t.Fatal("checked open accepted a back edge")
	}
	if m, err = OpenFileMapUnchecked[uint64, uint64](bad); err != nil {
		t.Fatal("unchecked open read past the header:", err)
	}
	m.Close()

	path := filepath.Join(t.TempDir(), "bad.bbt")
	if err = os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadFile[uint64, uint64](path); err == nil {
		t.Fatal("ReadFile accepted a back edge")
	}
	m, err = Mmap[uint64, uint64](path).Open()
	if errors.Is(err, ErrMmapUnavailable) {
		return
	}
	if err != nil {
		t.Fatal("Mmap read past the header:", err)
	}
	m.Close()
}

// A corrupt map opened unchecked may answer wrongly or panic, but must
// neither hang nor read out of bounds.
func TestFileMapUncheckedCorruptionPanicsOrAnswers(t *testing.T) {
	for _, keys := range []string{"uint", "string"} {
		var valid []byte
		if keys == "uint" {
			w, _ := NewFileWriter(fileEntries(5000))
			valid = w.Bytes()
		} else {
			w, _ := NewFileWriter(func(yield func(string, []byte) bool) {
				for i := range 3000 {
					if !yield(string(rune('a'+i/1000))+string(rune(i)), []byte{byte(i)}) {
						return
					}
				}
			})
			valid = w.Bytes()
		}
		rng := rand.New(rand.NewPCG(1, uint64(len(keys))))
		for trial := range 300 {
			b := bytes.Clone(valid)
			for range 1 + trial%8 {
				i := fileHeaderSize + rng.IntN(len(b)-fileHeaderSize)
				b[i] = byte(rng.Uint32())
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { _ = recover() }()
				if keys == "uint" {
					exerciseUnchecked[uint64, uint64](b, rng.Uint64N(6000))
				} else {
					exerciseUnchecked[string, []byte](b, "b")
				}
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s trial %d: corrupt map hung", keys, trial)
			}
		}
	}
}

func exerciseUnchecked[K uint64 | string, V any](b []byte, key K) {
	m, err := OpenFileMapUnchecked[K, V](b)
	if err != nil {
		return
	}
	defer m.Close()
	m.Get(key)
	m.First()
	m.Last()
	n := 0
	for range m.All() {
		if n++; n > 100000 {
			break
		}
	}
	n = 0
	for range m.Reverse() {
		if n++; n > 100000 {
			break
		}
	}
	n = 0
	for range m.Ascend(key) {
		if n++; n > 100000 {
			break
		}
	}
}
