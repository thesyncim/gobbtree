package gobbtree

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unsafe"
)

// nodeRoutedKey is a defined uint64. The word engine accepts only built-in
// word types, so maps of it run on the generic engine.
type nodeRoutedKey uint64

// checkGenericTree checks t's structure, at rest, and returns its entries:
// every node is unheld; leaves are at one depth; counts keep their minimums
// below the root; keys ascend within their fences; separator codes are
// their windows; an internal node's own separator is its parent's; and
// slots past a count hold nothing.
func checkGenericTree[K any, V any, O keyOrder[K]](t testing.TB, tree *genericTree[K, V, O]) int {
	t.Helper()
	root := tree.root.Load()
	if root == nil {
		return 0
	}
	leafDepth, leafIDs := -1, 0
	// checkPrefix checks that a node's keys, when it has any, begin with its
	// prefix, which its head holds, and returns the prefix's length.
	checkPrefix := func(h *gheader, any bool, keys func() []string) int {
		p := int(h.prefix.Load())
		if !prefixed[O, K]() || !any {
			if p != 0 || h.head[0].Load() != 0 || h.head[1].Load() != 0 {
				t.Fatalf("prefix %d on a node without one", p)
			}
			return p
		}
		all := keys()
		for _, k := range all {
			if len(k) < p || k[:p] != all[0][:p] {
				t.Fatalf("key %q lacks the node's prefix %q", k, all[0][:p])
			}
		}
		var probe gheader
		probe.setPrefix(all[0], p)
		if probe.head[0].Load() != h.head[0].Load() || probe.head[1].Load() != h.head[1].Load() {
			t.Fatal("node head is stale")
		}
		return p
	}
	var check func(h *gheader, low, high *K, depth int) int
	check = func(h *gheader, low, high *K, depth int) int {
		// Readers may still run, latching leaves shared where lookups
		// don't read them optimistically; no writer may hold a node.
		if word := h.latch.Load(); word&latchExclusive != 0 {
			t.Fatalf("node held at rest: %x", word)
		}
		count := int(h.count.Load())
		if count > nodeSize {
			t.Fatalf("count %d", count)
		}
		inside := func(k K) {
			if low != nil && compareKeys[O](k, *low) < 0 || high != nil && compareKeys[O](k, *high) >= 0 {
				t.Fatalf("key %v outside its fences", k)
			}
		}
		if h.leaf {
			if leafDepth < 0 {
				leafDepth = depth
			} else if depth != leafDepth {
				t.Fatalf("leaf at depth %d, others at %d", depth, leafDepth)
			}
			if h != root && count < leafMinLen {
				t.Fatalf("leaf holds %d entries", count)
			}
			// A leaf of integer keys has an ID its tree's directory holds
			// it at (generic_hints.go); others have none.
			if tree.exact != (h.id != 0) || h.id != 0 && tree.leaves.leaf(h.id) != h {
				t.Fatalf("leaf ID %d, exact %v, directory holds another", h.id, tree.exact)
			}
			leafIDs++
			n := asLeaf[K, V](h)
			for i := range n.tags {
				if want := uint8(0); i < count && exactCodes[O, K]() && n.tags[i] != uint8(n.codes[i]) || (i >= count || !exactCodes[O, K]()) && n.tags[i] != want {
					t.Fatalf("leaf tag %d is %x, its code %x", i, n.tags[i], n.codes[i])
				}
			}
			// A leaf's head is its separator, its low fence, and its first
			// key; the leftmost leaf, and leaves of exact codes, have none.
			if head, ok := tree.head(n); ok != (headed[O, K]() && low != nil) {
				t.Fatalf("leaf head present %v, low fence %v", ok, low != nil)
			} else if ok && (compareKeys[O](head, *low) != 0 || count == 0 || compareKeys[O](head, tree.key(n, 0)) != 0) {
				t.Fatalf("leaf head %v is not its separator %v and first key", head, *low)
			}
			// A leaf's prefix is what its fences share (D26), the shared
			// prefix standing in for one missing on a border (D50).
			if prefixed[O, K]() && count > 0 {
				if want := tree.fencePrefix(low, high); int(h.prefix.Load()) != want {
					t.Fatalf("leaf prefix %d, fences share %d", h.prefix.Load(), want)
				}
			}
			p := checkPrefix(h, count > 0, func() []string {
				var keys []string
				for i := 0; i < count; i++ {
					keys = append(keys, textView(new(tree.key(n, i))))
				}
				return keys
			})
			for i := 0; i < count; i++ {
				key := tree.key(n, i)
				inside(key)
				if prefixed[O, K]() {
					// A text key's bytes lie in its leaf's buffer, among those
					// written.
					c := n.text()
					if ref := c.slots[i].ref; ref>>32+uint64(uint32(ref)) > uint64(c.used) || c.used > c.size || uint32(ref) > 0 && c.buf == nil {
						t.Fatalf("leaf key %d at %#x past the %d bytes of %d its buffer holds", i, ref, c.used, c.size)
					}
					if s := tree.keyText(n, i); !strings.HasPrefix(s, tree.sharedText()) {
						t.Fatalf("key %q lacks the shared prefix %q", s, tree.sharedText())
					}
				}
				if n.codes[i] != windowKey[O](key, p) {
					t.Fatalf("leaf code %d is stale", i)
				}
				if prefixed[O, K]() {
					if k := probeText(tree.keyText(n, i), p); n.text().slots[i].tail != k.tail || refLen(n.text().slots[i].ref) != k.len {
						t.Fatalf("leaf tail or length %d is stale", i)
					}
				}
				if i > 0 && compareKeys[O](tree.key(n, i-1), key) >= 0 {
					t.Fatalf("leaf keys out of order at %d", i)
				}
			}
			if prefixed[O, K]() {
				// Each key's bytes are its own, and a hole's no key's.
				c := n.text()
				spans := [][2]uint64{}
				for i := 0; i < count; i++ {
					if r := c.slots[i].ref; uint32(r) > 0 {
						spans = append(spans, [2]uint64{r >> 32, r>>32 + uint64(uint32(r))})
					}
				}
				if h := c.hole; uint32(h) > 0 {
					if h>>32+uint64(uint32(h)) > uint64(c.used) {
						t.Fatalf("leaf hole %#x past the %d bytes written", h, c.used)
					}
					spans = append(spans, [2]uint64{h >> 32, h>>32 + uint64(uint32(h))})
				}
				slices.SortFunc(spans, func(a, b [2]uint64) int { return cmp.Compare(a[0], b[0]) })
				for i := 1; i < len(spans); i++ {
					if spans[i][0] < spans[i-1][1] {
						t.Fatalf("leaf keys' bytes overlap: %v", spans)
					}
				}
			}
			for i := count; i < nodeSize; i++ {
				if prefixed[O, K]() && n.text().slots[i] != (textSlot{}) {
					t.Fatalf("leaf slot %d past count %d keeps a tail or key", i, count)
				}
				if !prefixed[O, K]() && !codeKeys[O, K]() && !reflect.ValueOf(&n.keyed()[i]).Elem().IsZero() {
					t.Fatalf("leaf slot %d past count %d keeps a key", i, count)
				}
				if n.codes[i] != codePad || !reflect.ValueOf(&n.values[i]).Elem().IsZero() {
					t.Fatalf("leaf slot %d past count %d holds an entry", i, count)
				}
			}
			return count
		}
		n := asInner[K](h)
		if h == root && count < 2 || h != root && count < innerMinLen {
			t.Fatalf("internal node holds %d children", count)
		}
		// Separator i is the first key of the leftmost leaf below child i
		// (D49); the node keeps none, only their codes.
		firstBelow := func(i int) K {
			h := n.children[i].Load()
			for !h.leaf {
				h = asInner[K](h).children[0].Load()
			}
			return tree.key(asLeaf[K, V](h), 0)
		}
		if low != nil && compareKeys[O](firstBelow(0), *low) != 0 {
			t.Fatal("internal node's first key is not its low fence")
		}
		seps := make([]K, count)
		for i := 1; i < count; i++ {
			seps[i] = firstBelow(i)
			inside(seps[i])
			if i > 1 && compareKeys[O](seps[i-1], seps[i]) >= 0 {
				t.Fatalf("separators out of order at %d", i)
			}
		}
		// A text node's prefix is what its fences share (D26, D50), and its
		// summaries the top halves of its separators' windows past it.
		p := 0
		if prefixed[O, K]() {
			p = tree.fencePrefix(low, high)
			if int(h.prefix.Load()) != p {
				t.Fatalf("internal prefix %d, fences share %d", h.prefix.Load(), p)
			}
			sums := n.sums()
			for i := 0; i < nodeSize; i++ {
				want := ^uint32(0)
				switch {
				case i == 0:
					want = 0
				case i < count:
					want = summaryOf(textView(&seps[i]), p)
				}
				if sums[i] != want {
					t.Fatalf("summary %d is %x, want %x", i, sums[i], want)
				}
			}
		} else {
			if h.prefix.Load() != 0 || n.codes[0].Load() != 0 {
				t.Fatal("internal code 0 is not 0, or a prefix without text keys")
			}
			for i := 1; i < count; i++ {
				if n.codes[i].Load() != windowKey[O](seps[i], 0) {
					t.Fatalf("separator %d code is stale", i)
				}
			}
		}
		total := 0
		for i := 0; i < count; i++ {
			childLow, childHigh := low, high
			if i > 0 {
				childLow = &seps[i]
			}
			if i+1 < count {
				childHigh = &seps[i+1]
			}
			child := n.children[i].Load()
			if child == nil {
				t.Fatalf("child %d missing", i)
			}
			total += check(child, childLow, childHigh, depth+1)
		}
		for i := count; i < nodeSize; i++ {
			if n.children[i].Load() != nil || !prefixed[O, K]() && n.codes[i].Load() != codePad {
				t.Fatalf("internal slot %d past count %d holds a child", i, count)
			}
		}
		return total
	}
	total := check(root, nil, nil, 0)
	// The directory holds the tree's leaves and nothing else.
	if ids := tree.leaves.ids.Load(); tree.exact && ids != nil {
		held := 0
		for i := range *ids {
			if (*ids)[i].Load() != nil {
				held++
			}
		}
		if held != leafIDs {
			t.Fatalf("leaf directory holds %d leaves, the tree %d", held, leafIDs)
		}
	}
	return total
}

