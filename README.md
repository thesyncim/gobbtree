# gobbtree

**Ordered maps for Go, with SIMD search and efficient range scans.**

A Go adaptation of [XiangpengHao/bbtree](https://github.com/XiangpengHao/bbtree), built for **Go 1.27.x**. Generic keys and values, a concurrent map, and a single-owner tree. Pure Go: no handwritten assembly, cgo, or external runtime dependencies.

[![Tests](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml)
[![amd64 SIMD](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml)

## Quick start

```sh
go get github.com/thesyncim/gobbtree
```

```go
package main

import (
    "fmt"

    "github.com/thesyncim/gobbtree"
)

func main() {
    scores := gobbtree.New[uint64, string]()
    scores.Insert(42, "answer")
    scores.Insert(7, "lucky")

    if value, found := scores.Get(42); found {
        fmt.Println(value) // answer
    }

    // Visit keys in [0, 100), in ascending order.
    scores.Scan(0, 100, func(key uint64, value string) bool {
        fmt.Println(key, value) // 7 lucky, then 42 answer
        return true            // return false to stop
    })
}
```

Enable Go SIMD when building or running your application:

```sh
GOEXPERIMENT=simd go run .
```

## Choose your container

| | `Tree[K, V]` | `Map[K, V]` |
|---|---|---|
| Constructor | `NewTree[K, V]()` | `New[K, V]()` |
| Access | One owner; concurrent reads when there are no writes | Concurrent readers and writers |
| Ordered iteration | Forward, reverse, and bounded scans | Forward iteration and bounded scans in either direction |
| Copy | `Clone()` | Consistent `Snapshot()` |
| Conditional updates | — | `Update`, `UpdateOrInsert`, `LoadOrStore`, `DeleteIf` |

Both zero values are ready to use. Keys support Go's ordered types, including strings and defined numeric types; values can be any type. Do not copy a container after first use.

## Core operations

| Operation | Result |
|---|---|
| `Get(key)` | Value and presence flag |
| `Insert(key, value)` | Previous value and replacement flag |
| `Delete(key)` | Removed value and presence flag |
| `GetMany(keys, dst)` / `InsertMany(entries, dst)` | Results in input order, using caller-provided storage |
| `Scan(lower, upper, visit)` / `ScanReverse(...)` | Ordered traversal of `[lower, upper)` |
| `First()` / `Last()` | Smallest or largest key and its value |
| `PopFirst()` / `PopLast()` | Remove and return an end entry |
| `Len()` / `IsEmpty()` / `Clear()` | Size and reset |

`All()` works with Go's range-over-function syntax. `Tree` also offers `Reverse()`:

```go
tree := gobbtree.NewTree[string, int]()
tree.Insert("alpha", 1)
tree.Insert("beta", 2)

for key, value := range tree.All() {
    fmt.Println(key, value)
}
```

Ordering follows `cmp.Compare`: NaNs sort before other floating-point values, and signed zeros compare equal. Equal keys retain the original stored key. Keys and values are shallow copies.

### Concurrency

`Map` uses optimistic reads for built-in primitive word values and leaf read locks for other values. Writers lock leaves independently; structural changes coordinate splits and merges.

- Scans are ordered and weakly consistent during concurrent writes. Use `Snapshot()` for a consistent copy of the entries.
- Batch entries are individually consistent; a batch is not a transaction.
- Callbacks run under locks and must not reenter the same map. A callback panic releases locks; update callbacks store their result only after returning.
- Synchronizing a map does not synchronize mutable objects referenced by its values.

Do not modify a `Tree` while iterating over it.

## SIMD, written in Go

| Architecture | Search kernels |
|---|---|
| amd64 | AVX2 and AVX-512, with a 128-bit fallback |
| arm64 | 128-bit NEON |
| Other targets | Portable Go SIMD where available; scalar fallback |

Kernels use Go's `simd` and `simd/archsimd` APIs. Runtime CPU checks select a supported path; `gobbtree.SIMD()` returns the selected hardware status and width. A normal build works without the SIMD experiment. `GODEBUG=simd=0` selects the scalar fallback for validation.

## Reproduce the comparisons

Performance work compares the Go implementation with a pinned, optimized Rust build, plus Go's built-in `map` and `sync.Map`. Results depend on the workload, entry count, CPU, and concurrency level; the project does not yet meet its performance target in every case.

Build the drivers from the repository root:

```sh
GOEXPERIMENT=simd go build -o compare-go ./cmd/compare
GOEXPERIMENT=simd go build -o memory-go ./cmd/memory
RUSTFLAGS='-C target-cpu=native' cargo build --release --locked \
  --manifest-path internal/oracle/Cargo.toml --bin compare --bin memory
```

Run the **50-million-entry** CPU comparison:

```sh
python3 internal/cpu_compare.py \
  --go ./compare-go --rust internal/oracle/target/release/compare \
  --size 50000000 --queries 1048576 --rounds 4 --threads 8 --repeat 3 \
  --standard
```

Each backend runs in a fresh process. The drivers use `uint64`/`u64` keys and values. Go and Rust receive identical keys, query streams, and operation counts; the driver checks checksums and rotates execution order. Parallel timings exclude worker creation and final checksum reduction. Built-in `map` is read directly for read-only tests and protected by `RWMutex` for concurrent writes.

Measure **real process memory**, separately from CPU timings:

```sh
python3 internal/rss_compare.py \
  --go ./memory-go --rust internal/oracle/target/release/memory \
  --size 50000000 --repeat 3
```

The memory driver supports Linux and macOS. It reports peak RSS, RSS after construction, and RSS after Go collection or a Rust settling interval. Each process contains one populated container. Go's runtime and GC headroom remain in the totals; Go heap and scan counters are supplementary. Construction time is recorded as `build_ns`. Set `--size 5000000` for the 5M case.

Built-in integer keys with primitive word values use indexed, GC-noscan slabs in both containers. The concurrent numeric map stores entries once and reads shared numeric fields atomically. Pointer-bearing keys and values remain visible to Go's garbage collector.

CI artifacts include CPU details, selected SIMD backends, generated code, and benchmark results.

## Validation and upstream

```sh
GOEXPERIMENT=simd go test -race ./...
GOEXPERIMENT=simd GODEBUG=simd=0 go test ./...
go test ./...
GOEXPERIMENT=simd go test -run '^$' -fuzz FuzzTree -fuzztime 20s
```

Tests cover randomized operations against independent Go references, tree invariants, concurrent mutation, callback panic recovery, SIMD boundaries, and steady-state allocations. CI also runs a live Rust oracle for numeric keys and byte strings, checking return values, ranges in both directions, lengths, and deletion to empty.

The upstream reference is pinned to [`8adae97`](https://github.com/XiangpengHao/bbtree/tree/8adae97a0ed97e5c98fa2790312eb0aec9719846). This port covers the in-memory ordered maps. Rust borrowing and allocator APIs, custom `InlineKey` strategies, sets, persistence, mmap, and the separate store crate are outside the current scope.

Licensed under **MIT OR Apache-2.0**, following upstream. See [LICENSE](LICENSE).
