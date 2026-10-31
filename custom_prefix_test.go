package gobbtree

import (
	"fmt"
	"iter"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// prefixFamilies are custom key sets whose keys share prefixes as real keys
// do: one constant prefix, a few tenants', nested directories, a prefix
// longer than custom keys' nodes keep, keys that differ only in trailing
// zeros, and keys that break the shared prefix late, at both ends.
var prefixFamilies = map[string]func(r *rand.Rand, i, n int) string{
	"constant": func(r *rand.Rand, _, _ int) string { return fmt.Sprintf("tenant:00042/%016x", r.Uint64()) },
	"tenants":  func(r *rand.Rand, _, _ int) string { return fmt.Sprintf("tenant:%05d/%08x", r.IntN(7), r.Uint32()) },
	"paths": func(r *rand.Rand, _, _ int) string {
		dirs := [...]string{"bin", "lib/python3/site-packages", "share/doc"}
		return fmt.Sprintf("/srv/data/%s/%c/file-%04d", dirs[r.IntN(len(dirs))], 'a'+r.IntN(4), r.IntN(3000))
	},
	"long": func(r *rand.Rand, _, _ int) string {
		return strings.Repeat("p", 70) + fmt.Sprintf("%06x", r.IntN(1<<20))
	},
	"padded": func(r *rand.Rand, _, _ int) string {
		k := strings.Repeat("ab", r.IntN(9)) + strings.Repeat("\x00", r.IntN(4))
		if r.IntN(2) == 0 {
			k += string(rune('a' + r.IntN(3)))
		}
		return k
	},
	"late": func(r *rand.Rand, i, n int) string {
		switch {
		case i < n/2:
			return fmt.Sprintf("m:shared:%08x", r.Uint32())
		case r.IntN(2) == 0:
			return fmt.Sprintf("a%06x", r.IntN(1<<20))
		default:
			return fmt.Sprintf("z%06x", r.IntN(1<<20))
		}
	},
}

// prefixedTree is what the test asks of a MapOf or a TreeOf.
type prefixedTree struct {
	insert func(string)
	delete func(string) bool
	get    func(string) bool
	all    func() iter.Seq2[textKey, int]
	rng    func(lo, hi string) iter.Seq2[textKey, int]
	check  func(t *testing.T) int
}

func TestCustomKeysKeepPrefixes(t *testing.T) {
	for name, family := range prefixFamilies {
		for _, kind := range []string{"map", "tree", "annexed"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				var tree prefixedTree
				switch kind {
				case "annexed":
					m := NewAnnexedMapOf[textKey, int, noAnnex]()
					tree = prefixedTree{
						insert: func(k string) { m.Insert(textKey{k}, len(k)) },
						delete: func(k string) bool { _, ok := m.Delete(textKey{k}); return ok },
						get:    func(k string) bool { _, ok := m.Get(textKey{k}); return ok },
						all:    m.All,
						rng:    func(lo, hi string) iter.Seq2[textKey, int] { return m.Range(textKey{lo}, textKey{hi}) },
						check:  func(t *testing.T) int { return checkGenericTree(t, &m.generic) },
					}
				case "map":
					m := NewMapOf[textKey, int]()
					tree = prefixedTree{
						insert: func(k string) { m.Insert(textKey{k}, len(k)) },
						delete: func(k string) bool { _, ok := m.Delete(textKey{k}); return ok },
						get:    func(k string) bool { _, ok := m.Get(textKey{k}); return ok },
						all:    m.All,
						rng:    func(lo, hi string) iter.Seq2[textKey, int] { return m.Range(textKey{lo}, textKey{hi}) },
						check:  func(t *testing.T) int { return checkGenericTree(t, &m.generic) },
					}
				default:
					m := NewTreeOf[textKey, int]()
					tree = prefixedTree{
						insert: func(k string) { m.Insert(textKey{k}, len(k)) },
						delete: func(k string) bool { _, ok := m.Delete(textKey{k}); return ok },
						get:    func(k string) bool { _, ok := m.Get(textKey{k}); return ok },
						all:    m.All,
						rng:    func(lo, hi string) iter.Seq2[textKey, int] { return m.Range(textKey{lo}, textKey{hi}) },
						check: func(t *testing.T) int {
							if m.generic == nil {
								return 0
							}
							return checkGenericTree(t, m.generic)
						},
					}
				}
				r := rand.New(rand.NewPCG(uint64(len(name)), 7))
				model := map[string]bool{}
				verify := func(step int) {
					t.Helper()
					if got := tree.check(t); got != len(model) {
						t.Fatalf("step %d: tree holds %d entries, the model %d", step, got, len(model))
					}
					want := slices.Sorted(maps.Keys(model))
					var got []string
					for k := range tree.all() {
						got = append(got, k.s)
					}
					if !slices.Equal(got, want) {
						t.Fatalf("step %d: keys differ from the model", step)
					}
					for _, k := range want[:min(len(want), 64)] {
						if !tree.get(k) {
							t.Fatalf("step %d: lost %q", step, k)
						}
						// Neighbors a window can't tell apart, and keys outside
						// every prefix.
						for _, miss := range []string{k + "\x00", k[:len(k)/2], "\x00" + k, "\xff" + k} {
							if tree.get(miss) != model[miss] {
								t.Fatalf("step %d: %q present %v", step, miss, !model[miss])
							}
						}
					}
					// Ranges whose bounds lie outside the shared prefix, or
					// inside it.
					for range 4 {
						lo, hi := family(r, step, 1), family(r, step, 1)
						if r.IntN(3) == 0 {
							lo = ""
						}
						if lo > hi {
							lo, hi = hi, lo
						}
						var got, want []string
						for k := range tree.rng(lo, hi) {
							got = append(got, k.s)
						}
						for _, k := range slices.Sorted(maps.Keys(model)) {
							if k >= lo && k < hi {
								want = append(want, k)
							}
						}
						if !slices.Equal(got, want) {
							t.Fatalf("step %d: range [%q, %q) holds %d keys, want %d", step, lo, hi, len(got), len(want))
						}
					}
				}
				const n = 6000
				for i := range n {
					k := family(r, i, n)
					if r.IntN(10) < 7 {
						tree.insert(k)
						model[k] = true
					} else {
						if got := tree.delete(k); got != model[k] {
							t.Fatalf("step %d: delete %q found %v", i, k, got)
						}
						delete(model, k)
					}
					if i%500 == 0 {
						verify(i)
					}
				}
				verify(n)
				// Draining merges and rebalances, and promotes successors
				// that share less.
				keys := slices.Collect(maps.Keys(model))
				r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
				for i, k := range keys {
					if !tree.delete(k) {
						t.Fatalf("drain: lost %q", k)
					}
					delete(model, k)
					if i%300 == 0 {
						verify(n + i)
					}
				}
				verify(2 * n)
			})
		}
	}
}

// TestCustomKeysStripTheSharedPrefix checks the nodes of a tree of keys that
// share one prefix keep it: its windows are past it, so they don't tie.
func TestCustomKeysStripTheSharedPrefix(t *testing.T) {
	m := NewMapOf[textKey, int]()
	for i := range 20000 {
		m.Insert(textKey{fmt.Sprintf("tenant:00042/%016x", uint64(i)*0x9e3779b97f4a7c15)}, i)
	}
	checkGenericTree(t, &m.generic)
	h := m.generic.root.Load()
	for !h.leaf {
		if p := h.prefix.Load(); p < int32(len("tenant:00042/")) {
			t.Fatalf("internal node keeps a prefix of %d bytes", p)
		}
		h = asInner[textKey](h).children[0].Load()
	}
	if p := h.prefix.Load(); p < int32(len("tenant:00042/")) {
		t.Fatalf("leaf keeps a prefix of %d bytes", p)
	}
}