// Integer keys' trees route lookups by hints (generic_hints.go): leaves
// split, merge and give back their IDs, which new leaves take, while
// lookups, inserts and deletes go through cells naming leaves by them.
func TestCodeHints(t *testing.T) {
	keyings := map[string]func(uint64) int64{
		"mixed":     func(i uint64) int64 { return int64(mixedKey(i)) },
		"clustered": func(i uint64) int64 { return int64(mixedKey(i>>6)&^(1<<16-1) | i&63) },
		"small":     func(i uint64) int64 { return int64(i%5000) - 2500 },
	}
	for name, keyAt := range keyings {
		t.Run(name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(1, 2))
			var tr Tree[int64, string]
			m := New[int64, string]()
			ref := map[int64]string{}
			const span = 20000
			for op := 0; op < 300000; op++ {
				k := keyAt(uint64(rng.IntN(span)))
				switch rng.IntN(8) {
				case 0, 1, 2:
					v := fmt.Sprint(op)
					tr.Insert(k, v)
					m.Insert(k, v)
					ref[k] = v
				case 3, 4:
					_, had := ref[k]
					if _, ok := tr.Delete(k); ok != had {
						t.Fatalf("op %d: Tree delete %d found %v, want %v", op, k, ok, had)
					}
					if _, ok := m.Delete(k); ok != had {
						t.Fatalf("op %d: Map delete %d found %v, want %v", op, k, ok, had)
					}
					delete(ref, k)
				default:
					want, had := ref[k]
					if v, ok := tr.Get(k); ok != had || v != want {
						t.Fatalf("op %d: Tree get %d = %q %v, want %q %v", op, k, v, ok, want, had)
					}
					if v, ok := m.Get(k); ok != had || v != want {
						t.Fatalf("op %d: Map get %d = %q %v, want %q %v", op, k, v, ok, want, had)
					}
				}
				if op%50000 == 0 {
					checkTree(t, &tr)
					checkMap(t, &m.mapCore)
				}
			}
			for k, want := range ref {
				if v, ok := tr.Get(k); !ok || v != want {
					t.Fatalf("Tree lost %d", k)
				}
			}
			if got := checkMap(t, &m.mapCore); got != len(ref) {
				t.Fatalf("Map holds %d, want %d", got, len(ref))
			}
		})
	}
}

