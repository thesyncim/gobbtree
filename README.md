# gobbtree

A Go adaptation of [XiangpengHao/bbtree](https://github.com/XiangpengHao/bbtree): an ordered B+ tree with dense inline key summaries, prefix stripping for strings, and SIMD node searches written entirely in Go. Requires Go 1.27.x. No handwritten assembly, cgo, or runtime dependencies.

```sh
GOEXPERIMENT=simd go test ./...
```

```go
package main

import "github.com/thesyncim/gobbtree"

func main() {
    m := gobbtree.New[uint64, string]() // safe to share between goroutines
    m.Insert(42, "answer")
    value, found := m.Get(42)
    _, _ = value, found

    m.Scan(10, 100, func(key uint64, value string) bool {
        return true // false stops the scan
    })

    tree := gobbtree.NewTree[string, int]() // one owner; no locking
    tree.Insert("alpha", 1)
    for key, value := range tree.All() {
        _, _ = key, value
    }
}
```

Both types have useful zero values. Keys may be integers, strings, floating point numbers, or defined types with those underlying types. Ordering is `cmp.Compare`, including NaN before other floating point values and equality of signed zero. Keys and values are shallow copies.

`Insert` returns the previous value and a replacement flag. `Delete` returns the removed value and a presence flag. `GetMany` and `InsertMany` fill caller-provided result slices, retaining input order and distinguishing missing entries from zero values. Scan bounds are `[lower, upper)`. `First`, `Last`, `PopFirst`, and `PopLast` include extrema that cannot be expressed as an exclusive upper bound.

`Map` also provides atomic `Update`, `UpdateOrInsert`, `LoadOrStore`, and `DeleteIf`, plus `Snapshot`. Its scans are ordered and weakly consistent during concurrent writes; `Snapshot` makes a consistent copy. Batches are individually consistent operations, not transactions. Callbacks must not reenter the same map. A callback panic releases latches; copy-returning update callbacks commit only after returning. Values containing pointers still refer to caller-owned mutable data.

`Tree` supports `All`, `Reverse`, `Scan`, `ScanReverse`, `Clone`, and `Clear`. Do not mutate a tree during iteration, or copy either map type after use.

## SIMD

With `GOEXPERIMENT=simd`, node search uses Go 1.27's portable `simd` package. Hardware and width are selected by the Go runtime. `gobbtree.SIMD()` reports the actual selection. Without the experiment, or with `GODEBUG=simd=0`, it uses binary search. The emulated setting is tested separately. No CPU-specific assembly is checked into this repository.

## Port boundary

The upstream reference is pinned to `8adae97a0ed97e5c98fa2790312eb0aec9719846`. This is a Go adaptation of the in-memory ordered map, not a Rust API or memory-layout compatibility layer. Node occupancy is 32; values live only in leaves. Prefixes derive from live node keys and are checked on lookup instead of relying on ancestor fences.

Go synchronization uses a structural `RWMutex` plus leaf latches. Reads are data-race-free. In-place writes on different leaves can overlap; splits, merges, changes to child minima, and certain compound operations exclude other operations. This does **not** reproduce upstream's optimistic lock coupling or its scalability claims. `Len` adds an atomic counter absent from upstream's concurrent map.

Rust-specific entry/borrow/allocator APIs, custom `InlineKey` strategies, sets, file images, mmap, and the separate store crate are not implemented. Upstream formal proofs do not transfer to this implementation. See the tests and benchmark evidence for the tested boundary; performance is not a universal promise of beating upstream.

## Validation

```sh
go test ./...
GOEXPERIMENT=simd go test ./...
GOEXPERIMENT=simd GODEBUG=simd=0 go test ./...
GOEXPERIMENT=simd go test -race ./...
GOEXPERIMENT=simd go test -run '^$' -fuzz FuzzTree -fuzztime 20s
cargo build --release --manifest-path internal/oracle/Cargo.toml
GOEXPERIMENT=simd GOBBTREE_ORACLE="$PWD/internal/oracle/target/release/gobbtree-oracle" go test -run TestUpstreamOracle -v
GOEXPERIMENT=simd go test -run '^$' -bench 'Benchmark(Lookup|Search)$' -benchmem
```

The live Rust oracle compares insert/replace/delete/get return values, forward and reverse ranges, lengths, reset, and deletion-to-empty. Ordinary Go tests skip that gate unless its binary is configured; CI requires it. Randomized tests also compare against an independent Go map and sorted keys, check tree invariants, and cover concurrency, panic recovery, vector boundaries, and zero steady-state allocations.

Licensed under MIT OR Apache-2.0, following upstream. See [LICENSE](LICENSE).
