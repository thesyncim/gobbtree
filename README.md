# gobbtree

**Fast ordered maps and sets for Go: a B+ tree that searches with SIMD.**

[![Tests](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml)
[![amd64 SIMD](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml)

A Go port of [XiangpengHao/bbtree](https://github.com/XiangpengHao/bbtree). Nodes keep compact inline summaries of their keys and search them with vector instructions, so most lookups never touch a full key.

- **Fast.** It beats the Rust original on most read, batch and churn workloads. [Benchmarks](#benchmarks) cover every workload, including the ones where Rust still wins.
- **Concurrent.** `Map` has lock-free lookups for numeric values and per-leaf locking everywhere else.
- **Any ordered key.** It supports every `cmp.Ordered` type, plus your own types through a two-method `Key` interface.
- **On disk.** Write a snapshot, then query it in place with `mmap`.
- **Pure Go.** No assembly, cgo or dependencies. SIMD comes from Go's `simd` package (Go 1.27).

## Quick start

```sh
go get github.com/thesyncim/gobbtree
```

```go
scores := gobbtree.New[uint64, string]() // concurrent; the zero value works too
scores.Insert(42, "answer")
scores.Insert(7, "lucky")

value, found := scores.Get(42) // "answer", true

for key, value := range scores.All() { // ascending: 7, then 42
    fmt.Println(key, value)
}

scores.Scan(0, 10, func(key uint64, value string) bool { // keys in [0, 10)
    return true // false stops the scan
})
```

Build with `GOEXPERIMENT=simd` to enable the vector search. Without it, the same code uses scalar search.

## Containers

| Type | Use it for |
|---|---|
| `Map[K, V]` · `New` | Concurrent readers and writers |
| `Tree[K, V]` · `NewTree` | A single owner; concurrent reads only while nobody writes |
| `Set[K]` · `NewSet` | A single-owner set with union, intersection, difference and subset tests |
| `MapOf`, `TreeOf`, `SetOf` | The same three containers, for [custom keys](#custom-keys) and `[]byte` keys (`Bytes`) |
| `FileMap[K, V]` | A read-only snapshot, searched in place on disk ([file maps](#file-maps)) |

All zero values are ready to use. Don't copy a container after first use.

| Operation | Returns |
|---|---|
| `Get(k)` · `Insert(k, v)` · `Delete(k)` | The value or previous value, and a found flag |
| `GetMany(keys, dst)` · `InsertMany(entries, dst)` | Per-key results, in caller-provided storage |
| `Scan(lo, hi, f)` · `ScanReverse(lo, hi, f)` | Visits `[lo, hi)` in order |
| `All()`, plus `Keys()` · `Values()` · `Reverse()` on Tree | Range-over-func iterators |
| `First()` · `Last()` · `PopFirst()` · `PopLast()` | The end entries |
| `Update` · `LoadOrStore` · `UpdateOrInsert` · `DeleteIf` | Atomic read-modify-write on `Map` |
| `LowerBound` · `UpperBound` · `SplitOff` · `Append` · `Retain` · `ExtractIf` · `Clone` | Ordered editing on `Tree` |
| `Snapshot()` | A consistent copy of a `Map`'s entries |

Keys follow `cmp.Compare`: NaN sorts first and `-0 == +0`. An equal key keeps the originally stored key. Keys and values are copied shallowly.

## Custom keys

Any type can be a key by implementing `Key`. `Compare` defines the order. `Window` exposes that order to the SIMD search.

```go
type Key[K any] interface {
    Compare(other K) int
    // Bytes offset..offset+8 of an encoding that sorts like Compare,
    // big-endian and zero past its end.
    Window(offset int) uint64
}
```

For example, to order names case-insensitively:

```go
type Tag string

func (t Tag) Compare(o Tag) int {
    return strings.Compare(strings.ToLower(string(t)), strings.ToLower(string(o)))
}

func (t Tag) Window(offset int) uint64 {
    return gobbtree.StringWindow(strings.ToLower(string(t)), offset)
}

tags := gobbtree.NewMapOf[Tag, int]()
tags.Insert("Go", 1)
tags.Insert("GO", 2) // same key: replaces the value, keeps "Go"
```

Byte-slice keys are built in as `gobbtree.Bytes`:

```go
index := gobbtree.NewMapOf[gobbtree.Bytes, int]()
buf := []byte("user:42")
index.Insert(buf, 1)     // stores a copy: buf may be reused
v, ok := index.Get(buf)  // lookups never allocate or retain buf
```

A key type whose values refer to reusable memory implements `Clone() K`, and containers store a clone of each new key. Equal keys must have equal windows.

A composite key encodes its fields one after another. For example, a `uint32` tenant becomes four big-endian bytes, followed by a name. `StringWindow`, `BytesWindow` and `Uint64Window` window the common encodings. Windows that tie are resolved by `Compare`, so even a constant `Window` is correct, just slower. This is the Go counterpart of upstream's `InlineKey` trait.

## Concurrency

`Map` lookups of word values (integers, floats, bools) are lock-free: they read numeric slabs or immutable search images and retry if a writer interfered. Other values are read under a leaf read latch, so their writes publish no snapshots and allocate nothing. Writers lock single leaves; splits, merges and scans take a structural lock.

- Scans are ordered but only weakly consistent under concurrent writes. Use `Snapshot()` when you need a consistent copy.
- Each batch entry is consistent on its own; a batch is not a transaction.
- Callbacks run under locks and must not call back into the same map. A panicking callback releases its locks and stores nothing.

## File maps

```go
if err := tree.WriteFile("index.bbt"); err != nil { ... }

disk, err := gobbtree.MmapFile[string, int]("index.bbt") // or ReadFile
if err != nil { ... }
defer disk.Close()
value, found := disk.Get("alpha")
```

A file map validates the image and searches it in place, with no rebuild. It supports `Get`, `GetMany`, iteration in both directions and bounded scans. Files use upstream's version 1 format, which the Rust oracle tests in both directions.

- **Keys:** integer and string keys.
- **Values:** numbers, plain arrays, empty structs, strings and byte slices.
- **Writing:** `CreateFile` builds a file from an ascending `iter.Seq2`. `NewFileWriter`, `WriteAt`, `ReadFileRange` and `MmapFileRange` handle snapshots embedded in larger files.
- **Immutability:** to change a file map, write a new snapshot; `WriteFile` replaces the file atomically.

## SIMD

| Target | Search kernels |
|---|---|
| amd64 | AVX-512 or AVX2, with a 128-bit fallback |
| arm64 | NEON |
| Others | Portable Go SIMD where available, otherwise scalar |

The kernel is chosen at run time. `gobbtree.SIMD()` reports the choice, and `GODEBUG=simd=0` forces the scalar path.

## Benchmarks

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.97.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max. Sources: [Go `47c9c2d`](https://github.com/thesyncim/gobbtree/commit/47c9c2d64d357ee821868dea036d49188dc6e55d), [Rust `8adae97`](https://github.com/XiangpengHao/bbtree/tree/8adae97a0ed97e5c98fa2790312eb0aec9719846). `uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results.

![Change in Go operation time versus Rust across four sizes and multiple workloads](benchmarks/cpu-arm64.svg)

Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. Parallel rows use four workers; the mixed row replaces values on 10% of operations.

<details>
<summary>Exact timings — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 11.62 / 12.64 | 16.31 / 23.79 | 47.89 / 91.50 | 89.23 / 136.91 |
| Read miss | 10.64 / 12.17 | 14.79 / 22.77 | 38.01 / 72.67 | 72.16 / 130.51 |
| Batch hit · 8 keys | 10.31 / 12.57 | 13.61 / 22.91 | 38.40 / 53.86 | 62.29 / 81.24 |
| Batch miss · 8 keys | 10.30 / 12.23 | 12.84 / 20.48 | 32.05 / 45.38 | 55.12 / 69.19 |
| Replace value | 15.91 / 13.12 | 26.23 / 26.75 | 99.20 / 90.33 | 174.51 / 150.88 |
| Delete + reinsert same key | 45.99 / 75.62 | 69.81 / 116.80 | 180.70 / 266.03 | 360.41 / 404.01 |
| Delete + insert new key | 90.38 / 99.64 | 122.69 / 152.29 | 283.43 / 392.42 | 566.93 / 686.11 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.26 / 14.03 | 14.36 / 27.06 | 77.46 / 90.34 | 124.27 / 116.49 |
| Read miss | 9.55 / 12.79 | 12.86 / 24.16 | 51.66 / 67.53 | 95.66 / 103.94 |
| Batch hit · 8 keys | 8.89 / 13.58 | 13.03 / 23.17 | 49.69 / 55.62 | 87.37 / 78.28 |
| Batch miss · 8 keys | 9.23 / 12.56 | 11.93 / 20.58 | 38.50 / 44.58 | 66.34 / 66.05 |
| Replace value | 16.97 / 14.84 | 30.07 / 30.10 | 109.87 / 111.80 | 158.72 / 156.13 |
| Delete + reinsert same key | 83.86 / 75.03 | 112.90 / 115.75 | 273.58 / 253.67 | 384.15 / 353.74 |
| Delete + insert new key | 119.90 / 105.58 | 149.90 / 149.06 | 371.07 / 380.30 | 639.59 / 545.36 |
| 4 workers · reads | 2.42 / 4.28 | 3.71 / 7.31 | 20.77 / 22.65 | 30.97 / 32.88 |
| 4 workers · 10% writes | 6.30 / 6.21 | 5.63 / 9.10 | 22.22 / 24.36 | 32.80 / 35.67 |

</details>

![Go versus Rust peak RSS for Tree and Map at four sizes](benchmarks/memory-arm64.svg)

![Go versus Rust construction time for Tree and Map at four sizes](benchmarks/build-arm64.svg)

Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. Construction inserts every entry into an empty container.

<details>
<summary>Memory and build measurements — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.64 / 2.16 | 4.67 / 2.15 | 5.64 / 2.15 | 0.0004 / 0.0003 |
| Tree | 262K | 12.12 / 9.06 | 11.71 / 9.04 | 12.12 / 9.04 | 0.0279 / 0.0275 |
| Tree | 5M | 123.44 / 136.48 | 123.26 / 136.46 | 123.44 / 136.46 | 0.7032 / 0.7802 |
| Tree | 50M | 1,180.83 / 1,347.81 | 1,180.75 / 1,347.80 | 1,180.83 / 1,347.80 | 13.5419 / 13.8490 |
| Map | 4K | 5.77 / 2.16 | 4.72 / 2.15 | 5.77 / 2.15 | 0.0004 / 0.0003 |
| Map | 262K | 18.92 / 9.06 | 18.78 / 9.04 | 18.92 / 9.04 | 0.0277 / 0.0269 |
| Map | 5M | 127.53 / 136.51 | 127.47 / 136.50 | 127.50 / 136.50 | 0.8799 / 0.7949 |
| Map | 50M | 1,196.28 / 1,348.22 | 1,196.23 / 1,348.21 | 1,196.28 / 1,348.21 | 15.2331 / 14.3491 |

</details>

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-26.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

To reproduce, use source checkpoint `47c9c2d` and build both drivers:

```sh
GOEXPERIMENT=simd go build -o compare-go ./cmd/compare
GOEXPERIMENT=simd go build -o memory-go ./cmd/memory
RUSTFLAGS='-C target-cpu=native' cargo build --release --locked \
  --manifest-path internal/oracle/Cargo.toml --bin compare --bin memory
```

Run the CPU comparison (shown for 50M entries; for 4K and 262K use `--queries 8192 --rounds 512`):

```sh
python3 internal/cpu_compare.py --go ./compare-go --rust internal/oracle/target/release/compare \
  --size 50000000 --queries 1048576 --rounds 4 --threads 4 --repeat 3 --mutations --distinct
```

Then the memory and construction comparison:

```sh
python3 internal/rss_compare.py --go ./memory-go --rust internal/oracle/target/release/memory \
  --size 50000000 --repeat 3 --kinds tree map
```

`python3 internal/render_benchmarks.py` regenerates the charts and the tables above from the saved report.

How the comparison is kept fair:

- **Identical work.** Both languages get identical keys, queries and operation counts. Checksums are verified, including every bit of replaced values. Different-key churn uses disjoint key sets and checks that every delete and insert succeeds.
- **Fresh processes.** Every backend runs in a fresh process, in rotated order. Serial cases are pinned to one OS thread. Parallel timings exclude starting workers and reducing checksums.
- **Public APIs.** Batches go through each library's public batch API, and the Rust code is unmodified upstream.
- **Unequal guarantees.** The concurrent APIs promise different things. Rust omits `len()` and reads payloads with plain loads ([design](https://github.com/XiangpengHao/bbtree/blob/8adae97a0ed97e5c98fa2790312eb0aec9719846/design/09-concurrency.md)). Go keeps an exact O(1) `Len` and reads numeric payloads atomically.
- **Whole-process memory.** Memory is RSS of a process holding one container, measured after a full read pass, with Go's default GC settings.

`--standard` adds Go's built-in `map` and `sync.Map`.

## Testing

```sh
GOEXPERIMENT=simd go test -race ./...
GOEXPERIMENT=simd GODEBUG=simd=0 go test ./...
GOEXPERIMENT=simd go test -run '^$' -fuzz FuzzTree -fuzztime 20s
```

The tests check every container against independent references:

- randomized operation sequences with tree invariants;
- concurrent mutation and callback panics;
- SIMD boundaries and steady-state allocations;
- custom key orders.

CI also runs a live Rust oracle for containers and file interoperability, against upstream pinned at [`8adae97`](https://github.com/XiangpengHao/bbtree/tree/8adae97a0ed97e5c98fa2790312eb0aec9719846).

Licensed under **MIT OR Apache-2.0**, like upstream. See [LICENSE](LICENSE).