// genericKeys are the key families the generic tests run over: lengths
// around the eight-byte window, with codes that tie; and keys sharing more
// than a node's head holds.
var genericKeys = map[string]func(uint64) string{
	"mixed": genericKey,
	"long prefix": func(i uint64) string {
		return fmt.Sprintf("https://example.com/users/%016x", mixedKey(i))
	},
}

// genericKey spreads keys over lengths around the eight-byte window, with
// long shared prefixes, so codes tie and separators are compared whole.
func genericKey(i uint64) string {
	switch i % 4 {
	case 0:
		return fmt.Sprintf("%x", i)
	case 1:
		return fmt.Sprintf("user:%08x", i)
	case 2:
		return fmt.Sprintf("user:0000%x", i)
	}
	return fmt.Sprintf("user:00000000:%016x:tail", mixedKey(i))
}

func TestGenericChurn(t *testing.T) {
	for name, genericKey := range genericKeys {
		t.Run(name, func(t *testing.T) { testGenericChurn(t, genericKey) })
	}
}

func testGenericChurn(t *testing.T, genericKey func(uint64) string) {
	type value struct {
		key  string
		hits []int
	}
	var m Map[string, value]
	const keys, workers = 4096, 8
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Go(func() {
			// Each worker owns the keys equal to it modulo workers, so its
			// model of them is exact.
			present := map[uint64]bool{}
			x := uint64(worker)*0x9e3779b97f4a7c15 + 1
			for round := 0; round < rounds; round++ {
				for j := 0; j < keys; j++ {
					x = x*6364136223846793005 + 1442695040888963407
					i := uint64(worker) + workers*((x>>33)%(keys/workers))
					k := genericKey(i)
					switch x >> 61 {
					case 0, 1:
						if _, ok := m.Delete(k); ok != present[i] {
							t.Errorf("delete %q found %v, want %v", k, ok, present[i])
						}
						present[i] = false
					case 2:
						if _, v, ok := m.Take(k); ok != present[i] || ok && v.key != k {
							t.Errorf("took %q: %v holding %q, want %v", k, ok, v.key, present[i])
						}
						present[i] = false
					case 3:
						if _, ok := m.UpdateOrInsert(k, value{k, []int{1}}, func(_ string, old, _ value) value {
							return value{old.key, append(old.hits[:len(old.hits):len(old.hits)], 1)}
						}); ok != present[i] {
							t.Errorf("upsert %q found %v, want %v", k, ok, present[i])
						}
						present[i] = true
					default:
						if _, ok := m.Insert(k, value{k, []int{0}}); ok != present[i] {
							t.Errorf("insert %q found %v, want %v", k, ok, present[i])
						}
						present[i] = true
					}
					if v, ok := m.Get(k); ok != present[i] || ok && v.key != k {
						t.Errorf("get %q: %v holding %q, want %v", k, ok, v.key, present[i])
					}
					if v, ok := m.Get(genericKey((x >> 20) % keys)); ok && v.key != genericKey((x>>20)%keys) {
						t.Errorf("read %q holding %q", genericKey((x>>20)%keys), v.key)
					}
				}
			}
		})
	}
	wg.Go(func() {
		for round := 0; round < rounds; round++ {
			var last string
			first := true
			for k, v := range m.All() {
				if !first && k <= last || v.key != k {
					t.Errorf("scan: %q after %q holding %q", k, last, v.key)
					return
				}
				last, first = k, false
			}
		}
	})
	wg.Wait()
	if got, want := checkGenericTree(t, &m.generic), seqLen(m.All()); got != want {
		t.Fatalf("tree holds %d entries, scan %d", got, want)
	}
}

