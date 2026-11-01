package gobbtree

import (
	"cmp"
	"fmt"
	"iter"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// prefixedTree is what the test asks of a MapOf, a TreeOf or an annexed
// map, of keys made from strings.
type prefixedTree struct {
	insert func(string)
	delete func(string) bool
	get    func(string) bool
	all    func() iter.Seq[string]
	rng    func(lo, hi string) iter.Seq[string]
	check  func(t *testing.T) int
}

// strings of a map's keys.
func stringsOf[K any, V any](seq iter.Seq2[K, V], of func(K) string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for k := range seq {
			if !yield(of(k)) {
				return
			}
		}
	}
}

// prefixedTrees makes the maps of keys of type K, made from strings by key
// and back by of, that TestCustomKeysKeepPrefixes runs.
func prefixedTrees[K Key[K]](key func(string) K, of func(K) string) map[string]func() prefixedTree {
	type core interface {
		Insert(K, int) (int, bool)
		Delete(K) (int, bool)
		Get(K) (int, bool)
		All() iter.Seq2[K, int]
		Range(K, K) iter.Seq2[K, int]
	}
	treeOf := func(m core, check func(t *testing.T) int) prefixedTree {
		return prefixedTree{
			insert: func(k string) { m.Insert(key(k), len(k)) },
			delete: func(k string) bool { _, ok := m.Delete(key(k)); return ok },
			get:    func(k string) bool { _, ok := m.Get(key(k)); return ok },
			all:    func() iter.Seq[string] { return stringsOf(m.All(), of) },
			rng:    func(lo, hi string) iter.Seq[string] { return stringsOf(m.Range(key(lo), key(hi)), of) },
			check:  check,
		}
	}
	return map[string]func() prefixedTree{
		"map": func() prefixedTree {
			m := NewMapOf[K, int]()
			return treeOf(m, func(t *testing.T) int { return checkGenericTree(t, &m.generic) })
		},
		"tree": func() prefixedTree {
			m := NewTreeOf[K, int]()
			return treeOf(m, func(t *testing.T) int {
				if m.generic == nil {
					return 0
				}
				return checkGenericTree(t, m.generic)
			})
		},
		"annexed": func() prefixedTree {
			m := NewAnnexedMapOf[K, int, noAnnex]()
			return treeOf(m, func(t *testing.T) int { return checkGenericTree(t, &m.generic) })
		},
	}
}

// fromKey is textKey with all of upstream's optional InlineKey methods, as
// a store's keys have: its maps' internal nodes keep 4 bytes of each window.
// CompareFrom checks what the map says is equal.
type fromKey struct{ s string }

func (k fromKey) Compare(o fromKey) int    { return strings.Compare(k.s, o.s) }
func (k fromKey) Window(offset int) uint64 { return StringWindow(k.s, offset) }
func (k fromKey) HashTag() uint8           { return uint8(min(len(k.s), 255)) }
func (k fromKey) CompareFrom(o fromKey, equal int) int {
	return compareCheckedFrom(k.s, o.s, equal)
}
func (k fromKey) ComparePrefix(prefix []byte) int {
	for i, c := range prefix {
		b := byte(0)
		if i < len(k.s) {
			b = k.s[i]
		}
		if b != c {
			return cmp.Compare(b, c)
		}
	}
	return 0
}

// twoKey keeps 2 bytes of each window, in both kinds of node, and fourKey
// 4, as upstream's WIDTH declares: their windows tie often.
type (
	twoKey  struct{ s string }
	fourKey struct{ s string }
)

func (k twoKey) Compare(o twoKey) int                { return strings.Compare(k.s, o.s) }
func (k twoKey) Window(offset int) uint64            { return StringWindow(k.s, offset) }
func (twoKey) InlineWidth() InlineWidth              { return InlineWidthTwo }
func (k twoKey) CompareFrom(o twoKey, equal int) int { return compareCheckedFrom(k.s, o.s, equal) }
func (k fourKey) Compare(o fourKey) int              { return strings.Compare(k.s, o.s) }
func (k fourKey) Window(offset int) uint64           { return StringWindow(k.s, offset) }
func (fourKey) InlineWidth() InlineWidth             { return InlineWidthFour }

// compareCheckedFrom orders a and b, which the map says share their first
// equal bytes, zero-padded, and panics where they don't.
func compareCheckedFrom(a, b string, equal int) int {
	for i := range equal {
		x, y := byte(0), byte(0)
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			panic(fmt.Sprintf("CompareFrom: %q and %q differ at %d, within %d said equal", a, b, i, equal))
		}
	}
	return strings.Compare(a, b)
}

