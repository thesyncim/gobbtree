package gobbtree

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
)

type annexKey uint64

func (k annexKey) Compare(o annexKey) int   { return cmp.Compare(k, o) }
func (k annexKey) Window(offset int) uint64 { return Uint64Window(uint64(k), offset) }

func sortedKeys(keys []annexKey) bool           { return slices.IsSortedFunc(keys, annexKey.Compare) }
func holdsKey(keys []annexKey, k annexKey) bool { return slices.Contains(keys, k) }

// testPages hands out pages and checks that each comes back once, as
// upstream's TestPages.
type testPages struct {
	mu   sync.Mutex
	live map[*testPage]bool
}

type testPage struct{ pages *testPages }

func (p *testPages) allocate() *testPage {
	p.mu.Lock()
	defer p.mu.Unlock()
	page := &testPage{p}
	p.live[page] = true
	return page
}

func (p *testPages) free(page *testPage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.live[page] {
		panic("a page freed twice, or never taken")
	}
	delete(p.live, page)
}

func (p *testPages) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.live)
}

// pagePerEntry is upstream's test annex: a page for each entry of its leaf.
type pagePerEntry struct{ pages []*testPage }

func (a *pagePerEntry) Split(right *pagePerEntry) {
	half := len(a.pages) / 2
	right.pages = append(right.pages, a.pages[half:]...)
	a.pages = a.pages[:half:half]
}

func (a *pagePerEntry) Merge(right *pagePerEntry) { a.pages = append(a.pages, right.pages...) }

func (a *pagePerEntry) Release() {
	for _, page := range a.pages {
		page.pages.free(page)
	}
}

type pagedMap = AnnexedMapOf[annexKey, uint64, pagePerEntry, *pagePerEntry]

func pagedInsert(t *testing.T, m *pagedMap, pages *testPages, key annexKey, value uint64, check bool) bool {
	t.Helper()
	found, ok := m.UpdateOrInsertWithLeaf(key, value,
		func(leaf AnnexedLeaf[annexKey, pagePerEntry], stored annexKey, v *uint64) bool {
			if check && (!sortedKeys(leaf.Keys()) || !holdsKey(leaf.Keys(), stored)) {
				t.Errorf("an occupied leaf's keys %v miss %d", leaf.Keys(), stored)
			}
			*v = value
			return true
		},
		func(leaf AnnexedLeaf[annexKey, pagePerEntry], fresh annexKey, _ uint64) bool {
			if check && (!sortedKeys(leaf.Keys()) || holdsKey(leaf.Keys(), fresh)) {
				t.Errorf("a vacant leaf's keys %v hold %d", leaf.Keys(), fresh)
			}
			leaf.Annex().pages = append(leaf.Annex().pages, pages.allocate())
			return true
		})
	if !ok {
		t.Fatal("the test's pages have no budget")
	}
	return !found
}

func pagedDelete(t *testing.T, m *pagedMap, key annexKey, check bool) (uint64, bool) {
	t.Helper()
	_, v, ok := m.DeleteWithLeaf(key, func(leaf AnnexedLeaf[annexKey, pagePerEntry], stored annexKey, _ uint64) {
		if check && (!sortedKeys(leaf.Keys()) || !holdsKey(leaf.Keys(), stored)) {
			t.Errorf("a removal's leaf keys %v miss %d", leaf.Keys(), stored)
		}
		a := leaf.Annex()
		if n := len(a.pages); n > 0 {
			a.pages[n-1].pages.free(a.pages[n-1])
			a.pages = a.pages[:n-1]
		}
	})
	return v, ok
}

// annexPages is how many pages the annexes of m's leaves hold.
func annexPages(m *pagedMap) int {
	t := &m.generic
	var walk func(h *gheader) int
	walk = func(h *gheader) int {
		if h == nil {
			return 0
		}
		if h.leaf {
			return len((*pagePerEntry)(t.annexOf(asLeaf[annexKey, uint64](h))).pages)
		}
		total := 0
		n := asInner[annexKey](h)
		for i := range int(n.count.Load()) {
			total += walk(n.children[i].Load())
		}
		return total
	}
	return walk(t.root.Load())
}