// Filling and draining in order grows and shrinks the tree through every
// height, splitting and merging at the borders.
func TestGenericFillAndDrain(t *testing.T) {
	for name, genericKey := range genericKeys {
		t.Run(name, func(t *testing.T) { testGenericFillAndDrain(t, genericKey) })
	}
}

func testGenericFillAndDrain(t *testing.T, genericKey func(uint64) string) {
	var m Map[string, int]
	const n = 20000
	for _, order := range []string{"ascending", "descending", "mixed"} {
		key := func(i int) int {
			switch order {
			case "descending":
				return n - 1 - i
			case "mixed":
				return int(mixedKey(uint64(i)) % n)
			}
			return i
		}
		present := map[int]bool{}
		for i := 0; i < n; i++ {
			k := key(i)
			m.Insert(genericKey(uint64(k)), k)
			present[k] = true
		}
		if got := checkGenericTree(t, &m.generic); got != len(present) {
			t.Fatalf("%s fill: %d entries, want %d", order, got, len(present))
		}
		removed := 0
		for i := 0; i < n && removed < len(present); i++ {
			if i%2 == 0 {
				if _, _, ok := m.PopFirst(); !ok {
					t.Fatalf("%s: PopFirst found nothing with %d left", order, len(present)-removed)
				}
				removed++
			} else if _, ok := m.Delete(genericKey(uint64(key(i)))); ok {
				removed++
			}
			if i%997 == 0 {
				if got := checkGenericTree(t, &m.generic); got != len(present)-removed {
					t.Fatalf("%s drain: %d entries, want %d", order, got, len(present)-removed)
				}
			}
		}
		for {
			if _, _, ok := m.PopLast(); !ok {
				break
			}
		}
		if !m.IsEmpty() || checkGenericTree(t, &m.generic) != 0 {
			t.Fatalf("%s: not empty after draining", order)
		}
	}
}