func TestCustomKeysKeepPrefixes(t *testing.T) {
	keyTypes := map[string]map[string]func() prefixedTree{
		"plain": prefixedTrees(func(s string) textKey { return textKey{s} }, func(k textKey) string { return k.s }),
		"from":  prefixedTrees(func(s string) fromKey { return fromKey{s} }, func(k fromKey) string { return k.s }),
		"two":   prefixedTrees(func(s string) twoKey { return twoKey{s} }, func(k twoKey) string { return k.s }),
		"four":  prefixedTrees(func(s string) fourKey { return fourKey{s} }, func(k fourKey) string { return k.s }),
	}
	for name, family := range prefixFamilies {
		for keyType, kinds := range keyTypes {
			for kind, build := range kinds {
				t.Run(name+"/"+keyType+"/"+kind, func(t *testing.T) {
					tree := build()
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
							got = append(got, k)
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
								got = append(got, k)
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

// raceKey has all of upstream's optional InlineKey methods, its CompareFrom
// a whole compare: a racing descent may pass it a stale equalLen, whose
// answer its version check discards.
type raceKey struct{ s string }

func (k raceKey) Compare(o raceKey) int            { return strings.Compare(k.s, o.s) }
func (k raceKey) Window(offset int) uint64         { return StringWindow(k.s, offset) }
func (k raceKey) HashTag() uint8                   { return uint8(min(len(k.s), 255)) }
func (k raceKey) CompareFrom(o raceKey, _ int) int { return strings.Compare(k.s, o.s) }
func (k raceKey) ComparePrefix(prefix []byte) int  { return fromKey(k).ComparePrefix(prefix) }

// TestCustomKeyPrefixesUnderWriters reads a set of keys that stays while
// writers churn others of their family around them, for maps of custom keys
// with and without upstream's optional methods: their prefixes refit, and a
// late family's shared prefix shrinks, as readers descend.
func TestCustomKeyPrefixesUnderWriters(t *testing.T) {
	type tree struct {
		insert, delete func(string)
		get            func(string) bool
		check          func(t *testing.T) int
	}
	kinds := map[string]func() tree{
		"plain/map": func() tree {
			m := NewMapOf[textKey, int]()
			return tree{func(k string) { m.Insert(textKey{k}, 1) }, func(k string) { m.Delete(textKey{k}) },
				func(k string) bool { _, ok := m.Get(textKey{k}); return ok }, func(t *testing.T) int { return checkGenericTree(t, &m.generic) }}
		},
		"options/map": func() tree {
			m := NewMapOf[raceKey, int]()
			return tree{func(k string) { m.Insert(raceKey{k}, 1) }, func(k string) { m.Delete(raceKey{k}) },
				func(k string) bool { _, ok := m.Get(raceKey{k}); return ok }, func(t *testing.T) int { return checkGenericTree(t, &m.generic) }}
		},
		"options/annexed": func() tree {
			m := NewAnnexedMapOf[raceKey, int, noAnnex]()
			return tree{func(k string) { m.Insert(raceKey{k}, 1) }, func(k string) { m.Delete(raceKey{k}) },
				func(k string) bool { _, ok := m.Get(raceKey{k}); return ok }, func(t *testing.T) int { return checkGenericTree(t, &m.generic) }}
		},
	}
	ops := 20000
	if testing.Short() {
		ops = 4000
	}
	for _, family := range []string{"constant", "tenants", "paths", "late", "padded"} {
		for kind, build := range kinds {
			t.Run(family+"/"+kind, func(t *testing.T) {
				m, gen := build(), prefixFamilies[family]
				// Stable keys end in "|s", churned ones in "|w" and probes, never
				// inserted, in "|x": the first half's keys, as the late family's
				// then share their prefix.
				r := rand.New(rand.NewPCG(3, 4))
				stable := map[string]bool{}
				for range 3000 {
					// A family of few keys, as padded's, gives fewer.
					stable[gen(r, 0, 2)+"|s"] = true
				}
				keys := slices.Sorted(maps.Keys(stable))
				for _, k := range keys {
					m.insert(k)
				}
				var wg sync.WaitGroup
				var done atomic.Bool
				for w := range 3 {
					wg.Go(func() {
						r := rand.New(rand.NewPCG(uint64(w), 9))
						for i := range ops {
							k := gen(r, i, ops) + "|w"
							if r.IntN(3) == 0 {
								m.delete(k)
							} else {
								m.insert(k)
							}
						}
					})
				}
				var readers sync.WaitGroup
				for rd := range 3 {
					readers.Go(func() {
						r := rand.New(rand.NewPCG(uint64(rd), 10))
						for !done.Load() {
							k := keys[r.IntN(len(keys))]
							if !m.get(k) {
								t.Errorf("stable key %q missing", k)
								return
							}
							if probe := strings.TrimSuffix(k, "|s") + "|x"; m.get(probe) {
								t.Errorf("probe %q present", probe)
								return
							}
						}
					})
				}
				wg.Wait()
				done.Store(true)
				readers.Wait()
				m.check(t)
				for _, k := range keys {
					if !m.get(k) {
						t.Fatalf("stable key %q missing at rest", k)
					}
				}
			})
		}
	}
}