// Annexes follow their leaves through splits, merges and Clear, as
// upstream's annexes_follow_their_leaves_through_splits_merges_and_teardown:
// each entry's page stays in some annex, and every page comes back.
func TestAnnexesFollowTheirLeaves(t *testing.T) {
	pages := &testPages{live: map[*testPage]bool{}}
	m := NewAnnexedMapOf[annexKey, uint64, pagePerEntry]()
	rng := rand.New(rand.NewPCG(7, 7))
	model := map[annexKey]uint64{}
	for step := range uint64(30000) {
		key := annexKey(rng.Uint64N(3000))
		check := step%16 == 0
		if rng.IntN(3) < 2 {
			_, had := model[key]
			if fresh := pagedInsert(t, m, pages, key, step, check); fresh == had {
				t.Fatalf("step %d: insert of %d fresh %v, model had it %v", step, key, fresh, had)
			}
			model[key] = step
		} else {
			want, had := model[key]
			got, ok := pagedDelete(t, m, key, check)
			if ok != had || got != want {
				t.Fatalf("step %d: delete of %d gave %d %v, want %d %v", step, key, got, ok, want, had)
			}
			delete(model, key)
		}
	}
	if n := checkGenericTree(t, &m.generic); n != len(model) {
		t.Fatalf("tree holds %d entries, model %d", n, len(model))
	}
	checkPages(t, m, pages, len(model))
	for k, v := range model {
		if got, ok := m.Get(k); !ok || got != v {
			t.Fatalf("Get(%d) = %d %v, want %d", k, got, ok, v)
		}
	}
	m.Clear()
	if pages.count() != 0 {
		t.Fatalf("%d pages outlived Clear", pages.count())
	}
}

// checkPages checks that every live page is in an annex, and that each of
// the entries put one there. Entries move between leaves without their
// pages, as removals and inserts rebalance them, so a removal from a leaf
// whose annex holds none frees none: pages outnumber entries by those.
func checkPages(t *testing.T, m *pagedMap, pages *testPages, entries int) {
	t.Helper()
	if held := annexPages(m); held != pages.count() || held < entries {
		t.Fatalf("annexes hold %d pages, %d live, for %d entries", held, pages.count(), entries)
	}
}

// A refused insert changes nothing, as upstream's
// a_refused_insert_changes_nothing: new keys land in leaves with room, in
// full leaves that would split, and past the last key.
func TestAnnexedRefusedInsertChangesNothing(t *testing.T) {
	pages := &testPages{live: map[*testPage]bool{}}
	m := NewAnnexedMapOf[annexKey, uint64, pagePerEntry]()
	rng := rand.New(rand.NewPCG(8, 8))
	model := map[annexKey]uint64{}
	const count = 2000
	for step := range uint64(count) {
		key := annexKey(rng.Uint64N(2 * count))
		pagedInsert(t, m, pages, key, step, step%16 == 0)
		model[key] = step
	}
	for key := annexKey(0); key < 2*count+100; key += 3 {
		found, ok := m.UpdateOrInsertWithLeaf(key, 0,
			func(AnnexedLeaf[annexKey, pagePerEntry], annexKey, *uint64) bool { return false },
			func(AnnexedLeaf[annexKey, pagePerEntry], annexKey, uint64) bool { return false })
		if _, had := model[key]; ok || found != had {
			t.Fatalf("refused insert of %d: found %v ok %v, model had it %v", key, found, ok, had)
		}
	}
	if n := checkGenericTree(t, &m.generic); n != len(model) {
		t.Fatalf("tree holds %d entries, model %d", n, len(model))
	}
	for k, v := range model {
		if got, ok := m.Get(k); !ok || got != v {
			t.Fatalf("Get(%d) = %d %v, want %d", k, got, ok, v)
		}
	}
	checkPages(t, m, pages, len(model))
	m.Clear()
	if pages.count() != 0 {
		t.Fatalf("%d pages outlived Clear", pages.count())
	}
}

