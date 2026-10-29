package main

import (
	"fmt"
	"runtime"
	"time"

	"github.com/thesyncim/gobbtree"
)

// textKey is the string key of a -text run for the pattern's key k,
// matching text_key in the Rust driver.
func textKey(k uint64) string { return fmt.Sprintf("user:%016x", k) }

// runText runs the string-keyed cells, matching text in the Rust driver: a
// build from keys made beforehand; lookups that hit and miss; with
// mutations, replacing values and deleting entries to insert them back; and
// the Map's parallel lookups. No timed operation makes a key. Each value is
// its key's pattern code.
func runText(kind string, size, rounds, threads, queryCount int, mutations bool, keyAt func(uint64) uint64) {
	doTree := kind == "all" || kind == "tree"
	doMap := kind == "all" || kind == "map"
	tree := gobbtree.NewTree[string, uint64]()
	concurrent := gobbtree.New[string, uint64]()
	build := func(name string, start time.Time, sum uint64) {
		fmt.Printf("%s %.3f ns/op checksum=%d\n", name, float64(time.Since(start).Nanoseconds())/float64(size), sum)
	}
	keys, codes := make([]string, size), make([]uint64, size)
	for i := range keys {
		codes[i] = keyAt(uint64(i))
		keys[i] = textKey(codes[i])
	}
	runtime.GC()
	if doTree {
		var sum uint64
		start := time.Now()
		for i, k := range keys {
			sum += codes[i]
			tree.Insert(k, codes[i])
		}
		build("text_tree_build", start, sum)
	}
	if doMap {
		var sum uint64
		start := time.Now()
		for i, k := range keys {
			sum += codes[i]
			concurrent.Insert(k, codes[i])
		}
		build("text_map_build", start, sum)
	}
	keys, codes = nil, nil
	runtime.GC()
	queries, queryCodes := make([]string, queryCount), make([]uint64, queryCount)
	report := func(name string, start time.Time, sum uint64) {
		fmt.Printf("%s %.3f ns/op checksum=%d\n", name, float64(time.Since(start).Nanoseconds())/float64(rounds*queryCount), sum)
	}
	for _, hit := range []bool{true, false} {
		label := "hit"
		if !hit {
			label = "miss"
		}
		for i := range queries {
			x := mix(uint64(i)+17) % uint64(size)
			if !hit {
				x += uint64(size)
			}
			queryCodes[i] = keyAt(x)
			queries[i] = textKey(queryCodes[i])
		}
		if doTree {
			var sum uint64
			start := time.Now()
			for range rounds {
				for _, q := range queries {
					v, _ := tree.Get(q)
					sum += v
				}
			}
			report("text_tree_"+label, start, sum)
		}
		if doMap {
			var sum uint64
			start := time.Now()
			for range rounds {
				for _, q := range queries {
					v, _ := concurrent.Get(q)
					sum += v
				}
			}
			report("text_map_"+label, start, sum)
		}
	}
	// The hit queries again, for the parallel lookups and the mutations.
	for i := range queries {
		queryCodes[i] = keyAt(mix(uint64(i)+17) % uint64(size))
		queries[i] = textKey(queryCodes[i])
	}
	if threads > 0 && doMap {
		parallel(fmt.Sprintf("text_map_parallel%d_hit", threads), threads, rounds*queryCount, func(worker int) uint64 {
			var sum uint64
			for range rounds {
				for i := range queryCount {
					v, _ := concurrent.Get(queries[(i+worker*113)&(queryCount-1)])
					sum += v
				}
			}
			return sum
		})
	}
	if !mutations {
		return
	}
	if doTree {
		var sum uint64
		start := time.Now()
		for r := range rounds {
			for i, q := range queries {
				old, _ := tree.Insert(q, queryCodes[i]^uint64(r&1))
				sum += old
			}
		}
		report("text_tree_replace", start, sum)
		for i, q := range queries {
			tree.Insert(q, queryCodes[i])
		}
		sum = 0
		start = time.Now()
		for range rounds {
			for i, q := range queries {
				v, _ := tree.Delete(q)
				sum += v
				tree.Insert(q, queryCodes[i])
			}
		}
		report("text_tree_churn_pair", start, sum)
	}
	if doMap {
		var sum uint64
		start := time.Now()
		for r := range rounds {
			for i, q := range queries {
				old, _ := concurrent.Insert(q, queryCodes[i]^uint64(r&1))
				sum += old
			}
		}
		report("text_map_replace", start, sum)
		for i, q := range queries {
			concurrent.Insert(q, queryCodes[i])
		}
		sum = 0
		start = time.Now()
		for range rounds {
			for i, q := range queries {
				v, _ := concurrent.Delete(q)
				sum += v
				concurrent.Insert(q, queryCodes[i])
			}
		}
		report("text_map_churn_pair", start, sum)
	}
}