// Lookups of word keys copy values without latching their leaf; a copy torn
// by a writer must never be returned. Values here span many words, with
// pointers and an interface, and each is consistent only whole.
func TestGenericOptimisticValues(t *testing.T) {
	type wide struct {
		id    uint64
		name  *string
		check []uint64
		any   any
		pad   [6]uint64
		self  uint64
	}
	make := func(k, gen uint64) wide {
		name := fmt.Sprint(k, ":", gen)
		w := wide{id: k, name: &name, check: []uint64{k, gen}, any: gen, self: k ^ gen}
		for i := range w.pad {
			w.pad[i] = gen
		}
		return w
	}
	consistent := func(k uint64, w wide) bool {
		gen := w.self ^ k
		if w.id != k || *w.name != fmt.Sprint(k, ":", gen) || len(w.check) != 2 || w.check[0] != k || w.check[1] != gen || w.any.(uint64) != gen {
			return false
		}
		for _, p := range w.pad {
			if p != gen {
				return false
			}
		}
		return true
	}
	var m Map[uint64, wide]
	const keys = 2048
	for k := uint64(0); k < keys; k++ {
		m.Insert(k, make(k, 0))
	}
	ops := 200000
	if testing.Short() {
		ops = 20000
	}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Go(func() {
			x := uint64(w + 1)
			for i := 0; i < ops; i++ {
				x = x*6364136223846793005 + 1442695040888963407
				k := (x >> 33) % keys
				if x>>62 == 0 {
					m.Delete(k)
				}
				m.Insert(k, make(k, x>>40))
			}
		})
	}
	for r := 0; r < 4; r++ {
		wg.Go(func() {
			x := uint64(r + 100)
			for i := 0; i < 2*ops; i++ {
				x = x*6364136223846793005 + 1442695040888963407
				k := (x >> 33) % keys
				if v, ok := m.Get(k); ok && !consistent(k, v) {
					t.Errorf("key %d: torn value %+v", k, v)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := checkGenericTree(t, &m.generic); got != seqLen(m.All()) {
		t.Fatalf("tree holds %d entries, scan %d", got, seqLen(m.All()))
	}
}

// The summary kernels count as the scalar search does, over sorted summaries
// padded with MaxUint32, whatever the target.
func TestLowerSummary(t *testing.T) {
	x := uint64(1)
	next := func() uint64 { x = x*6364136223846793005 + 1442695040888963407; return x }
	for trial := 0; trial < 20000; trial++ {
		var sums [nodeSize]uint32
		n := int(next() % (nodeSize + 1))
		spread := uint32(1) << (next() % 32)
		base := uint32(next())
		for i := range sums {
			sums[i] = ^uint32(0)
			if i < n {
				sums[i] = base + uint32(next())%spread
			}
		}
		slices.Sort(sums[:n])
		for _, s := range []uint32{0, ^uint32(0), uint32(next()), sums[min(n, nodeSize-1)], sums[max(n-1, 0)] + 1} {
			if got, want := lowerSummary(&sums, n, s), lowerSummaryScalar(&sums, n, s); got != want {
				t.Fatalf("n=%d target %x: %d, want %d", n, s, got, want)
			}
		}
	}
}

// Keys without the shared prefix shorten it while readers look keys up: the
// tree's border nodes take the shorter prefix, and every key stays found.
func TestGenericSharedPrefixShrinks(t *testing.T) {
	var m Map[string, int]
	groups := []string{"service/users/region-1/", "service/users/", "service/", "serv", "", "admin/", "zz", "service/users/region-1/x"}
	const per = 3000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	present := func(g, i int) string { return fmt.Sprintf("%s%06d", groups[g], i) }
	for r := 0; r < 4; r++ {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := 0; i < per; i += 7 {
					if v, ok := m.Get(present(0, i)); ok && v != i {
						t.Errorf("get %q = %d", present(0, i), v)
					}
				}
			}
		})
	}
	for i := 0; i < per; i++ {
		m.Insert(present(0, i), i)
	}
	for g := 1; g < len(groups); g++ {
		wg.Go(func() {
			for i := 0; i < per; i++ {
				m.Insert(present(g, i), i)
			}
		})
	}
	m.Insert("", -1)
	close(stop)
	wg.Wait()
	for g := range groups {
		for i := 0; i < per; i++ {
			if v, ok := m.Get(present(g, i)); !ok || v != i {
				t.Fatalf("lost %q: %d, %v", present(g, i), v, ok)
			}
		}
	}
	if v, ok := m.Get(""); !ok || v != -1 {
		t.Fatal("lost the empty key")
	}
	if got := checkGenericTree(t, &m.generic); got != len(groups)*per+1 {
		t.Fatalf("tree holds %d entries, want %d", got, len(groups)*per+1)
	}
	if s := m.generic.sharedText(); s != "" {
		t.Fatalf("shared prefix %q survives the empty key", s)
	}
	var last string
	first := true
	for k := range m.All() {
		if !first && k <= last {
			t.Fatalf("scan: %q after %q", k, last)
		}
		last, first = k, false
	}
}

// stringCode windows any offset: whole windows, tails, past the end, and
// offsets below zero, which panic as slicing does.
func TestStringCodeOffsets(t *testing.T) {
	s := "0123456789abcdefg"
	for offset := 0; offset <= len(s)+9; offset++ {
		var want uint64
		for i := 0; i < 8; i++ {
			if offset+i < len(s) {
				want |= uint64(s[offset+i]) << (56 - 8*i)
			}
		}
		if got := stringCode(s, offset); got != want {
			t.Fatalf("offset %d: %x, want %x", offset, got, want)
		}
	}
	for _, offset := range []int{-1, -8, -9} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("offset %d read before the string", offset)
				}
			}()
			stringCode(s, offset)
		}()
	}
}

// Keys whose windows are all ones share the code padding past a leaf's
// count, and keys of equal codes differ past them: lookups still settle.
func TestGenericTextWindowEdges(t *testing.T) {
	var m Map[string, int]
	var keys []string
	for i := 0; i < 2000; i++ {
		keys = append(keys,
			fmt.Sprintf("\xff\xff\xff\xff\xff\xff\xff\xff%04d", i),
			fmt.Sprintf("\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff%d", i),
			fmt.Sprintf("same-code-%05d", i),
			fmt.Sprintf("same-code-%05d\x00", i),
		)
	}
	for i, k := range keys {
		m.Insert(k, i)
	}
	for i, k := range keys {
		if v, ok := m.Get(k); !ok || v != i {
			t.Fatalf("get %q: %d, %v", k, v, ok)
		}
		if _, ok := m.Get(k + "\x00\x00"); ok {
			t.Fatalf("found %q", k+"\x00\x00")
		}
	}
	if _, ok := m.Get("\xff\xff\xff\xff\xff\xff\xff\xff\xff"); ok {
		t.Fatal("found an absent all-ones key")
	}
	checkGenericTree(t, &m.generic)
}

