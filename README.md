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

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.97.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max. Sources: [Go `e456e10`](https://github.com/thesyncim/gobbtree/commit/e456e10653b71fa1c80cebe467649bbf63baa6c7), [Rust `8adae97`](https://github.com/XiangpengHao/bbtree/tree/8adae97a0ed97e5c98fa2790312eb0aec9719846). `uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results.

![Change in Go operation time versus Rust across four sizes and multiple workloads](benchmarks/cpu-arm64.svg)

Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. Parallel rows use four workers; the mixed row replaces values on 10% of operations.

<details>
<summary>Exact timings — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.67 / 12.96 | 16.96 / 24.95 | 55.01 / 98.06 | 97.20 / 133.91 |
| Read miss | 10.99 / 12.31 | 15.17 / 23.64 | 42.93 / 89.22 | 79.73 / 128.53 |
| Batch hit · 8 keys | 10.42 / 12.69 | 14.30 / 23.62 | 42.90 / 60.35 | 69.58 / 84.37 |
| Batch miss · 8 keys | 10.26 / 12.37 | 13.35 / 21.27 | 36.85 / 50.16 | 60.66 / 70.61 |
| Replace value | 16.31 / 13.37 | 27.73 / 27.98 | 116.55 / 102.10 | 176.00 / 149.35 |
| Delete + reinsert same key | 46.69 / 79.12 | 74.56 / 123.84 | 218.00 / 298.02 | 381.92 / 395.92 |
| Delete + insert new key | 91.07 / 102.32 | 131.42 / 163.34 | 327.87 / 436.94 | 595.17 / 652.68 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.21 / 14.06 | 14.75 / 28.46 | 69.64 / 97.18 | 109.66 / 129.29 |
| Read miss | 9.16 / 12.80 | 13.20 / 25.23 | 53.35 / 76.62 | 85.86 / 121.06 |
| Batch hit · 8 keys | 8.81 / 14.02 | 13.57 / 23.92 | 56.00 / 61.48 | 87.10 / 84.50 |
| Batch miss · 8 keys | 9.07 / 12.85 | 11.91 / 21.30 | 44.89 / 51.68 | 74.63 / 73.00 |
| Replace value | 17.46 / 15.09 | 33.80 / 31.66 | 119.37 / 121.99 | 161.73 / 170.42 |
| Delete + reinsert same key | 84.99 / 77.86 | 119.79 / 121.99 | 292.22 / 291.87 | 444.40 / 399.79 |
| Delete + insert new key | 121.31 / 107.50 | 165.14 / 161.39 | 433.46 / 430.02 | 693.46 / 624.74 |
| 4 workers · reads | 2.40 / 4.69 | 3.68 / 7.47 | 17.63 / 25.15 | 25.70 / 34.03 |
| 4 workers · 10% writes | 7.07 / 6.51 | 6.00 / 9.26 | 20.51 / 27.04 | 28.17 / 37.06 |

</details>

![Go versus Rust peak RSS for Tree and Map at four sizes](benchmarks/memory-arm64.svg)

![Go versus Rust construction time for Tree and Map at four sizes](benchmarks/build-arm64.svg)

Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. Construction inserts every entry into an empty container.

<details>
<summary>Memory and build measurements — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.67 / 2.16 | 4.90 / 2.15 | 5.67 / 2.15 | 0.0005 / 0.0003 |
| Tree | 262K | 12.14 / 9.06 | 11.63 / 9.04 | 12.14 / 9.04 | 0.0300 / 0.0270 |
| Tree | 5M | 123.52 / 136.48 | 123.45 / 136.46 | 123.52 / 136.46 | 0.7304 / 0.7916 |
| Tree | 50M | 1,180.73 / 1,347.81 | 1,180.68 / 1,347.80 | 1,180.73 / 1,347.80 | 13.0852 / 13.6610 |
| Map | 4K | 5.75 / 2.16 | 4.87 / 2.15 | 5.75 / 2.15 | 0.0003 / 0.0003 |
| Map | 262K | 18.91 / 9.06 | 18.76 / 9.04 | 18.91 / 9.04 | 0.0320 / 0.0303 |
| Map | 5M | 127.47 / 136.51 | 127.43 / 136.50 | 127.47 / 136.50 | 0.8912 / 0.8220 |
| Map | 50M | 1,196.23 / 1,348.21 | 1,196.15 / 1,348.19 | 1,196.23 / 1,348.19 | 15.1163 / 14.3175 |

</details>

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-26.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

To reproduce, use source checkpoint `e456e10` and build both drivers:

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
