package store

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// stressKey is key n of the stress test: stressPrefix, which every key
// shares, then n.
func stressKey(n int) []byte {
	return binary.BigEndian.AppendUint32([]byte(stressPrefix), uint32(n))
}

// stressPrefixes are the stress test's prefixes: one the nodes strip, so
// that their windows past it tell keys apart and their prefixes refit from
// heads whose records move; and one longer than nodes keep of custom keys'
// prefixes, 56 bytes, so that every separator's window ties and each descent
// compares separators' keys, which their leaves' first records hold.
var (
	stressPrefixes = map[string]string{
		"stripped": "0123456789abcdef",
		"tied":     strings.Repeat("0123456789abcdef", 4),
	}
	stressPrefix = stressPrefixes["stripped"]
)

// stableValue is stable key i's value, which no writer changes: every 16th
// one takes overflow pages, which records carry their link to as they move.
func stableValue(i int) []byte {
	if i%16 == 0 {
		return value(i, 3, 2*page+i%(3*page))
	}
	return value(i, 3, 24+i%40)
}

// Readers look stable keys up, and scan runs of them across leaf
// boundaries, while writers insert and remove the keys between them with
// values of changing sizes: leaves compact in place, sliding the stable
// keys' records, their first keys' among them, and writers remove leaves'
// first keys. Every separator ties, so every descent compares the first
// key of a leaf it heads, holding that leaf. A record read after it moved
// or died, or a page after it went back, reads 0xA5 (scribble), not the
// stable value.
func TestReadersAcrossLeavesWhileCompactionsMove(t *testing.T) {
	for name, prefix := range stressPrefixes {
		t.Run(name, func(t *testing.T) {
			stressPrefix = prefix
			defer func() { stressPrefix = stressPrefixes["stripped"] }()
			readAcrossCompactions(t)
		})
	}
}

func readAcrossCompactions(t *testing.T) {
	const stable, writers, readers = 3000, 3, 4
	ops := 60000
	if testing.Short() {
		ops = 10000
	}
	pages := &counting{}
	s := NewIn(pages)
	// Stable key i is 4i; writer w churns 4i+1+w.
	for i := range stable {
		mustInsert(t, s, stressKey(4*i), stableValue(i))
	}
	var done atomic.Bool
	slidBefore := slidFirst.Load()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 21))
			for range ops {
				k := stressKey(4*r.IntN(stable) + 1 + w)
				if r.IntN(3) == 0 {
					s.Remove(k)
					continue
				}
				if _, err := s.Insert(k, value(w, r.IntN(100), 8+r.IntN(200))); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	var readerWG sync.WaitGroup
	for rd := range readers {
		readerWG.Go(func() {
			r := rand.New(rand.NewPCG(uint64(rd), 22))
			var buf []byte
			for !done.Load() {
				i := r.IntN(stable)
				var ok bool
				if buf, ok = s.GetInto(stressKey(4*i), buf[:0]); !ok || !bytes.Equal(buf, stableValue(i)) {
					t.Errorf("reader %d: stable key %d read %v %x", rd, i, ok, buf)
					return
				}
				if !s.Contains(stressKey(4 * i)) {
					t.Errorf("reader %d: stable key %d missing", rd, i)
					return
				}
				if !scanRun(t, s, i, stable, r.IntN(2) == 0) {
					return
				}
			}
		})
	}
	wg.Wait()
	done.Store(true)
	readerWG.Wait()
	if slid := slidFirst.Load() - slidBefore; slid == 0 {
		t.Fatal("no compaction slid a leaf's first record")
	} else {
		t.Logf("compactions slid %d leaves' first records", slid)
	}
	for i := range stable {
		assertGet(t, s, stressKey(4*i), stableValue(i), true)
	}
	checkLive(t, s)
	assertCleared(t, s, pages)
}

// scanRun scans up to 64 entries from stable key i, forwards or back, and
// checks that the stable keys among them come in order, with their values,
// none skipped.
func scanRun(t *testing.T, s *Store, i, stable int, forwards bool) bool {
	t.Helper()
	seq, next, step := s.Scan(AtLeast(stressKey(4*i))), i, 1
	if !forwards {
		seq, step = s.ScanReverse(AtMost(stressKey(4*i))), -1
	}
	seen := 0
	for k, v := range seq {
		n := int(binary.BigEndian.Uint32(k[len(stressPrefix):]))
		if n%4 == 0 {
			if n/4 != next || !bytes.Equal(v, stableValue(next)) {
				t.Errorf("a scan from stable key %d met stable key %d, want %d", i, n/4, next)
				return false
			}
			next += step
		}
		if seen++; seen == 64 || next < 0 || next == stable {
			break
		}
	}
	return true
}

