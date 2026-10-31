package store

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

// stressKey is key n of the stress test: a 16-byte prefix every key shares,
// then n, so that every separator's window ties and each descent compares
// separators' keys, which their leaves' first records hold.
func stressKey(n int) []byte {
	return binary.BigEndian.AppendUint32([]byte("0123456789abcdef"), uint32(n))
}

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
		n := int(binary.BigEndian.Uint32(k[16:]))
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
