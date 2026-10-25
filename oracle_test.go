package gobbtree

import (
	"bytes"
	"fmt"
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
func TestUpstreamOracle(t *testing.T) {
	binary := os.Getenv("GOBBTREE_ORACLE")
	if binary == "" {
		t.Skip("set GOBBTREE_ORACLE to the pinned Rust oracle binary")
	}
	var tr Tree[uint64, uint64]
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
		op := rng.IntN(10)
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
			tr.Scan(key, end, func(k, v uint64) bool { fmt.Fprintf(&want, "%d:%d,", k, v); return true })
			fmt.Fprintln(&want)
		case 9:
			end := key + 64
			if key > math.MaxUint64-64 {
				end = math.MaxUint64
			}
			fmt.Fprintf(&input, "V %d %d\n", key, end)
			tr.ScanReverse(key, end, func(k, v uint64) bool { fmt.Fprintf(&want, "%d:%d,", k, v); return true })
			fmt.Fprintln(&want)
		}
		if i%3000 == 2999 {
			fmt.Fprintln(&input, "L")
			fmt.Fprintln(&want, tr.Len())
		}
	}
	// Deletion to empty exercises merges and root collapse in both trees.
	keys := make([]uint64, 0, tr.Len())
	for k := range tr.All() {
		keys = append(keys, k)
	}
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	for _, k := range keys {
		fmt.Fprintf(&input, "D %d\n", k)
		result(tr.Delete(k))
	}
	fmt.Fprintln(&input, "L\nC\nI 7 9\nG 7")
	fmt.Fprintln(&want, "0\n-\n-\n9")
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
	t.Log("matched every return value, forward/reverse range, length, reset, and deletion-to-empty against upstream 8adae97")
}
