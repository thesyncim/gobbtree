package gobbtree

import (
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMapSoak runs a numeric Map under concurrent writers, readers and
// scanners against an exact reference. Each writer owns the keys congruent to
// its index, so it can keep a sequential model of them, while all writers'
// keys share leaves, splits and merges. Values carry their key, so readers
// can check any value they see, and scans must ascend strictly. At each
// checkpoint every goroutine pauses and the whole map is compared with the
// union of the models.
//
// GOBBTREE_SOAK sets how long each key pattern runs (a Go duration); the
// default is a short smoke run.
func TestMapSoak(t *testing.T) {
	duration := 300 * time.Millisecond
	if s := os.Getenv("GOBBTREE_SOAK"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatalf("GOBBTREE_SOAK: %v", err)
		}
		duration = d
	} else if testing.Short() {
		duration = 50 * time.Millisecond
	}
	for _, pattern := range []string{"mixed", "sequential", "clustered"} {
		t.Run(pattern, func(t *testing.T) { soakMap(t, pattern, duration) })
	}
}

const (
	soakWriters = 6
	soakReaders = 3
	soakKeyBits = 20 // keys stay below 1<<soakKeyBits before spreading
)

// soakValue packs the key's low bits beside a counter, so a value read for
// the wrong key is caught.
func soakValue(key, counter uint64) uint64 { return counter<<32 | key&(1<<32-1) }
func soakKeyOf(value uint64) uint64        { return value & (1<<32 - 1) }

// soakSpread maps an index to a key in the pattern. It is injective and keeps
// the index's low byte in bits 1-8, which decide the key's owner.
func soakSpread(pattern string, i uint64) uint64 {
	switch pattern {
	case "sequential":
		return i << 1
	case "clustered":
		return (mixedKey(i>>8)&(1<<24-1))<<16 | (i&255)<<1
	}
	return mixedKey(i)&^(1<<soakKeyBits-1)<<1 | i<<1
}

