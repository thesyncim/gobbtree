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

Apple M4 Max (ARM64), Go 1.27.3 with Go SIMD, and Rust 1.97.1 with native CPU tuning, full LTO, and mimalloc. Measured sources: [Go `c156788`](https://github.com/thesyncim/gobbtree/commit/c1567888316ed3a0c2ff4e7895fddc6eb1558ad3) and [Rust `8adae97`](https://github.com/XiangpengHao/bbtree/tree/8adae97a0ed97e5c98fa2790312eb0aec9719846). Timings and memory figures are medians of three fresh-process runs using identical datasets and checked results. 4K and 262K denote 4,096 and 262,144 entries.

### Operation time

![Change in Go operation time versus Rust across four sizes and multiple workloads](benchmarks/cpu-arm64.svg)

Each cell shows the **percentage change in Go time versus Rust**. Negative means faster Go; positive means slower Go. Gray cells are within 5%. Batch timings are per key. Churn timings cover one deletion and one insertion; the two rows distinguish reinserting the same key from inserting a different key. Different-key churn uses unique keys spread throughout the tree and checks that every deletion succeeds and every insertion adds a new key. Parallel cases use four workers; the mixed case replaces existing values on 10% of operations.

<details>
<summary>Exact timings — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 11.20 / 11.77 | 17.11 / 21.68 | 49.16 / 78.53 | 82.24 / 114.06 |
| Read miss | 11.27 / 11.27 | 15.83 / 20.66 | 37.26 / 62.18 | 65.61 / 103.30 |
| Batch hit · 8 keys | 10.79 / 11.84 | 14.50 / 22.51 | 38.67 / 49.16 | 60.51 / 66.10 |
| Batch miss · 8 keys | 10.77 / 11.41 | 13.99 / 20.27 | 31.52 / 37.92 | 52.85 / 55.55 |
| Replace value | 17.24 / 12.51 | 27.43 / 24.95 | 68.53 / 80.13 | 154.02 / 111.15 |
| Delete + reinsert same key | 45.63 / 68.29 | 67.33 / 104.22 | 150.09 / 220.34 | 265.74 / 295.21 |
| Delete + insert new key | 92.20 / 88.39 | 126.22 / 133.15 | 266.25 / 326.29 | 455.29 / 473.79 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.54 / 12.78 | 13.56 / 25.34 | 73.55 / 77.23 | 121.71 / 101.66 |
| Read miss | 9.33 / 11.52 | 12.26 / 22.82 | 49.26 / 57.49 | 93.19 / 90.24 |
| Batch hit · 8 keys | 9.35 / 12.91 | 12.79 / 22.52 | 53.66 / 50.46 | 76.16 / 67.55 |
| Batch miss · 8 keys | 8.67 / 11.93 | 11.27 / 20.15 | 44.53 / 38.98 | 61.49 / 55.86 |
| Replace value | 17.45 / 14.20 | 28.99 / 28.20 | 89.49 / 95.89 | 153.82 / 143.63 |
| Delete + reinsert same key | 98.33 / 69.39 | 119.96 / 101.90 | 227.66 / 218.04 | 349.26 / 304.00 |
| Delete + insert new key | 128.19 / 93.68 | 162.90 / 131.10 | 377.19 / 316.76 | 605.62 / 470.66 |
| 4 workers · reads | 2.52 / 3.71 | 3.88 / 6.91 | 20.95 / 23.06 | 32.45 / 32.67 |
| 4 workers · 10% writes | 4.72 / 4.82 | 4.46 / 6.94 | 22.37 / 24.88 | 33.88 / 36.55 |

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
| Tree | 4K | 5.62 / 2.15 | 4.82 / 2.13 | 5.62 / 2.13 | 0.0009 / 0.0004 |
| Tree | 262K | 11.65 / 9.03 | 11.22 / 9.01 | 11.65 / 9.01 | 0.0396 / 0.0282 |
| Tree | 5M | 114.92 / 136.45 | 114.85 / 136.43 | 114.92 / 136.43 | 0.7649 / 0.6318 |
| Tree | 50M | 1,091.32 / 1,347.78 | 1,091.24 / 1,347.76 | 1,091.32 / 1,347.76 | 12.4979 / 10.1136 |
| Map | 4K | 5.82 / 2.15 | 4.92 / 2.13 | 5.82 / 2.13 | 0.0012 / 0.0004 |
| Map | 262K | 17.94 / 9.03 | 17.81 / 9.01 | 17.94 / 9.01 | 0.0468 / 0.0287 |
| Map | 5M | 117.54 / 136.48 | 117.42 / 136.46 | 117.54 / 136.46 | 0.9233 / 0.6369 |
| Map | 50M | 1,091.75 / 1,348.17 | 1,091.68 / 1,348.16 | 1,091.75 / 1,348.16 | 14.8154 / 10.0997 |

</details>

Background CPU activity was observed on the host; small timing gaps are inconclusive. These are ARM64 measurements. Native amd64 results are available as [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml). The [raw report](benchmarks/arm64-2026-10-26.json) also includes Go’s built-in `map` and `sync.Map` CPU comparisons through 5M entries.

## Reproduce the comparisons

Performance work compares the Go implementation with a pinned, optimized Rust build, plus Go's built-in `map` and `sync.Map`. Results depend on the workload, entry count, CPU, and concurrency level.

To reproduce the recorded results, use source checkpoint `c156788` in a separate checkout. Build the drivers from its repository root:

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
