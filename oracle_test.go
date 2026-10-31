package gobbtree

import (
	"bytes"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestUpstreamOracle requires the binary compiled from the exact revision in
// internal/oracle/Cargo.toml. Ordinary users need only Go; CI explicitly
// requires and executes this gate instead of counting a skip as parity.
type oracleMap[K any] interface {
	Insert(K, uint64) (uint64, bool)
	Delete(K) (uint64, bool)
	Get(K) (uint64, bool)
	Clear()
	Range(K, K) iter.Seq2[K, uint64]
	RangeReverse(K, K) iter.Seq2[K, uint64]
	Scan(Bounds[K]) iter.Seq2[K, uint64]
	ScanReverse(Bounds[K]) iter.Seq2[K, uint64]
	All() iter.Seq2[K, uint64]
}

// randomBounds draws bounds around key, inverted or equal-excluded at times
// so that both sides panic, and writes them in the oracle's notation.
func randomBounds(rng *rand.Rand, key uint64, input *bytes.Buffer, op string) Bounds[uint64] {
	kinds := []boundKind{unbounded, included, excluded}
	names := map[boundKind]string{unbounded: "u", included: "i", excluded: "e"}
	lo, hi := key, key+rng.Uint64N(80)
	if hi < lo {
		hi = math.MaxUint64
	}
	switch rng.IntN(8) {
	case 0:
		lo, hi = hi, lo
	case 1:
		hi = lo
	}
	b := Bounds[uint64]{lower: lo, upper: hi, lowerKind: kinds[rng.IntN(3)], upperKind: kinds[rng.IntN(3)]}
	fmt.Fprintf(input, "%s %s %d %s %d\n", op, names[b.lowerKind], lo, names[b.upperKind], hi)
	return b
}

// recordPanic writes what f writes, or "panic" if it panics.
func recordPanic(want *bytes.Buffer, f func(w *bytes.Buffer)) {
	var line bytes.Buffer
	defer func() {
		if recover() != nil {
			want.WriteString("panic\n")
			return
		}
		want.Write(line.Bytes())
		want.WriteString("\n")
	}()
	f(&line)
}

func TestUpstreamOracle(t *testing.T) {
	t.Run("Tree", func(t *testing.T) { runUpstreamOracle(t, NewTree[uint64, uint64]()) })
	t.Run("Map", func(t *testing.T) { runUpstreamOracle(t, New[uint64, uint64]()) })
}
func runUpstreamOracle(t *testing.T, tr oracleMap[uint64]) {
	binary := os.Getenv("GOBBTREE_ORACLE")
	if binary == "" {
		t.Skip("set GOBBTREE_ORACLE to the pinned Rust oracle binary")
	}
	var input, want bytes.Buffer
	result := func(v uint64, ok bool) {
		if ok {
			fmt.Fprintln(&want, v)
		} else {
			fmt.Fprintln(&want, "-")
		}
	}
	rng := rand.New(rand.NewPCG(77, 88))
	for i := 0; i < 22000; i++ {
		key := rng.Uint64N(5000)
		value := rng.Uint64()
		if i%197 == 0 {
			key = math.MaxUint64
		}
		if i%199 == 0 {
			key = 0
		}
		op := rng.IntN(12)
		if i < 6000 {
			op = 0
		}
		switch op {
		case 0, 1, 2, 3:
			fmt.Fprintf(&input, "I %d %d\n", key, value)
			result(tr.Insert(key, value))
		case 4, 5:
			fmt.Fprintf(&input, "D %d\n", key)
			result(tr.Delete(key))
		case 6, 7:
			fmt.Fprintf(&input, "G %d\n", key)
			result(tr.Get(key))
		case 8:
			end := key + 64
			if key > math.MaxUint64-64 {
				end = math.MaxUint64
			}
			fmt.Fprintf(&input, "R %d %d\n", key, end)
			visitRange(tr.Range(key, end), func(k, v uint64) bool { fmt.Fprintf(&want, "%d:%d,", k, v); return true })
			fmt.Fprintln(&want)
		case 9:
			end := key + 64
			if key > math.MaxUint64-64 {
				end = math.MaxUint64
			}
			fmt.Fprintf(&input, "V %d %d\n", key, end)
			visitRange(tr.RangeReverse(key, end), func(k, v uint64) bool { fmt.Fprintf(&want, "%d:%d,", k, v); return true })
			fmt.Fprintln(&want)
		case 10, 11:
			op := map[bool]string{false: "B", true: "W"}[op == 11]
			b := randomBounds(rng, key, &input, op)
			recordPanic(&want, func(w *bytes.Buffer) {
				seq := tr.Scan(b)
				if op == "W" {
					seq = tr.ScanReverse(b)
				}
				for k, v := range seq {
					fmt.Fprintf(w, "%d:%d,", k, v)
				}
			})
		}
		if i%3000 == 2999 {
			fmt.Fprintln(&input, "L")
			fmt.Fprintln(&want, seqLen(tr.All()))
		}
	}
	// Deletion to empty exercises merges and root collapse in both trees.
	keys := make([]uint64, 0, seqLen(tr.All()))
	for k := range tr.All() {
		keys = append(keys, k)
	}
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	for _, k := range keys {
		fmt.Fprintf(&input, "D %d\n", k)
		result(tr.Delete(k))
	}
	fmt.Fprintln(&input, "L\nC\nI 7 9\nG 7")
	fmt.Fprintln(&want, seqLen(tr.All()))
	tr.Clear()
	fmt.Fprintln(&want, "-")
	result(tr.Insert(7, 9))
	result(tr.Get(7))
	command := exec.Command(binary)
	command.Stdin = &input
	var stderr bytes.Buffer
	command.Stderr = &stderr
	got, err := command.Output()
	if err != nil {
		t.Fatalf("oracle failed: %v: %s", err, stderr.String())
	}
	if !bytes.Equal(got, want.Bytes()) {
		g, w := strings.Split(string(got), "\n"), strings.Split(want.String(), "\n")
		for i := 0; i < min(len(g), len(w)); i++ {
			if g[i] != w[i] {
				t.Fatalf("oracle mismatch line %d: got %q want %q", i+1, g[i], w[i])
			}
		}
		t.Fatalf("oracle output length: %d, expected %d", len(got), want.Len())
	}
	t.Log("matched every return value, forward/reverse range, length, reset, and deletion-to-empty against upstream 8ef597b")
}

func TestUpstreamOracleStrings(t *testing.T) {
	t.Run("Tree", func(t *testing.T) { runUpstreamOracleStrings(t, NewTree[string, uint64]()) })
	t.Run("Map", func(t *testing.T) { runUpstreamOracleStrings(t, New[string, uint64]()) })
}
func runUpstreamOracleStrings(t *testing.T, tr oracleMap[string]) {
	binary := os.Getenv("GOBBTREE_ORACLE")
	if binary == "" {
		t.Skip("set GOBBTREE_ORACLE to the pinned Rust oracle binary")
	}
	encode := func(s string) string {
		if s == "" {
			return "_"
		}
		return fmt.Sprintf("%x", s)
	}
	pool := []string{"", "\x00", "\x00\x00", "a", "a\x00", "a\x00\x00", "abcdefgh", "abcdefgh\x00", "abcdefgh\x00\x00", "\xff"}
	for i := 0; i < 1000; i++ {
		pool = append(pool, fmt.Sprintf("/tenant/long/shared/prefix/records/%08d/entry", i))
	}
	rng := rand.New(rand.NewPCG(111, 222))
	var input, want bytes.Buffer
	result := func(v uint64, ok bool) {
		if ok {
			fmt.Fprintln(&want, v)
		} else {
			fmt.Fprintln(&want, "-")
		}
	}
	for i := 0; i < 8000; i++ {
		key := pool[rng.IntN(len(pool))]
		value := rng.Uint64()
		op := rng.IntN(6)
		if i < 2000 {
			op = 0
		}
		switch op {
		case 0, 1:
			fmt.Fprintf(&input, "I %s %d\n", encode(key), value)
			result(tr.Insert(key, value))
		case 2:
			fmt.Fprintf(&input, "D %s\n", encode(key))
			result(tr.Delete(key))
		case 3:
			fmt.Fprintf(&input, "G %s\n", encode(key))
			result(tr.Get(key))
		case 4, 5:
			upper := key + "\xff"
			cmd := "R"
			if op == 5 {
				cmd = "V"
			}
			fmt.Fprintf(&input, "%s %s %s\n", cmd, encode(key), encode(upper))
			visit := func(k string, v uint64) bool { fmt.Fprintf(&want, "%s:%d,", encode(k), v); return true }
			if op == 4 {
				visitRange(tr.Range(key, upper), visit)
			} else {
				visitRange(tr.RangeReverse(key, upper), visit)
			}
			fmt.Fprintln(&want)
		}
	}
	var keys []string
	for k := range tr.All() {
		keys = append(keys, k)
	}
	for _, k := range keys {
		fmt.Fprintf(&input, "D %s\n", encode(k))
		result(tr.Delete(k))
	}
	command := exec.Command(binary, "strings")
	command.Stdin = &input
	var stderr bytes.Buffer
	command.Stderr = &stderr
	got, err := command.Output()
	if err != nil {
		t.Fatalf("oracle failed: %v: %s", err, stderr.String())
	}
	if !bytes.Equal(got, want.Bytes()) {
		g, w := strings.Split(string(got), "\n"), strings.Split(want.String(), "\n")
		for i := 0; i < min(len(g), len(w)); i++ {
			if g[i] != w[i] {
				t.Fatalf("string oracle mismatch line %d: got %q want %q", i+1, g[i], w[i])
			}
		}
		t.Fatal("string oracle output length mismatch")
	}
	t.Log("matched byte-string keys, embedded NULs, long prefixes, ranges, and deletion-to-empty")
}

// TestUpstreamOracleConcurrent checks Map's read-modify-write operations,
// bounded scans and FirstIn against upstream's concurrent BBTreeMap.
func TestUpstreamOracleConcurrent(t *testing.T) {
	binary := os.Getenv("GOBBTREE_ORACLE")
	if binary == "" {
		t.Skip("set GOBBTREE_ORACLE to the pinned Rust oracle binary")
	}
	m := New[uint64, uint64]()
	var input, want bytes.Buffer
	result := func(v uint64, ok bool) {
		if ok {
			fmt.Fprintln(&want, v)
		} else {
			fmt.Fprintln(&want, "-")
		}
	}
	rng := rand.New(rand.NewPCG(5, 6))
	for i := 0; i < 20000; i++ {
		key, value := rng.Uint64N(3000), rng.Uint64()
		if i%211 == 0 {
			key = math.MaxUint64
		}
		op := rng.IntN(11)
		if i < 3000 {
			op = 0
		}
		switch op {
		case 0, 1:
			fmt.Fprintf(&input, "I %d %d\n", key, value)
			result(m.Insert(key, value))
		case 2:
			fmt.Fprintf(&input, "D %d\n", key)
			result(m.Delete(key))
		case 3:
			fmt.Fprintf(&input, "G %d\n", key)
			result(m.Get(key))
		case 4:
			fmt.Fprintf(&input, "U %d %d\n", key, value)
			result(m.UpdateOrInsert(key, value, func(_, old, value uint64) uint64 { return old + value }))
		case 5:
			fmt.Fprintf(&input, "X %d\n", key)
			result(m.Update(key, func(_, v uint64) uint64 { return v ^ 1 }))
		case 6:
			fmt.Fprintf(&input, "O %d %d\n", key, value)
			v, _ := m.LoadOrStore(key, value)
			fmt.Fprintln(&want, v)
		case 7:
			fmt.Fprintf(&input, "F %d\n", key)
			if k, v, ok := m.DeleteIf(key, func(k, v uint64) bool { return (k^v)%2 == 0 }); ok {
				fmt.Fprintf(&want, "%d:%d\n", k, v)
			} else {
				fmt.Fprintln(&want, "-")
			}
		case 8, 9:
			op := map[bool]string{false: "B", true: "W"}[op == 9]
			b := randomBounds(rng, key, &input, op)
			recordPanic(&want, func(w *bytes.Buffer) {
				seq := m.Scan(b)
				if op == "W" {
					seq = m.ScanReverse(b)
				}
				for k, v := range seq {
					fmt.Fprintf(w, "%d:%d,", k, v)
				}
			})
		case 10:
			b := randomBounds(rng, key, &input, "M")
			recordPanic(&want, func(w *bytes.Buffer) {
				if k, v, ok := m.FirstIn(b); ok {
					fmt.Fprintf(w, "%d:%d", k, v)
				} else {
					w.WriteString("-")
				}
			})
		}
	}
	command := exec.Command(binary, "concurrent")
	command.Stdin = &input
	var stderr bytes.Buffer
	command.Stderr = &stderr
	got, err := command.Output()
	if err != nil {
		t.Fatalf("oracle failed: %v: %s", err, stderr.String())
	}
	if !bytes.Equal(got, want.Bytes()) {
		g, w := strings.Split(string(got), "\n"), strings.Split(want.String(), "\n")
		for i := 0; i < min(len(g), len(w)); i++ {
			if g[i] != w[i] {
				t.Fatalf("concurrent oracle mismatch line %d: got %q want %q", i+1, g[i], w[i])
			}
		}
		t.Fatal("concurrent oracle output length mismatch")
	}
	t.Log("matched Map's upserts, conditional deletes, bounded scans and FirstIn against upstream's BBTreeMap")
}