func soakMap(t *testing.T, pattern string, duration time.Duration) {
	var m Map[uint64, uint64]
	models := make([]map[uint64]uint64, soakWriters)
	for w := range models {
		models[w] = map[uint64]uint64{}
	}
	var (
		stop     atomic.Bool
		pause    sync.RWMutex // writers and readers hold it shared; checkpoints exclusively
		failures = make(chan string, 64)
		wg       sync.WaitGroup
		ops      atomic.Int64
	)
	fail := func(format string, args ...any) {
		select {
		case failures <- fmt.Sprintf(format, args...):
		default:
		}
		stop.Store(true)
	}
	// Sequential runs mostly append: each writer advances its own cursor
	// through the keys it owns.
	for w := range soakWriters {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 99))
			model := models[w]
			cursor := uint64(w)
			// Draw indexes until one lands on a key this writer owns.
			next := func() uint64 {
				if pattern == "sequential" && rng.IntN(4) != 0 {
					for cursor++; soakOwner(soakSpread(pattern, cursor)) != w; cursor++ {
					}
					return soakSpread(pattern, cursor)
				}
				for {
					i := uint64(rng.IntN(1 << soakKeyBits))
					if pattern == "sequential" {
						i %= cursor + 1
					}
					if key := soakSpread(pattern, i); soakOwner(key) == w {
						return key
					}
				}
			}
			counter := uint64(0)
			check := func(op string, key, got uint64, gotOK bool, want uint64, wantOK bool) {
				if gotOK != wantOK || gotOK && got != want {
					fail("%s %s(%d): got %d %v, want %d %v", pattern, op, key, got, gotOK, want, wantOK)
				}
			}
			for !stop.Load() {
				pause.RLock()
				for range 64 {
					key := next()
					counter++
					value := soakValue(key, counter)
					old, had := model[key]
					switch op := rng.IntN(100); {
					case op < 30:
						got, ok := m.Insert(key, value)
						check("Insert", key, got, ok, old, had)
						model[key] = value
					case op < 45:
						got, ok := m.Delete(key)
						check("Delete", key, got, ok, old, had)
						delete(model, key)
					case op < 60:
						got, ok := m.Get(key)
						check("Get", key, got, ok, old, had)
					case op < 68:
						got, ok := m.UpdateOrInsert(key, value, func(k, o, v uint64) uint64 {
							if k != key || o != old {
								fail("%s UpdateOrInsert(%d) merge saw %d, %d", pattern, key, k, o)
							}
							return v
						})
						check("UpdateOrInsert", key, got, ok, old, had)
						model[key] = value
					case op < 74:
						got, ok := m.Update(key, func(k, o uint64) uint64 { return value })
						check("Update", key, got, ok, value, had)
						if had {
							model[key] = value
						}
					case op < 80:
						got, loaded := m.LoadOrStore(key, value)
						if loaded != had || had && got != old || !had && got != value {
							fail("%s LoadOrStore(%d): got %d %v, had %d %v", pattern, key, got, loaded, old, had)
						}
						if !had {
							model[key] = value
						}
					case op < 86:
						remove := counter&1 == 0
						k, got, ok := m.DeleteIf(key, func(k, v uint64) bool { return remove })
						check("DeleteIf", key, got, ok, old, had && remove)
						if ok && k != key {
							fail("%s DeleteIf(%d) returned key %d", pattern, key, k)
						}
						if had && remove {
							delete(model, key)
						}
					case op < 92:
						k, got, ok := m.Take(key)
						check("Take", key, got, ok, old, had)
						if ok && k != key {
							fail("%s Take(%d) returned key %d", pattern, key, k)
						}
						delete(model, key)
					default:
						keys := [4]uint64{key, next(), next(), next()}
						var dst [4]Result[uint64]
						m.GetMany(keys[:], dst[:])
						for i, k := range keys {
							want, wantOK := model[k]
							check("GetMany", k, dst[i].Value, dst[i].OK, want, wantOK)
						}
					}
					ops.Add(1)
				}
				pause.RUnlock()
			}
		})
	}
	// Readers see other writers' keys mid-flight: any value must belong to
	// its key, and scans must ascend strictly within their bounds.
	for r := range soakReaders {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(r), 7))
			for !stop.Load() {
				pause.RLock()
				for range 32 {
					key := soakSpread(pattern, uint64(rng.IntN(1<<soakKeyBits)))
					if v, ok := m.Get(key); ok && soakKeyOf(v) != key&(1<<32-1) {
						fail("%s Get(%d) returned a value of key %d", pattern, key, soakKeyOf(v))
					}
					lo := key
					hi := lo + uint64(rng.IntN(1<<12))<<1
					if hi < lo {
						hi = ^uint64(0)
					}
					var prev uint64
					first := true
					seq := m.Scan(Between(lo, hi))
					if rng.IntN(2) == 0 {
						seq = m.ScanReverse(AtLeast(lo).AtMost(hi))
					}
					count := 0
					for k, v := range seq {
						if k < lo || k > hi || soakKeyOf(v) != k&(1<<32-1) {
							fail("%s scan [%d, %d] yielded %d:%d", pattern, lo, hi, k, v)
						}
						if !first && k == prev {
							fail("%s scan [%d, %d] repeated %d", pattern, lo, hi, k)
						}
						prev, first = k, false
						if count++; count == 256 {
							break
						}
					}
					if k, v, ok := m.FirstIn(AtLeast(lo)); ok && (k < lo || soakKeyOf(v) != k&(1<<32-1)) {
						fail("%s FirstIn(%d) = %d:%d", pattern, lo, k, v)
					}
				}
				pause.RUnlock()
			}
		})
	}
	// Checkpoints compare everything while the others wait.
	deadline := time.Now().Add(duration)
	checkpoints := 0
	for !stop.Load() && time.Now().Before(deadline) {
		time.Sleep(min(time.Until(deadline), 25*time.Millisecond))
		pause.Lock()
		checkpoints++
		want := 0
		for _, model := range models {
			want += len(model)
		}
		if m.Len() != want {
			fail("%s checkpoint %d: Len %d, models hold %d", pattern, checkpoints, m.Len(), want)
		}
		seen, prev, first := 0, uint64(0), true
		for k, v := range m.All() {
			if !first && k <= prev {
				fail("%s checkpoint %d: All not ascending at %d after %d", pattern, checkpoints, k, prev)
				break
			}
			prev, first = k, false
			w := soakOwner(k)
			if mv, ok := models[w][k]; !ok || mv != v {
				fail("%s checkpoint %d: map holds %d:%d, model %d %v", pattern, checkpoints, k, v, mv, ok)
				break
			}
			seen++
		}
		if seen != want && !stop.Load() {
			fail("%s checkpoint %d: All visited %d of %d", pattern, checkpoints, seen, want)
		}
		pause.Unlock()
	}
	stop.Store(true)
	wg.Wait()
	close(failures)
	for f := range failures {
		t.Error(f)
	}
	t.Logf("%s: %d operations, %d checkpoints, %d keys at the end", pattern, ops.Load(), checkpoints, m.Len())
}

// soakOwner is the writer that owns key: the low byte of its index under
// every pattern, which the spread keeps in bits 1-8, or bits 0-7 when mixed.
func soakOwner(key uint64) int {
	return int(key>>1&255) % soakWriters
}