// stampedValue is key i's value of write seq, length n bytes, at least 12:
// i, seq and n, then bytes that follow from them, so that a value read whole
// tells itself apart from a mix of two writes' bytes, or a dead record's.
func stampedValue(i, seq, n int) []byte {
	v := make([]byte, n)
	binary.LittleEndian.PutUint32(v, uint32(i))
	binary.LittleEndian.PutUint32(v[4:], uint32(seq))
	binary.LittleEndian.PutUint32(v[8:], uint32(n))
	for j := 12; j < n; j++ {
		v[j] = byte(i*7 + seq*13 + j)
	}
	return v
}

// stampedOf reports whether v is a value stampedValue made for key i.
func stampedOf(v []byte, i int) bool {
	if len(v) < 12 || binary.LittleEndian.Uint32(v) != uint32(i) || binary.LittleEndian.Uint32(v[8:]) != uint32(len(v)) {
		return false
	}
	seq := int(binary.LittleEndian.Uint32(v[4:]))
	for j := 12; j < len(v); j++ {
		if v[j] != byte(i*7+seq*13+j) {
			return false
		}
	}
	return true
}

// Readers look keys up while writers rewrite the very values they read: in
// place, at the same length or shorter, and moved, longer, or into overflow
// pages; and remove keys and insert them again, singly and by runs, which
// merges leaves and splits them, while compactions slide what is left. A
// lookup reads its record racing, checked by its leaf's version, or holding
// the leaf: either way every value it returns is one write's, whole, and
// every key no writer removes is found.
func TestReadersSeeWholeValuesWhileWritersRewriteThem(t *testing.T) {
	for name, prefix := range stressPrefixes {
		t.Run(name, func(t *testing.T) {
			stressPrefix = prefix
			defer func() { stressPrefix = stressPrefixes["stripped"] }()
			readWhileRewritten(t)
		})
	}
}

func readWhileRewritten(t *testing.T) {
	const keys, writers, readers = 4000, 3, 5
	ops := 150000
	if testing.Short() {
		ops = 20000
	}
	pages := &counting{}
	s := NewIn(pages)
	// Key i is writer i%writers's; it removes none of those i%4 == 0.
	for i := range keys {
		mustInsert(t, s, stressKey(i), stampedValue(i, 0, 12+i%50))
	}
	length := func(r *rand.Rand) int {
		switch r.IntN(64) {
		case 0:
			return 2*page + r.IntN(page)
		case 1, 2:
			return 12
		}
		return 12 + r.IntN(240)
	}
	var done atomic.Bool
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 31))
			mine := func(i int) int { return i - i%writers + w }
			for op := range ops {
				i := mine(r.IntN(keys - writers))
				seq := op + 1
				switch c := r.IntN(16); {
				case c == 0 && i%4 != 0:
					s.Remove(stressKey(i))
				case c == 1:
					// A run of this writer's keys, removed and inserted
					// again: their leaves merge, then split.
					start := mine(r.IntN(keys - 400))
					for j := start; j < start+300 && j < keys; j += writers {
						if j%4 != 0 {
							s.Remove(stressKey(j))
						}
					}
					for j := start; j < start+300 && j < keys; j += writers {
						if _, err := s.Insert(stressKey(j), stampedValue(j, seq, length(r))); err != nil {
							t.Error(err)
							return
						}
					}
				default:
					if _, err := s.Insert(stressKey(i), stampedValue(i, seq, length(r))); err != nil {
						t.Error(err)
						return
					}
				}
			}
		})
	}
	var readerWG sync.WaitGroup
	var reads, found atomic.Int64
	for rd := range readers {
		readerWG.Go(func() {
			r := rand.New(rand.NewPCG(uint64(rd), 32))
			var buf []byte
			n, hits := 0, 0
			defer func() { reads.Add(int64(n)); found.Add(int64(hits)) }()
			for !done.Load() {
				i := r.IntN(keys)
				var ok bool
				buf, ok = s.GetInto(stressKey(i), buf[:0])
				n++
				if ok {
					hits++
					if !stampedOf(buf, i) {
						t.Errorf("reader %d: key %d read %d bytes no write wrote: %x", rd, i, len(buf), buf[:min(len(buf), 64)])
						return
					}
				} else if i%4 == 0 {
					t.Errorf("reader %d: key %d, which no writer removes, missing", rd, i)
					return
				}
				if i%4 == 0 && !s.Contains(stressKey(i)) {
					t.Errorf("reader %d: Contains misses key %d, which no writer removes", rd, i)
					return
				}
			}
		})
	}
	wg.Wait()
	done.Store(true)
	readerWG.Wait()
	t.Logf("%d reads, %d found", reads.Load(), found.Load())
	for i := range keys {
		if v, ok := s.Get(stressKey(i)); ok && !stampedOf(v, i) {
			t.Fatalf("key %d holds %x", i, v)
		} else if !ok && i%4 == 0 {
			t.Fatalf("key %d, which no writer removes, missing", i)
		}
	}
	checkLive(t, s)
	assertCleared(t, s, pages)
}
