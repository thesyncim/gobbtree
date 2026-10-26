# gobbtree

**Ordered maps and sets for Go, with SIMD search and range scans.**

A Go port of [XiangpengHao/bbtree](https://github.com/XiangpengHao/bbtree), built for **Go 1.27.x**. Generic keys and values, a concurrent map, a single-owner tree and set, and read-only file maps. Pure Go: no handwritten assembly, cgo, or external runtime dependencies.

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
| Conditional updates | `Update`, `LoadOrStore` | `Update`, `UpdateOrInsert`, `LoadOrStore`, `DeleteIf` |

Both zero values are ready to use. Keys support Go's ordered types, including strings and defined numeric types; values can be any type. Do not copy a container after first use.

`NewSet[K]()` provides the single-owner ordered set from upstream: insertion, removal, batch membership, range scans, union, intersection, difference, and subset checks. It also supports cloning, splitting, appending, filtering and file snapshots. `Tree` offers `LowerBound`, `UpperBound`, `GetKeyValue`, `SplitOff`, `Append`, `Retain`, and lazy `ExtractIf`.

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

## File maps

Write a snapshot, then query its pages directly from disk:

```go
if err := tree.WriteFile("index.bbt"); err != nil {
    panic(err)
}
disk, err := gobbtree.MmapFile[string, int]("index.bbt")
if err != nil {
    panic(err)
}
defer disk.Close()
value, found := disk.Get("alpha")
```

`ReadFile` loads the same pages into owned memory. Both paths validate the image and search it in place, without reconstructing a `Tree`. File maps support `GetMany`, `GetKeyValue`, forward and reverse iteration, and bounded scans. Returned values remain valid after `Close`.

Files use the pinned Rust reference's version 1 format, checked in both directions by the Rust oracle. Integer and string keys, and numeric, plain-array, empty-struct, string and byte-slice values are supported on 64-bit targets. Match the Rust key/value types and platform layout; Go strings use Rust's `VarBytes` representation. Floating-point keys, pointer types, and nonempty structs are rejected. `mmap` is available on Linux, macOS and the BSDs; `ReadFile` also works elsewhere.

`CreateFile(path, entries)` accepts an ascending `iter.Seq2`. `NewFileWriter` builds a reusable image; `WriteAt`, `ReadFileRange`, and `MmapFileRange` support embedded snapshots. `WriteFile` publishes a complete replacement by rename. A file map is immutable: publish a new snapshot to change it, and do not modify or truncate a mapped file.

## SIMD, written in Go

| Architecture | Search kernels |
|---|---|
| amd64 | AVX2 and AVX-512, with a 128-bit fallback |
| arm64 | 128-bit NEON |
| Other targets | Portable Go SIMD where available; scalar fallback |

Kernels use Go's `simd` and `simd/archsimd` APIs. Runtime CPU checks select a supported path; `gobbtree.SIMD()` returns the selected hardware status and width. A normal build works without the SIMD experiment. `GODEBUG=simd=0` selects the scalar fallback for validation.

## Benchmarks

Apple M4 Max (ARM64), Go 1.27.3 with Go SIMD, and Rust 1.97.1 with native CPU tuning, full LTO, and mimalloc. Measured sources: [Go `2cbcaee`](https://github.com/thesyncim/gobbtree/commit/2cbcaee3e6a28b330184322b643998d61a289304) and [Rust `8adae97`](https://github.com/XiangpengHao/bbtree/tree/8adae97a0ed97e5c98fa2790312eb0aec9719846). Timings and memory figures are medians of three fresh-process runs using identical datasets and checked results. 4K and 262K denote 4,096 and 262,144 entries.

### Operation time

![Change in Go operation time versus Rust across four sizes and multiple workloads](benchmarks/cpu-arm64.svg)

Each cell shows the **percentage change in Go time versus Rust**. Negative means faster Go; positive means slower Go. Gray cells are within 5%. Batch timings are per key. Churn timings cover one deletion and one insertion; the two rows distinguish reinserting the same key from inserting a different key. Different-key churn uses unique keys spread throughout the tree and checks that every deletion succeeds and every insertion adds a new key. Parallel cases use four workers; the mixed case replaces existing values on 10% of operations.

<details>
<summary>Exact timings — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.58 / 12.51 | 16.50 / 23.46 | 48.03 / 82.75 | 78.24 / 109.30 |
| Read miss | 10.59 / 11.89 | 15.10 / 22.62 | 36.05 / 67.30 | 62.30 / 106.00 |
| Batch hit · 8 keys | 10.30 / 12.25 | 13.84 / 22.65 | 38.24 / 50.06 | 58.62 / 68.10 |
| Batch miss · 8 keys | 10.23 / 11.97 | 12.96 / 20.23 | 30.98 / 38.64 | 48.89 / 56.53 |
| Replace value | 17.00 / 12.97 | 27.69 / 26.51 | 73.11 / 87.11 | 151.30 / 115.64 |
| Delete + reinsert same key | 45.42 / 74.50 | 68.60 / 113.40 | 156.53 / 237.84 | 271.95 / 315.85 |
| Delete + insert new key | 86.35 / 98.75 | 121.75 / 149.72 | 264.69 / 344.30 | 453.32 / 485.18 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.03 / 13.61 | 13.82 / 26.73 | 59.99 / 82.60 | 110.57 / 101.57 |
| Read miss | 9.39 / 12.30 | 12.65 / 23.85 | 43.63 / 61.53 | 81.12 / 91.41 |
| Batch hit · 8 keys | 8.72 / 13.44 | 12.74 / 22.58 | 43.39 / 51.60 | 74.00 / 69.29 |
| Batch miss · 8 keys | 9.44 / 12.40 | 11.37 / 20.12 | 34.02 / 39.45 | 59.89 / 56.54 |
| Replace value | 16.31 / 14.58 | 26.96 / 28.58 | 76.38 / 100.31 | 141.22 / 143.05 |
| Delete + reinsert same key | 82.51 / 74.11 | 106.24 / 112.27 | 214.55 / 236.18 | 336.01 / 312.29 |
| Delete + insert new key | 108.83 / 103.73 | 140.69 / 146.26 | 334.70 / 337.43 | 554.24 / 490.17 |
| 4 workers · reads | 2.41 / 3.83 | 3.80 / 7.09 | 17.61 / 23.23 | 31.65 / 33.24 |
| 4 workers · 10% writes | 4.62 / 4.84 | 4.43 / 6.99 | 19.75 / 24.98 | 33.13 / 38.01 |

</details>

### Memory and construction

![Go versus Rust peak RSS for Tree and Map at four sizes](benchmarks/memory-arm64.svg)

Memory includes each process’s runtime, allocator, GC headroom, and caches populated by a full read pass. Go uses its default GC settings. MB are decimal.

![Go versus Rust construction time for Tree and Map at four sizes](benchmarks/build-arm64.svg)

Construction measures inserting all entries into an empty container. Small cases use milliseconds; large cases use seconds.

<details>
<summary>Memory and build measurements — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.69 / 2.16 | 4.82 / 2.15 | 5.69 / 2.15 | 0.0005 / 0.0004 |
| Tree | 262K | 11.76 / 9.06 | 11.21 / 9.04 | 11.76 / 9.04 | 0.0318 / 0.0277 |
| Tree | 5M | 114.88 / 136.48 | 114.82 / 136.46 | 114.88 / 136.46 | 0.6385 / 0.6472 |
| Tree | 50M | 1,090.83 / 1,347.81 | 1,090.80 / 1,347.80 | 1,090.83 / 1,347.80 | 11.2209 / 10.3513 |
| Map | 4K | 5.77 / 2.16 | 4.93 / 2.15 | 5.77 / 2.15 | 0.0006 / 0.0004 |
| Map | 262K | 17.78 / 9.06 | 17.61 / 9.04 | 17.78 / 9.04 | 0.0343 / 0.0283 |
| Map | 5M | 117.26 / 136.51 | 117.19 / 136.50 | 117.26 / 136.50 | 0.7694 / 0.6574 |
| Map | 50M | 1,090.62 / 1,348.21 | 1,090.58 / 1,348.19 | 1,090.62 / 1,348.19 | 13.0186 / 10.3467 |

</details>

Background CPU activity was observed on the host; small timing gaps are inconclusive. These are ARM64 measurements. Native amd64 results are available as [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml). The [raw report](benchmarks/arm64-2026-10-26.json) also includes Go’s built-in `map` and `sync.Map` CPU comparisons through 5M entries.

## Reproduce the comparisons

Performance work compares the Go implementation with a pinned, optimized Rust build, plus Go's built-in `map` and `sync.Map`. Results depend on the workload, entry count, CPU, and concurrency level.

To reproduce the recorded results, use source checkpoint `2cbcaee` in a separate checkout. Build the drivers from its repository root:

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
  --mutations --distinct
```

Each backend runs in a fresh process. The drivers use `uint64`/`u64` keys and values. Rust uses the pinned upstream benchmark’s mimalloc allocator, full LTO, and native CPU tuning; Go uses its normal allocator and GC settings.

Go and Rust receive identical keys, query streams, and operation counts; the driver checks checksums and rotates execution order. Eight-key batches use each library’s public batch API.

Serial replacement checks include every bit of the previous value, independently account for repeated queries, and verify final values outside the timer.

The concurrent APIs have different costs: Rust deliberately omits `len()` and uses optimistic ordinary payload reads, as described in its [concurrency design](https://github.com/XiangpengHao/bbtree/blob/8adae97a0ed97e5c98fa2790312eb0aec9719846/design/09-concurrency.md). Go maintains exact O(1) `Len` and uses atomic numeric payload accesses. The pinned Rust implementation is unmodified.

`--mutations` includes existing-key replacements and same-key delete/reinsert pairs after the read cases. `--distinct` adds successful deletion followed by insertion of a different key, using unique, disjoint key sets; operation counts and expected checksums are checked independently.

Parallel timings exclude worker creation and final checksum reduction. Add `--standard` to compare Go's built-in `map` and `sync.Map`; built-in `map` is read directly for read-only tests and protected by `RWMutex` for concurrent writes.

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

The upstream reference is pinned to [`8adae97`](https://github.com/XiangpengHao/bbtree/tree/8adae97a0ed97e5c98fa2790312eb0aec9719846). The test suite includes Rust-generated file fixtures and optional live Rust oracles for container operations and file interoperability.

Licensed under **MIT OR Apache-2.0**, following upstream. See [LICENSE](LICENSE).
