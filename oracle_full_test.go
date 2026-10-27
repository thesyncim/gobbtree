package gobbtree

import (
	"bytes"
	"cmp"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runOracle feeds input to the pinned Rust oracle in mode and requires its
// output to equal want, line for line.
func runOracle(t *testing.T, mode string, input, want *bytes.Buffer) {
	t.Helper()
	binary := os.Getenv("GOBBTREE_ORACLE")
	if binary == "" {
		t.Skip("set GOBBTREE_ORACLE to the pinned Rust oracle binary")
	}
	command := exec.Command(binary, mode)
	command.Stdin = input
	var stderr bytes.Buffer
	command.Stderr = &stderr
	got, err := command.Output()
	if err != nil {
		t.Fatalf("%s oracle failed: %v: %s", mode, err, stderr.String())
	}
	if !bytes.Equal(got, want.Bytes()) {
		g, w := strings.Split(string(got), "\n"), strings.Split(want.String(), "\n")
		in := strings.Split(input.String(), "\n")
		for i := 0; i < min(len(g), len(w)); i++ {
			if g[i] != w[i] {
				t.Fatalf("%s oracle mismatch at line %d (%q): upstream %q, Go %q", mode, i+1, in[i], g[i], w[i])
			}
		}
		t.Fatalf("%s oracle output length: upstream %d lines, Go %d", mode, len(g), len(w))
	}
}

func optional(v uint64, ok bool) string {
	if ok {
		return fmt.Sprint(v)
	}
	return "-"
}
func entryText(k, v uint64, ok bool) string {
	if ok {
		return fmt.Sprintf("%d:%d", k, v)
	}
	return "-"
}

// TestUpstreamOracleFull drives every single-owner map operation Go mirrors
// against upstream's unsync BBTreeMap: point operations, the entry API,
// extract_if, retain, split_off and append, the mutable iterators, the batch
// calls, Clone, Eq and Ord, and consuming iteration.
func TestUpstreamOracleFull(t *testing.T) {
	tr := NewTree[uint64, uint64]()
	var input, want bytes.Buffer
	rng := rand.New(rand.NewPCG(31, 41))
	key := func() uint64 { return rng.Uint64N(3000) }
	line := func(format string, args ...any) { fmt.Fprintf(&input, format+"\n", args...) }
	for i := 0; i < 30000; i++ {
		k, v := key(), rng.Uint64()
		op := rng.IntN(36)
		if i < 3000 {
			op = 0
		}
		var out string
		switch op {
		case 0, 1, 2:
			line("I %d %d", k, v)
			out = optional(tr.Insert(k, v))
		case 3, 4:
			line("D %d", k)
			out = optional(tr.Delete(k))
		case 5:
			line("G %d", k)
			out = optional(tr.Get(k))
		case 6:
			line("K %d", k)
			out = entryText(tr.GetKeyValue(k))
		case 7:
			line("H %d", k)
			out = fmt.Sprint(tr.Contains(k))
		case 8:
			line("A %d", k)
			out = entryText(tr.Take(k))
		case 9:
			line("F")
			out = entryText(tr.First())
		case 10:
			line("Z")
			out = entryText(tr.Last())
		case 11:
			end := k % 2
			line("P %d", end)
			if end == 0 {
				out = entryText(tr.PopFirst())
			} else {
				out = entryText(tr.PopLast())
			}
		case 12:
			line("N %d %d", k, v)
			out = fmt.Sprint(tr.Entry(k).OrInsert(v))
		case 13:
			d := v % 1000
			line("O %d %d", k, d)
			out = fmt.Sprint(tr.Entry(k).AndModify(func(x uint64) uint64 { return x + d }).OrInsert(d))
		case 14:
			line("W %d", k)
			out = fmt.Sprint(tr.Entry(k).OrDefault())
		case 15:
			line("Wk %d", k)
			out = fmt.Sprint(tr.Entry(k).OrInsertWithKey(func(k uint64) uint64 { return k ^ 0xff }))
		case 16:
			line("Q %d %d", k, v)
			if e := tr.Entry(k); e.Occupied() {
				old, _ := e.Insert(v)
				out = fmt.Sprintf("o:%d", old)
			} else {
				e.Insert(v)
				out = fmt.Sprintf("v:%d", v)
			}
		case 17:
			line("Qe %d %d", k, v)
			e := tr.Entry(k).InsertEntry(v)
			value, _ := e.Value()
			out = fmt.Sprintf("%d:%d", e.Key(), value)
		case 18:
			line("Y %d", k)
			out = "-"
			if e := tr.Entry(k); e.Occupied() {
				out = entryText(e.Remove())
			}
		case 19:
			end := k % 2
			line("B %d", end)
			e, ok := tr.FirstEntry()
			if end == 1 {
				e, ok = tr.LastEntry()
			}
			out = "-"
			if ok {
				out = entryText(e.Remove())
			}
		case 20:
			// Upstream's extract_if takes inverted bounds and extracts nothing.
			lo, hi, m := k, key(), 2+v%5
			line("E %d %d %d", lo, hi, m)
			var b strings.Builder
			for k, v := range tr.ExtractIf(Between(lo, hi), func(k, v uint64) bool { return (k^v)%m == 0 }) {
				fmt.Fprintf(&b, "%d:%d,", k, v)
			}
			out = b.String()
		case 21:
			m := 5 + v%20
			line("T %d", m)
			tr.Retain(func(k, v uint64) bool { return (k^v)%m != 0 })
			out = fmt.Sprint(tr.Len())
		case 22:
			line("S %d", k)
			high := tr.SplitOff(k)
			out = fmt.Sprintf("%d,%d,%s", tr.Len(), high.Len(), entryText(high.First()))
			tr.Append(high)
			out += fmt.Sprintf(",%d,%d", tr.Len(), high.Len())
		case 23:
			d := v % 1000
			line("X %d", d)
			tr.UpdateAll(func(_, x uint64) uint64 { return x + d })
			out = fmt.Sprint(tr.Len())
		case 24:
			d := v % 1000
			line("Xi %d", d)
			tr.UpdateAll(func(k, x uint64) uint64 { return x + (k ^ d) })
			out = fmt.Sprint(tr.Len())
		case 25:
			lo, d := min(k, key()), v%1000
			hi := lo + uint64(rng.IntN(400))
			line("R %d %d %d", lo, hi, d)
			tr.UpdateRange(Between(lo, hi), func(_, x uint64) uint64 { return x + d })
			out = fmt.Sprint(tr.Len())
		case 26:
			keys := []uint64{k, key(), k, key()}
			line("M %d %d %d %d", keys[0], keys[1], keys[2], keys[3])
			var dst [4]Result[uint64]
			tr.GetMany(keys, dst[:])
			for _, r := range dst {
				out += optional(r.Value, r.OK) + ","
			}
		case 27:
			pairs := []Pair[uint64, uint64]{{k, v}, {key(), rng.Uint64()}, {k, rng.Uint64()}, {key(), rng.Uint64()}}
			line("IM %d %d %d %d %d %d %d %d", pairs[0].Key, pairs[0].Value, pairs[1].Key, pairs[1].Value, pairs[2].Key, pairs[2].Value, pairs[3].Key, pairs[3].Value)
			var dst [4]Result[uint64]
			tr.InsertMany(pairs, dst[:])
			for _, r := range dst {
				out += optional(r.Value, r.OK) + ","
			}
		case 28:
			line("C %d %d", k, v)
			c := tr.Clone()
			same := c.EqualFunc(tr, func(a, b uint64) bool { return a == b })
			c.Insert(k, v)
			out = fmt.Sprintf("%v,%v,%d", same, c.EqualFunc(tr, func(a, b uint64) bool { return a == b }), tr.CompareFunc(c, cmp.Compare))
		case 29, 30, 31:
			n := rng.IntN(6)
			code := [...]string{"J", "Jk", "Jv"}[op-29]
			line("%s %d", code, n)
			taken := 0
			for k, v := range tr.Clone().Drain() {
				if taken == n {
					break
				}
				taken++
				switch op {
				case 29:
					out += fmt.Sprintf("%d:%d,", k, v)
				case 30:
					out += fmt.Sprintf("%d,", k)
				default:
					out += fmt.Sprintf("%d,", v)
				}
			}
		case 32:
			line("V")
			var keys, values uint64
			for k := range tr.Keys() {
				keys ^= k
			}
			for v := range tr.Values() {
				values ^= v
			}
			count := 0
			for range tr.All() {
				count++
			}
			out = fmt.Sprintf("%d,%d,%d", keys, values, count)
		case 33:
			lo := k
			hi := lo + uint64(rng.IntN(300))
			line("L %d %d", lo, hi)
			for k, v := range tr.Range(lo, hi) {
				out += fmt.Sprintf("%d:%d,", k, v)
			}
		case 34:
			line("Len")
			out = fmt.Sprint(tr.Len())
		case 35:
			if rng.IntN(20) != 0 {
				line("Len")
				out = fmt.Sprint(tr.Len())
				break
			}
			line("Clr")
			tr.Clear()
			out = fmt.Sprint(tr.IsEmpty())
		}
		fmt.Fprintln(&want, out)
	}
	runOracle(t, "full", &input, &want)
}

// TestUpstreamOracleConcurrentFull drives the concurrent map's remaining
// operations against upstream's BBTreeMap: read, remove_entry, the batch
// calls including repeated keys, range_cloned, clear, the owner's get_mut and
// iter_mut, and consuming iteration.
func TestUpstreamOracleConcurrentFull(t *testing.T) {
	m := New[uint64, uint64]()
	var input, want bytes.Buffer
	rng := rand.New(rand.NewPCG(59, 26))
	key := func() uint64 { return rng.Uint64N(2500) }
	line := func(format string, args ...any) { fmt.Fprintf(&input, format+"\n", args...) }
	for i := 0; i < 25000; i++ {
		k, v := key(), rng.Uint64()
		op := rng.IntN(18)
		if i < 2500 {
			op = 0
		}
		var out string
		switch op {
		case 0, 1:
			line("I %d %d", k, v)
			out = optional(m.Insert(k, v))
		case 2:
			line("D %d", k)
			out = optional(m.Delete(k))
		case 3:
			line("R %d", k)
			out = "-"
			m.Read(k, func(k, v uint64) { out = fmt.Sprintf("%d:%d", k, v) })
		case 4:
			line("A %d", k)
			out = entryText(m.Take(k))
		case 5:
			line("H %d", k)
			out = fmt.Sprint(m.Contains(k))
		case 6:
			line("E")
			out = fmt.Sprint(m.IsEmpty())
		case 7:
			keys := []uint64{k, key(), key(), k}
			line("GM %d %d %d %d", keys[0], keys[1], keys[2], keys[3])
			var dst [4]Result[uint64]
			m.GetMany(keys, dst[:])
			for _, r := range dst {
				out += optional(r.Value, r.OK) + ","
			}
		case 8:
			keys := []uint64{k, key(), k, key()}
			line("RM %d %d %d %d", keys[0], keys[1], keys[2], keys[3])
			got := [4]string{"-", "-", "-", "-"}
			m.ReadMany(keys, func(i int, k, v uint64) { got[i] = fmt.Sprintf("%d:%d", k, v) })
			out = strings.Join(got[:], ",") + ","
		case 9, 10:
			pairs := []Pair[uint64, uint64]{{k, v % 1000}, {key(), rng.Uint64N(1000)}, {k, rng.Uint64N(1000)}, {key(), rng.Uint64N(1000)}}
			code := map[bool]string{false: "IM", true: "UM"}[op == 10]
			line("%s %d %d %d %d %d %d %d %d", code, pairs[0].Key, pairs[0].Value, pairs[1].Key, pairs[1].Value, pairs[2].Key, pairs[2].Value, pairs[3].Key, pairs[3].Value)
			var dst [4]Result[uint64]
			if op == 9 {
				m.InsertMany(pairs, dst[:])
			} else {
				m.UpdateOrInsertMany(pairs, func(_, old, value uint64) uint64 { return old + value }, dst[:])
			}
			for _, r := range dst {
				out += optional(r.Value, r.OK) + ","
			}
		case 11, 12:
			b := randomBounds(rng, k, &input, "RC")
			var w bytes.Buffer
			recordPanic(&w, func(w *bytes.Buffer) {
				for k, v := range m.Scan(b) {
					fmt.Fprintf(w, "%d:%d,", k, v)
				}
			})
			want.Write(w.Bytes())
			continue
		case 13:
			d := v % 1000
			line("XM %d", d)
			m.UpdateAll(func(k, x uint64) uint64 { return x + (k ^ d) })
			out = "-"
		case 14:
			line("GU %d", k)
			out = optional(m.Update(k, func(_, x uint64) uint64 { return x ^ 3 }))
		case 15:
			line("G %d", k)
			out = optional(m.Get(k))
		case 16:
			if rng.IntN(30) != 0 {
				line("G %d", k)
				out = optional(m.Get(k))
				break
			}
			line("C")
			m.Clear()
			out = fmt.Sprint(m.IsEmpty())
		case 17:
			if rng.IntN(40) != 0 {
				line("E")
				out = fmt.Sprint(m.IsEmpty())
				break
			}
			n := rng.IntN(5)
			line("J %d", n)
			taken := 0
			for k, v := range m.Drain() {
				if taken == n {
					break
				}
				taken++
				out += fmt.Sprintf("%d:%d,", k, v)
			}
			if !m.IsEmpty() {
				t.Fatal("Drain left entries behind")
			}
		}
		fmt.Fprintln(&want, out)
	}
	runOracle(t, "concurrent", &input, &want)
}

// TestUpstreamSetOracleFull drives the set operations the base set oracle
// leaves out: get, replace, take, first and last, the relations, Eq and Ord,
// contains_many and insert_many, range, Clone and consuming iteration.
func TestUpstreamSetOracleFull(t *testing.T) {
	sets := [2]*Set[uint64]{NewSet[uint64](), NewSet[uint64]()}
	var input, want bytes.Buffer
	rng := rand.New(rand.NewPCG(83, 17))
	key := func() uint64 { return rng.Uint64N(1500) }
	line := func(format string, args ...any) { fmt.Fprintf(&input, format+"\n", args...) }
	for i := 0; i < 12000; i++ {
		slot := rng.IntN(2)
		s, k := sets[slot], key()
		op := rng.IntN(13)
		if i < 1500 {
			op = 0
		}
		var out string
		switch op {
		case 0, 1:
			line("I %d %d", slot, k)
			out = fmt.Sprint(s.Insert(k))
		case 2:
			line("D %d %d", slot, k)
			out = fmt.Sprint(s.Delete(k))
		case 3:
			line("Gt %d %d", slot, k)
			out = optional(s.Get(k))
		case 4:
			line("Rp %d %d", slot, k)
			out = optional(s.Replace(k))
		case 5:
			line("Tk %d %d", slot, k)
			out = optional(s.Take(k))
		case 6:
			line("Fi %d", slot)
			out = optional(s.First())
		case 7:
			line("La %d", slot)
			out = optional(s.Last())
		case 8:
			line("Rel")
			out = fmt.Sprintf("%v,%v,%v,%v,%d", sets[0].IsSubset(sets[1]), sets[0].IsSuperset(sets[1]), sets[0].IsDisjoint(sets[1]), sets[0].Equal(sets[1]), sets[0].Compare(sets[1]))
		case 9, 10:
			keys := []uint64{k, key(), k, key()}
			code := map[bool]string{false: "CM", true: "IM"}[op == 10]
			line("%s %d %d %d %d %d", code, slot, keys[0], keys[1], keys[2], keys[3])
			var dst [4]bool
			if op == 9 {
				s.ContainsMany(keys, dst[:])
			} else {
				s.InsertMany(keys, dst[:])
			}
			out = fmt.Sprintf("[%v, %v, %v, %v]", dst[0], dst[1], dst[2], dst[3])
		case 11:
			hi := k + uint64(rng.IntN(200))
			line("Rg %d %d %d", slot, k, hi)
			for k := range s.Range(k, hi) {
				out += fmt.Sprintf("%d,", k)
			}
		case 12:
			n := rng.IntN(5)
			line("Cl %d %d", slot, n)
			c := s.Clone()
			out = fmt.Sprintf("%v,", c.Equal(s))
			taken := 0
			for k := range c.Drain() {
				if taken == n {
					break
				}
				taken++
				out += fmt.Sprintf("%d,", k)
			}
		}
		fmt.Fprintln(&want, out)
	}
	runOracle(t, "sets", &input, &want)
}