// fastWindow agrees with stringCode wherever it answers.
func TestFastWindow(t *testing.T) {
	for _, s := range []string{"", "a", "0123456", "01234567", "0123456789abcdefghij", "\xff\x00\xff\x00\xff\x00\xff\x00\xff"} {
		for offset := -2; offset <= len(s)+9; offset++ {
			got, ok := fastWindow(s, offset)
			if !ok {
				continue
			}
			if want := stringCode(s, offset); got != want {
				t.Fatalf("%q at %d: %x, want %x", s, offset, got, want)
			}
		}
	}
}

// headDiff agrees with a byte comparison for every prefix length.
func TestHeadDiff(t *testing.T) {
	prefixes := []string{"", "u", "user:", "user:3a", "user:3a4f", "user:3a4f5b6c7d", "user:3a4f5b6c7d8"}
	keys := []string{"", "u", "user", "user:", "user:3", "user:3a", "user:3a4", "user:3a4f", "user:3a4f5b6c7d", "user:3a4f5b6c7d8e9f", "user:3b", "usex:3a4f5b6c7d8e", "user:3a4f5b6c7d8f"}
	for _, prefix := range prefixes {
		var h gheader
		h.setPrefix(prefix, len(prefix))
		for _, k := range keys {
			want := len(k) >= len(prefix) && k[:len(prefix)] == prefix
			got := len(k) >= len(prefix) && headDiff(stringCode(k, 0), headWindow(k), len(prefix), h.head[0].Load(), h.head[1].Load()) == 0
			if got != want {
				t.Fatalf("%q against %q: %v, want %v", k, prefix, got, want)
			}
		}
	}
}

// leafRoute lays out as every leaf begins.
func TestLeafRouteLayout(t *testing.T) {
	var r leafRoute
	var a gleaf[string, uint64]
	var b gleaf[Bytes, [3]string]
	if unsafe.Offsetof(a.codes) != unsafe.Offsetof(r.codes) || unsafe.Offsetof(b.codes) != unsafe.Offsetof(r.codes) {
		t.Fatalf("gleaf codes at %d and %d, leafRoute's at %d", unsafe.Offsetof(a.codes), unsafe.Offsetof(b.codes), unsafe.Offsetof(r.codes))
	}
}

// innerRoute lays out as every internal node begins.
func TestInnerRouteLayout(t *testing.T) {
	var r innerRoute
	var a ginner[string]
	var b ginner[Bytes]
	for _, n := range []struct{ codes, children uintptr }{
		{unsafe.Offsetof(a.codes), unsafe.Offsetof(a.children)},
		{unsafe.Offsetof(b.codes), unsafe.Offsetof(b.children)},
	} {
		if n.codes != unsafe.Offsetof(r.codes) || n.children != unsafe.Offsetof(r.children) {
			t.Fatalf("ginner fields at %d and %d, innerRoute's at %d and %d", n.codes, n.children, unsafe.Offsetof(r.codes), unsafe.Offsetof(r.children))
		}
	}
}

// growLeaf, from a leaf's windows, leaves it as refitLeaf does from its
// keys: each key's windows past the new prefix.
func TestGrowLeaf(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 9))
	alphabet := []string{"", "\x00", "a", "ab", "abc", "user:", "user:3a", "user:3a4f5b", "\xff\x00\xff"}
	var tr genericTree[string, uint64, natural[string]]
	for trial := 0; trial < 20000; trial++ {
		base := alphabet[rng.IntN(len(alphabet))]
		// Bytes every key shares past the base, which the columns, kept at
		// the base's prefix, don't yet strip.
		var shared []byte
		for range rng.IntN(24) {
			shared = append(shared, "\x00\x01ab\xff"[rng.IntN(5)])
		}
		count := 2 + rng.IntN(nodeSize-1)
		keys := make([]string, 0, count)
		seen := map[string]bool{}
		for tries := 0; len(keys) < count && tries < 1000; tries++ {
			b := append([]byte(base), shared...)
			for range rng.IntN(30) {
				b = append(b, "\x00\x01ab\xff"[rng.IntN(5)])
			}
			if k := string(b); !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		count = len(keys)
		if count < 2 {
			continue
		}
		slices.Sort(keys)
		// Columns at the prefix the base gives, as a split's half keeps.
		q := len(base)
		for _, k := range keys {
			q = min(q, commonPrefix(k, base))
		}
		build := func() *gleaf[string, uint64] {
			n := (&genericTree[string, uint64, natural[string]]{}).newLeaf()
			total := 0
			for _, k := range keys {
				total += len(k)
			}
			tr.reserveKeys(n, total)
			for i, k := range keys {
				n.text().slots[i] = textSlot{stringCode(k, q+8), tr.appendKey(n, k)}
				n.codes[i] = stringCode(k, q)
			}
			n.count.Store(int32(count))
			n.setPrefix(keys[0], q)
			return n
		}
		// Any prefix from q to what the keys share, as a split's fences
		// give its halves.
		p := q + rng.IntN(commonPrefix(keys[0], keys[count-1])-q+1)
		a, b := build(), build()
		tr.growLeaf(a, p)
		tr.refitLeaf(b, p)
		if a.prefix.Load() != b.prefix.Load() || a.head[0].Load() != b.head[0].Load() || a.head[1].Load() != b.head[1].Load() {
			t.Fatalf("%q at %d: prefix %d, want %d", keys, q, a.prefix.Load(), b.prefix.Load())
		}
		for i := range count {
			if a.codes[i] != b.codes[i] || a.text().slots[i].tail != b.text().slots[i].tail || tr.keyText(a, i) != tr.keyText(b, i) {
				t.Fatalf("%q at %d: key %d windows %x %x, want %x %x", keys, q, i, a.codes[i], a.text().slots[i].tail, b.codes[i], b.text().slots[i].tail)
			}
		}
	}
}