// Goroutines inserting and deleting with their leaves keep each entry's
// page in an annex, while splits and merges move them.
func TestAnnexesUnderConcurrentWrites(t *testing.T) {
	pages := &testPages{live: map[*testPage]bool{}}
	m := NewAnnexedMapOf[annexKey, uint64, pagePerEntry]()
	const workers, keys = 4, 4000
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 9))
			for step := range uint64(20000) {
				key := annexKey(rng.Uint64N(keys)*workers + uint64(w))
				if rng.IntN(3) < 2 {
					pagedInsert(t, m, pages, key, step, false)
				} else {
					pagedDelete(t, m, key, false)
				}
			}
		})
	}
	wg.Wait()
	checkPages(t, m, pages, checkGenericTree(t, &m.generic))
	m.Clear()
	if pages.count() != 0 {
		t.Fatalf("%d pages outlived Clear", pages.count())
	}
}

// A vacant callback that panics as its full leaf would split lets go of
// what it held, with nothing changed: the map goes on.
func TestAnnexedVacantPanicReleases(t *testing.T) {
	pages := &testPages{live: map[*testPage]bool{}}
	m := NewAnnexedMapOf[annexKey, uint64, pagePerEntry]()
	for k := range annexKey(nodeSize * 4) {
		pagedInsert(t, m, pages, k*2, uint64(k), false)
	}
	for k := range annexKey(nodeSize * 4) {
		func() {
			defer func() { _ = recover() }()
			m.UpdateOrInsertWithLeaf(k*2+1, 0,
				func(AnnexedLeaf[annexKey, pagePerEntry], annexKey, *uint64) bool { panic("occupied") },
				func(AnnexedLeaf[annexKey, pagePerEntry], annexKey, uint64) bool { panic("vacant") })
		}()
	}
	if n := checkGenericTree(t, &m.generic); n != nodeSize*4 {
		t.Fatalf("tree holds %d entries after panics", n)
	}
	for k := range annexKey(nodeSize * 4) {
		pagedInsert(t, m, pages, k*2+1, 0, false)
	}
	n := checkGenericTree(t, &m.generic)
	if n != nodeSize*8 {
		t.Fatalf("tree holds %d entries", n)
	}
	checkPages(t, m, pages, n)
}

// cellKey orders by the value its cell holds, as a store's key orders by
// the bytes of its record: replacing a key with an equal one in another
// cell, as a record moves, keeps lookups past it right even once the old
// cell changes, as the leaf's copy of its first key moves with it.
type cellKey struct{ cell *uint64 }

func (k cellKey) Compare(o cellKey) int    { return cmp.Compare(*k.cell, *o.cell) }
func (k cellKey) Window(offset int) uint64 { return Uint64Window(*k.cell, offset) }

type noAnnex struct{}

func (*noAnnex) Split(*noAnnex) {}
func (*noAnnex) Merge(*noAnnex) {}
func (*noAnnex) Release()       {}

func TestAnnexedReplaceKeyMovesTheHead(t *testing.T) {
	m := NewAnnexedMapOf[cellKey, uint64, noAnnex]()
	key := func(v uint64) cellKey { return cellKey{&v} }
	accept := func(AnnexedLeaf[cellKey, noAnnex], cellKey, uint64) bool { return true }
	const n = nodeSize * 8
	for v := range uint64(n) {
		m.UpdateOrInsertWithLeaf(key(v*2), v, nil, accept)
	}
	// Move every key to a new cell, through its leaf, then spoil the old
	// cells: a stale copy of a leaf's first key would now misroute.
	var old []*uint64
	for v := range uint64(n) {
		m.UpdateOrInsertWithLeaf(key(v*2), v,
			func(leaf AnnexedLeaf[cellKey, noAnnex], stored cellKey, _ *uint64) bool {
				for i, k := range leaf.Keys() {
					if *k.cell == *stored.cell {
						old = append(old, k.cell)
						leaf.ReplaceKey(i, key(*k.cell))
					}
				}
				return true
			}, accept)
	}
	for _, cell := range old {
		*cell = 1 << 62
	}
	for v := range uint64(n) {
		if got, ok := m.Get(key(v * 2)); !ok || got != v {
			t.Fatalf("Get(%d) = %d %v after its key moved", v*2, got, ok)
		}
		if _, ok := m.Get(key(v*2 + 1)); ok {
			t.Fatalf("Get(%d) found a key never inserted", v*2+1)
		}
	}
}

