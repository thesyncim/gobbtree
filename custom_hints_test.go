package gobbtree

import (
	be "encoding/binary"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// TestCustomKeyHintsSurviveMerges builds custom-key maps large enough to
// keep hints, fills their cells by lookups, then removes most keys, which
// merges leaves away under the cells that name them, and inserts others,
// which splits leaves the cells name: every lookup still answers as the
// model does, by the hint where it settles and by the descent where not.
func TestCustomKeyHintsSurviveMerges(t *testing.T) {
	type tree struct {
		insert, delete func(string)
		get            func(string) bool
		hints          func() *customHints
		check          func(t *testing.T) int
	}
	kinds := map[string]func() tree{
		"map": func() tree {
			m := NewMapOf[raceKey, int]()
			return tree{func(k string) { m.Insert(raceKey{k}, 1) }, func(k string) { m.Delete(raceKey{k}) },
				func(k string) bool { _, ok := m.Get(raceKey{k}); return ok }, m.generic.customHints.Load,
				func(t *testing.T) int { return checkGenericTree(t, &m.generic) }}
		},
		"annexed": func() tree {
			m := NewAnnexedMapOf[raceKey, int, noAnnex]()
			return tree{func(k string) { m.Insert(raceKey{k}, 1) }, func(k string) { m.Delete(raceKey{k}) },
				func(k string) bool { _, ok := m.Get(raceKey{k}); return ok }, m.generic.customHints.Load,
				func(t *testing.T) int { return checkGenericTree(t, &m.generic) }}
		},
	}
	// Keys of binary bytes, whose codes spread and whose hints serve: short,
	// past a 13-byte prefix, past 25 bytes of one; and keys of hex text,
	// whose bytes spread over few values, and whose tables turn off.
	binary := func(prefix string, i int) string {
		return prefix + string(be.BigEndian.AppendUint64(nil, uint64(i)*0x9e3779b97f4a7c15))
	}
	families := map[string]func(i int) string{
		"short":  func(i int) string { return binary("", i) },
		"prefix": func(i int) string { return binary("tenant:00042/", i) },
		"deep":   func(i int) string { return binary("/srv/data/shared/objects/", i) },
		"hex":    func(i int) string { return fmt.Sprintf("%08x", uint32(i)*2654435761) },
		"hex-prefix": func(i int) string {
			return fmt.Sprintf("tenant:00042/%016x", uint64(i)*0x9e3779b97f4a7c15)
		},
	}
	const n = 20000
	for fname, key := range families {
		for kind, build := range kinds {
			t.Run(fname+"/"+kind, func(t *testing.T) {
				m := build()
				model := map[string]bool{}
				for i := range n {
					m.insert(key(i))
					model[key(i)] = true
				}
				// Hints are racing reads', which only plain-store builds take.
				if r := m.hints(); seqlockPlainStores && !strings.HasPrefix(fname, "hex") && (r == nil || r.off) {
					t.Fatal("a tree of 20000 binary keys keeps no hints")
				}
				verify := func(when string) {
					for i := range 2 * n {
						if k := key(i); m.get(k) != model[k] {
							t.Fatalf("%s: key %q found %v, want %v", when, k, !model[k], model[k])
						}
					}
				}
				verify("built")
				r := rand.New(rand.NewPCG(11, 12))
				for i := range n {
					if r.IntN(10) < 9 {
						m.delete(key(i))
						delete(model, key(i))
					}
				}
				m.check(t)
				verify("after merges")
				for i := n; i < 2*n; i += 2 {
					m.insert(key(i))
					model[key(i)] = true
				}
				m.check(t)
				verify("after splits")
			})
		}
	}
}