// Keys the tree hands out read its leaves' buffers, whose bytes are written
// once: through churn that splits, merges and refills leaves, every key
// taken, popped, found or walked keeps its bytes.
func TestHandedOutKeysKeepTheirBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	keyOf := func(i int) string {
		switch i % 7 {
		case 0:
			return ""
		case 1:
			return strings.Repeat("\x00", i%5)
		case 2:
			return fmt.Sprintf("long:%0300d", i)
		}
		return fmt.Sprintf("user:%016x", mixedKey(uint64(i)))
	}
	type kept struct{ got, want string }
	var keep []kept
	var keepBytes []struct {
		got  Bytes
		want string
	}
	tr := NewTree[string, int]()
	var tb TreeOf[Bytes, int]
	m := New[string, int]()
	const n = 6000
	for round := 0; round < 6; round++ {
		for i := range n {
			k := keyOf(rng.IntN(3 * n))
			tr.Insert(k, i)
			tb.Insert(Bytes(k), i)
			m.Insert(k, i)
		}
		for i := range n {
			k := keyOf(rng.IntN(3 * n))
			if got, _, ok := tr.Take(k); ok {
				keep = append(keep, kept{got, k})
			}
			if got, _, ok := tb.Take(Bytes(k)); ok {
				keepBytes = append(keepBytes, struct {
					got  Bytes
					want string
				}{got, k})
			}
			if got, _, ok := m.Take(k); ok {
				keep = append(keep, kept{got, k})
			}
			if i%50 == 0 {
				if got, _, ok := tr.PopFirst(); ok {
					keep = append(keep, kept{got, strings.Clone(got)})
				}
				if got, _, ok := tr.LowerBound(k); ok {
					keep = append(keep, kept{got, strings.Clone(got)})
				}
				if got, _, ok := m.PopLast(); ok {
					keep = append(keep, kept{got, strings.Clone(got)})
				}
			}
		}
		// Deletes hand out no key: an owner's buffers may then reuse
		// bytes, but none a key handed out before reads.
		for i := range 3 * n {
			k := keyOf(rng.IntN(3 * n))
			tr.Delete(k)
			tb.Delete(Bytes(k))
			if i%3 != 0 {
				tr.Insert(keyOf(rng.IntN(3*n)), i)
				tb.Insert(Bytes(keyOf(rng.IntN(3*n))), i)
			}
		}
		count := 0
		for k := range tr.All() {
			if count++; count%17 == 0 {
				keep = append(keep, kept{k, strings.Clone(k)})
			}
		}
		checkCore(t, &tr.treeCore)
		checkCore(t, &tb.treeCore)
	}
	for _, k := range keep {
		if k.got != k.want {
			t.Fatalf("handed-out key changed: %q, want %q", k.got, k.want)
		}
	}
	for _, k := range keepBytes {
		if string(k.got) != k.want || cap(k.got) != len(k.got) {
			t.Fatalf("handed-out Bytes key changed: %q (cap %d), want %q", k.got, cap(k.got), k.want)
		}
	}
	if len(keep) < 1000 || len(keepBytes) < 1000 {
		t.Fatalf("kept %d and %d keys", len(keep), len(keepBytes))
	}
}

// firstKeysOfLeaves is the first key of every leaf of tree but the
// leftmost, the separators, those of leaves at their minimum first.
func firstKeysOfLeaves[K any, V any, O keyOrder[K]](tree *genericTree[K, V, O]) []K {
	var full, short []K
	var walk func(h *gheader, leftmost bool)
	walk = func(h *gheader, leftmost bool) {
		if h.leaf {
			n := asLeaf[K, V](h)
			if leftmost || n.count.Load() == 0 {
				return
			}
			if n.count.Load() <= leafMinLen {
				short = append(short, tree.key(n, 0))
			} else {
				full = append(full, tree.key(n, 0))
			}
			return
		}
		inner := asInner[K](h)
		for i := 0; i < int(inner.count.Load()); i++ {
			walk(inner.children[i].Load(), leftmost && i == 0)
		}
	}
	if root := tree.root.Load(); root != nil {
		walk(root, true)
	}
	return append(short, full...)
}