// leafIDs mints, retires and frees the ids of an identity annex, which
// names its leaf: the root's first leaf is 0, Split names the new one, and
// Merge and Release retire them.
type leafIDs struct {
	mu   sync.Mutex
	next uint64
	live map[uint64]bool
}

// testLeafIDs are TestAnnexesNameTheirLeaves's, which sets them anew.
var testLeafIDs *leafIDs

type leafID struct{ id uint64 }

func (a *leafID) Split(right *leafID) {
	testLeafIDs.mu.Lock()
	defer testLeafIDs.mu.Unlock()
	testLeafIDs.next++
	right.id = testLeafIDs.next
	testLeafIDs.live[right.id] = true
}

func (a *leafID) Merge(right *leafID) {
	testLeafIDs.mu.Lock()
	defer testLeafIDs.mu.Unlock()
	if !testLeafIDs.live[right.id] {
		panic("merged a leaf twice")
	}
	delete(testLeafIDs.live, right.id)
}

func (a *leafID) Release() {
	testLeafIDs.mu.Lock()
	defer testLeafIDs.mu.Unlock()
	delete(testLeafIDs.live, a.id)
}

// Each split, merge and Clear calls its annex method once: the tree's
// leaves carry exactly the ids minted and not retired, one each.
func TestAnnexesNameTheirLeaves(t *testing.T) {
	testLeafIDs = &leafIDs{live: map[uint64]bool{0: true}}
	m := NewAnnexedMapOf[annexKey, uint64, leafID]()
	accept := func(AnnexedLeaf[annexKey, leafID], annexKey, uint64) bool { return true }
	update := func(AnnexedLeaf[annexKey, leafID], annexKey, *uint64) bool { return true }
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 11))
			for range 30000 {
				key := annexKey(rng.Uint64N(3000)*4 + uint64(w))
				if rng.IntN(3) < 2 {
					m.UpdateOrInsertWithLeaf(key, 1, update, accept)
				} else {
					m.DeleteWithLeaf(key, func(AnnexedLeaf[annexKey, leafID], annexKey, uint64) {})
				}
			}
		})
	}
	wg.Wait()
	checkGenericTree(t, &m.generic)
	tree := &m.generic
	seen := map[uint64]bool{}
	var walk func(h *gheader)
	walk = func(h *gheader) {
		if h.leaf {
			id := (*leafID)(tree.annexOf(asLeaf[annexKey, uint64](h))).id
			if seen[id] {
				t.Fatalf("two leaves carry id %d: a split gave none", id)
			}
			seen[id] = true
			return
		}
		n := asInner[annexKey](h)
		for i := range int(n.count.Load()) {
			walk(n.children[i].Load())
		}
	}
	walk(tree.root.Load())
	testLeafIDs.mu.Lock()
	live := len(testLeafIDs.live)
	for id := range seen {
		if !testLeafIDs.live[id] {
			t.Errorf("a leaf carries retired id %d", id)
		}
	}
	testLeafIDs.mu.Unlock()
	if live != len(seen) {
		t.Fatalf("%d ids live for %d leaves: a merge kept one", live, len(seen))
	}
	m.Clear()
	if len(testLeafIDs.live) != 0 {
		t.Fatalf("%d ids outlived Clear", len(testLeafIDs.live))
	}
}
