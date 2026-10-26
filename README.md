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

## Benchmarks

Apple M4 Max (ARM64), Go 1.27.3 with Go SIMD, and Rust 1.97.1 with native CPU tuning, full LTO, and mimalloc. Measured sources: [Go `55907ff`](https://github.com/thesyncim/gobbtree/commit/55907ff08815d434de96af13b89e75e603680dd5) and [Rust `8adae97`](https://github.com/XiangpengHao/bbtree/tree/8adae97a0ed97e5c98fa2790312eb0aec9719846). Timings and memory figures are medians of three fresh-process runs using identical datasets and checked results. 4K and 262K denote 4,096 and 262,144 entries.

### Operation time

![Go versus Rust speed ratios across four sizes and eight workloads](benchmarks/cpu-arm64.svg)

The chart shows **Rust time / Go time**: above 1× favors Go; below 1× favors Rust. Batch timings are per key. Delete + insert is one pair. Parallel cases use four workers; the mixed case replaces existing values on 10% of operations.

<details>
<summary>Exact timings — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 12.73 / 12.97 | 17.86 / 23.92 | 53.65 / 92.15 | 99.66 / 120.78 |
| Read miss | 12.47 / 12.32 | 16.67 / 22.30 | 41.91 / 75.36 | 81.23 / 113.46 |
| Batch hit · 8 keys | 12.20 / 12.62 | 15.35 / 23.11 | 42.54 / 53.10 | 72.06 / 72.82 |
| Batch miss · 8 keys | 12.28 / 12.50 | 15.15 / 20.75 | 36.16 / 42.65 | 63.88 / 58.68 |
| Replace value | 21.28 / 13.82 | 33.45 / 27.39 | 106.89 / 94.83 | 238.76 / 133.85 |
| Delete + insert | 87.02 / 80.48 | 123.14 / 114.92 | 266.33 / 251.34 | 498.36 / 348.67 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.34 / 13.97 | 14.65 / 26.84 | 81.87 / 89.61 | 135.41 / 109.96 |
| Read miss | 10.03 / 13.28 | 13.12 / 24.07 | 59.08 / 66.07 | 108.38 / 93.91 |
| Batch hit · 8 keys | 9.66 / 14.24 | 13.58 / 22.95 | 61.70 / 55.08 | 83.82 / 70.89 |
| Batch miss · 8 keys | 9.54 / 13.22 | 12.04 / 20.85 | 50.02 / 43.36 | 69.64 / 58.07 |
| Replace value | 18.89 / 15.24 | 29.27 / 29.48 | 108.31 / 107.70 | 161.85 / 150.74 |
| Delete + insert | 105.68 / 77.66 | 127.25 / 110.62 | 263.26 / 249.74 | 448.60 / 324.19 |
| 4 workers · reads | 2.69 / 4.71 | 3.79 / 7.26 | 23.97 / 22.76 | 33.67 / 34.75 |
| 4 workers · 10% writes | 7.35 / 7.75 | 5.33 / 8.97 | 25.35 / 24.07 | 34.94 / 38.66 |

</details>

### Memory and construction

![Go versus Rust peak RSS and construction time for Tree and Map at four sizes](benchmarks/memory-arm64.svg)

Memory includes each process’s runtime, allocator, GC headroom, and caches populated by a full read pass. Go uses its default GC settings. Construction measures inserting all entries into an empty container.

<details>
<summary>Memory and build measurements — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.60 / 2.15 | 4.78 / 2.13 | 5.60 / 2.13 | 0.0006 / 0.0002 |
| Tree | 262K | 11.67 / 9.03 | 11.27 / 9.01 | 11.67 / 9.01 | 0.0354 / 0.0190 |
| Tree | 5M | 114.84 / 136.45 | 114.84 / 136.43 | 114.84 / 136.43 | 1.0365 / 0.7528 |
| Tree | 50M | 1,091.19 / 1,347.78 | 1,091.14 / 1,347.76 | 1,091.19 / 1,347.76 | 18.4510 / 12.2963 |
| Map | 4K | 5.59 / 2.15 | 4.83 / 2.13 | 5.59 / 2.13 | 0.0007 / 0.0002 |
| Map | 262K | 17.87 / 9.03 | 17.79 / 9.01 | 17.87 / 9.01 | 0.0463 / 0.0288 |
| Map | 5M | 117.44 / 136.48 | 117.34 / 136.46 | 117.44 / 136.46 | 1.2258 / 0.7523 |
| Map | 50M | 1,091.94 / 1,348.17 | 1,091.91 / 1,348.16 | 1,091.94 / 1,348.16 | 19.4369 / 11.9766 |

</details>

Background CPU activity was observed on the host; small timing gaps are inconclusive. These are ARM64 measurements. Native amd64 results are available as [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml). The [raw report](benchmarks/arm64-2026-10-26.json) also includes Go’s built-in `map` and `sync.Map` CPU comparisons through 5M entries.

## Reproduce the comparisons

Performance work compares the Go implementation with a pinned, optimized Rust build, plus Go's built-in `map` and `sync.Map`. Results depend on the workload, entry count, CPU, and concurrency level.

To reproduce the recorded results, use source checkpoint `55907ff` in a separate checkout. Build the drivers from its repository root:

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
  --size 50000000 --queries 1048576 --rounds 4 --threads 4 --repeat 3 \
  --mutations
```

Each backend runs in a fresh process. The drivers use `uint64`/`u64` keys and values. Rust uses the pinned upstream benchmark’s mimalloc allocator, full LTO, and native CPU tuning; Go uses its normal allocator and GC settings. Go and Rust receive identical keys, query streams, and operation counts; the driver checks checksums and rotates execution order. Eight-key batches use each library’s public batch API. `--mutations` includes existing-key replacements and delete/insert pairs after the read cases. Parallel timings exclude worker creation and final checksum reduction. Add `--standard` to compare Go's built-in `map` and `sync.Map`; built-in `map` is read directly for read-only tests and protected by `RWMutex` for concurrent writes.

For 5M entries, change `--size` to `5000000`. For the smaller cases, use `--size 4096` or `--size 262144` with `--queries 8192 --rounds 512`.

At the recorded checkpoint, serial measurements keep the caller on one OS thread. The Go driver sets its worker limit before construction, using the requested parallel worker count, or one for serial-only runs. The output records this setting.

Measure **real process memory**, separately from CPU timings:

```sh
python3 internal/rss_compare.py \
  --go ./memory-go --rust internal/oracle/target/release/memory \
  --size 50000000 --repeat 3 --kinds tree map
```

The memory driver supports Linux and macOS. It reports peak RSS, RSS after construction, after reading every entry once, and after Go collection or a Rust settling interval. The full read pass includes any lazily allocated read caches in the memory totals. Each process contains one populated container. Go's runtime and GC headroom remain in the totals. Run `./memory-go -stats` separately for supplementary Go heap and scan counters. Construction time is recorded as `build_ns`. Set `--size 5000000` for the 5M case.

Built-in integer keys with primitive word values use indexed, GC-noscan slabs in both containers. The concurrent numeric map reads shared numeric fields atomically. Small maps with stable keys can retain an immutable read cache; large maps use a single copy of their entries. Pointer-bearing keys and values remain visible to Go's garbage collector.

CI artifacts include CPU details, selected SIMD backends, generated code, and benchmark results.

Regenerate the SVG charts and tables from the saved report with `python3 internal/render_benchmarks.py` (requires Matplotlib).

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