// Removing a separator, a leaf's first key, promotes the leaf's next key,
// also where the removal leaves the leaf to merge or borrow; the checker
// holds every separator the first key below it.
func TestSeparatorPromotion(t *testing.T) {
	const n = 6000
	tr := NewTree[string, int]()
	m := New[string, int]()
	var tu Tree[uint64, string]
	for i := range n {
		k := fmt.Sprintf("user:%016x", mixedKey(uint64(i)))
		tr.Insert(k, i)
		m.Insert(k, i)
		tu.Insert(mixedKey(uint64(i)), k)
	}
	if tu.generic == nil {
		t.Fatal("uint64 keys with string values left the generic tree")
	}
	for round := 0; round < 400; round++ {
		if ks := firstKeysOfLeaves(tr.generic); len(ks) > 0 {
			if _, ok := tr.Delete(strings.Clone(ks[0])); !ok {
				t.Fatalf("tree separator %q missing", ks[0])
			}
		}
		if ks := firstKeysOfLeaves(&m.generic); len(ks) > 0 {
			if _, ok := m.Delete(strings.Clone(ks[0])); !ok {
				t.Fatalf("map separator %q missing", ks[0])
			}
		}
		if ks := firstKeysOfLeaves(tu.generic); len(ks) > 0 {
			if _, ok := tu.Delete(ks[0]); !ok {
				t.Fatalf("uint64 separator %d missing", ks[0])
			}
		}
		if round%20 == 0 {
			checkGenericTree(t, tr.generic)
			checkGenericTree(t, &m.generic)
			checkGenericTree(t, tu.generic)
		}
	}
	checkGenericTree(t, tr.generic)
	checkGenericTree(t, &m.generic)
	checkGenericTree(t, tu.generic)
	if tr.Len() != n-400 || tu.Len() != n-400 {
		t.Fatalf("lengths %d and %d", tr.Len(), tu.Len())
	}
}

// A Map's separators change while lookups race them: writers remove and put
// back keys that are leaves' first keys, so their separators are promoted,
// leaves borrow and merge, and readers settle ties against the heads of
// path-like keys, which share long prefixes, and walk the leaves.
func TestMapRacingSeparators(t *testing.T) {
	m := New[string, int]()
	const n = 4000
	key := func(i int) string {
		return fmt.Sprintf("/srv/tenants/objects/o%05d/part/t%02d", i, i%7)
	}
	for i := range n {
		m.Insert(key(i), i)
	}
	var writers, readers sync.WaitGroup
	stop := make(chan struct{})
	for w := range 2 {
		writers.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 1))
			for {
				select {
				case <-stop:
					return
				default:
				}
				i := rng.IntN(n)
				m.Delete(key(i))
				m.Insert(key(i), i)
			}
		})
	}
	for r := range 4 {
		readers.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(r), 2))
			for range 20000 {
				i := rng.IntN(n)
				if v, ok := m.Get(key(i)); ok && v != i {
					t.Errorf("%s holds %d", key(i), v)
					return
				}
				if rng.IntN(64) == 0 {
					prev := ""
					for k := range m.Range(key(i), key(min(i+200, n-1))) {
						if k <= prev {
							t.Errorf("walk went back: %q after %q", k, prev)
							return
						}
						prev = k
					}
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	writers.Wait()
	for i := range n {
		if v, ok := m.Get(key(i)); !ok || v != i {
			t.Fatalf("%s: %d %v after the race", key(i), v, ok)
		}
	}
	checkGenericTree(t, &m.generic)
}

// A Map of integer keys reads through hints while writers split and merge
// leaves, which hands their IDs to new ones: keys that stay must always be
// found, and found with their own values.
func TestMapHintsRacing(t *testing.T) {
	m := New[int64, string]()
	const span = 40000
	key := func(i int) int64 { return int64(mixedKey(uint64(i))) }
	for i := 0; i < span; i++ {
		m.Insert(key(i), strconv.Itoa(i))
	}
	var writers, readers sync.WaitGroup
	stop := make(chan struct{})
	for w := range 3 {
		writers.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 3))
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Odd keys churn, by runs, so leaves empty and merge.
				i := rng.IntN(span/64) * 64
				for j := i + 1; j < i+64; j += 2 {
					m.Delete(key(j))
				}
				for j := i + 1; j < i+64; j += 2 {
					m.Insert(key(j), strconv.Itoa(j))
				}
			}
		})
	}
	for r := range 4 {
		readers.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(r), 4))
			for range 300000 {
				i := rng.IntN(span)
				v, ok := m.Get(key(i))
				if i%2 == 0 && !ok || ok && v != strconv.Itoa(i) {
					t.Errorf("get %d = %q %v", i, v, ok)
					return
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	writers.Wait()
	if got := checkMap(t, &m.mapCore); got != span {
		t.Fatalf("Map holds %d, want %d", got, span)
	}
}
