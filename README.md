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

for key, value := range scores.Range(0, 10) { // keys in [0, 10)
    fmt.Println(key, value) // break stops early
}
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
| `Take(k)` | Removes `k`, returning the stored key and value |
| `GetMany(keys, dst)` · `InsertMany(entries, dst)` | Per-key results, in caller-provided storage |
| `Range(lo, hi)` · `RangeReverse(lo, hi)` | Iterates `[lo, hi)`, ascending or descending |
| `Scan(b)` · `ScanReverse(b)` | Iterates any `Bounds`: `Between(a, b)`, `AtLeast(a).AtMost(b)`, `Above(a)`, `Below(b)`, … |
| `Ascend(from)` · `Descend(from)` | Iterates keys `>= from` ascending, or `<= from` descending |
| `All()` · `Reverse()`, plus `Keys()` · `Values()` on Tree | Iterates everything |
| `First()` · `Last()` · `PopFirst()` · `PopLast()` | The end entries |
| `Update` · `LoadOrStore` · `LoadOrStoreFunc` · `UpdateOrInsert` | Read-modify-write; atomic on `Map`. Callbacks receive the stored key |
| `UpdateRange(b, f)` · `UpdateAll(f)` · `InsertAll(seq)` | Bulk changes |
| `Read` · `ReadMany` · `UpdateOrInsertMany` · `DeleteIf` | Callback reads, batched upserts and conditional delete on `Map` |
| `LowerBound` · `UpperBound` · `SplitOff` · `Append` · `Retain` · `ExtractIf(b, f)` | Ordered editing on `Tree` |
| `Entry(k)` · `FirstEntry()` · `LastEntry()` | Upstream's entry API on `Tree`: `OrInsert`, `OrInsertWith`, `AndModify`, `Remove`, … |
| `Clone()` · `EqualFunc` · `CompareFunc` · `String()` · `Drain()` | Copy, compare, format and consume (`Equal`/`Compare` on `Set`) |

Iterators are Go range-over-func sequences: `break` stops them. `Set` offers the same iterators over keys, plus union, intersection, difference and subset tests; `FileMap` offers the read-only ones.

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

`Map` lookups of word values (integers, floats, bools) are lock-free: they read numeric slabs or immutable search images and retry if a writer interfered. Other values are read under a leaf read latch, so their writes publish no snapshots and allocate nothing. Writers lock single leaves; splits and merges take a structural lock.

- Iteration (`Scan`, `Range`, `Ascend`, `Descend`, `All`, their reverses, `First`, `Last`) works like upstream's `scan` and `range_cloned`: it copies a few leaves at a time and visits the copies with nothing held, so the loop body may use the map. Numeric maps copy without any latch, validated like lookups; other maps hold a leaf's read latch only while copying it. Iteration is not a snapshot: an entry present throughout is visited exactly once; entries inserted or removed meanwhile may or may not be.
- Each batch entry is consistent on its own; a batch is not a transaction.
- `Read`, `Update`, `UpdateOrInsert`, `DeleteIf` and `LoadOrStore` run under the entry's leaf latch, as upstream's do; their callbacks must not call back into the same map. A panicking callback releases its locks and stores nothing.

## File maps

```go
if err := tree.WriteFile("index.bbt"); err != nil { ... }

disk, err := gobbtree.Mmap[string, int]("index.bbt").Open() // or ReadFile
if err != nil { ... }
defer disk.Close()
value, found := disk.Get("alpha")
```

A file map searches the image in place, with no rebuild. As upstream, `ReadFile`, `ReadFileRange` and `OpenFileMap` check every page; `Mmap(...).Open()` and `OpenFileMapUnchecked` check the header alone, so a corrupt file may give wrong answers or panic, but is never read out of bounds. It supports `Get`, `GetMany`, iteration in both directions and bounded scans. Files use upstream's version 1 format, which the Rust oracle tests in both directions.

- **Keys:** integer and string keys.
- **Values:** numbers, plain arrays, empty structs, strings and byte slices.
- **Writing:** `CreateFile` builds a file from an ascending `iter.Seq2`. `NewFileWriter`, `WriteAt`, `ReadFileRange` and `Mmap(...).Range` handle snapshots embedded in larger files; `Mmap(...).Pages(OsPagesHuge)` advises huge pages on Linux.
- **Immutability:** to change a file map, write a new snapshot; `WriteFile` replaces the file atomically.

## Coming from bbtree

The API follows upstream's operations with Go names; results are Go multi-value returns and iterators are range-over-func sequences.

| Upstream | gobbtree |
|---|---|
| `get` · `get_copied` · `get_cloned` · `contains_key` · `get_key_value` | `Get` · `Contains` · `GetKeyValue` |
| `insert` · `remove` · `remove_entry` | `Insert` · `Delete` · `Take` |
| `get_many_*` · `insert_many` · `read_many` · `update_or_insert_many` | `GetMany` · `InsertMany` · `ReadMany` · `UpdateOrInsertMany` |
| `read` · `update` · `update_or_insert` · `get_or_insert` · `remove_if` | `Read` · `Update` · `UpdateOrInsert` · `LoadOrStore` · `DeleteIf`; callbacks see the stored key, and a closure's result is captured by the closure |
| `get_mut` · `iter_mut` · `values_mut` · `range_mut` | `Update` · `UpdateAll` · `UpdateRange` |
| `entry` · `first_entry` · `last_entry` | `Entry` · `FirstEntry` · `LastEntry`, with `OrInsert` · `OrInsertWith` · `OrInsertWithKey` · `OrDefault` · `AndModify` · `Insert` · `InsertEntry` · `Remove` |
| Range bounds `a..b` · `a..=b` · `a..` · `..b` · `..=b` · `(Excluded(a), _)` | `Between(a, b)` · `AtLeast(a).AtMost(b)` · `AtLeast(a)` · `Below(b)` · `AtMost(b)` · `Above(a)…`; invalid bounds panic as upstream's do |
| `iter` · `range` · `range_cloned` · `scan` · `scan_rev` | `All` / `Reverse` · `Scan` / `ScanReverse` (any `Bounds`) · `Range` / `RangeReverse` / `Ascend` / `Descend` |
| `keys` · `values` · `first_key_value` · `last_key_value` · `first_copied` · `pop_first` · `pop_last` | `Keys` · `Values` · `First` · `Last` · `FirstIn` · `PopFirst` · `PopLast` |
| `retain` · `extract_if` · `split_off` · `append` · `Extend` · `FromIterator` | `Retain` · `ExtractIf` · `SplitOff` · `Append` · `InsertAll` |
| set `contains_many` · `replace` · `take` · `union` · `intersection` · `difference` · `symmetric_difference` · `is_subset` | the same names on `Set` |
| `Clone` · `PartialEq`/`Eq` · `PartialOrd`/`Ord` · `Debug` · `into_iter`/`into_keys`/`into_values` | `Clone` · `EqualFunc`/`Equal` · `CompareFunc`/`Compare` · `String` · `Drain` |
| `InlineKey` (`inline_key_at`) · byte-string keys, which compare from where they differ (`cmp_from`) | `Key` (`Window`) · `Bytes`; string and `Bytes` keys compare from where they differ |
| `FileMap` · `FileWriter::from_ascending` | `FileMap` · `NewFileWriter` / `CreateFile` |
| `FileMap::open` · `open_unchecked` · `NotAscending::index` | `OpenFileMap` · `OpenFileMapUnchecked` · `NotAscendingError.Index` |
| bbtree-mmap `create` · `write_at` · `read` · `read_range` · `mmap().range().pages().open()` | `CreateFile` · `WriteAt` · `ReadFile` · `ReadFileRange` · `Mmap().Range().Pages().Open()` |

Not carried over, because Go has no counterpart:
- **References into the map.** Go takes values out and puts them back (the `Update` family and `Entry`), so `get_mut`, `iter_mut` and `range_mut` become `Update`, `UpdateAll` and `UpdateRange`.
- **Rust-only traits.** There is no operator overloading for `Index` (use `Get`) and no `Hash` trait.
- **Allocators and `InsertError`.** Go's allocation failure is fatal, so `Insert` has no error path.
- **`NodeConfig` page sizes.** Go array lengths cannot be type parameters, so nodes hold a fixed 32 entries.
- **The rest of `InlineKey`:** `WIDTH`, `hash_tag`, `cmp_prefix`, and `cmp_from` on custom keys. These tune upstream's node layout, and a generic Go container cannot call an optional key method without boxing the key.
- **Leaf annexes and their `*_with_leaf` methods.** They let `bbtree-store` manage manually allocated pages, which Go's garbage collector manages here.
- **The `inspect` and `check_invariants` debugging helpers.**

Additions beyond upstream: `Len`, `First`, `Last`, `PopFirst`, `PopLast`, `Ascend` and `Descend` on `Map`; `LowerBound` and `UpperBound` on `Tree`; and `Range(lo, hi)` as shorthand for `Scan(Between(lo, hi))`.

## SIMD

| Target | Search kernels |
|---|---|
| amd64 | AVX-512 or AVX2, with a 128-bit fallback |
| arm64 | NEON |
| Others | Portable Go SIMD where available, otherwise scalar |

The kernel is chosen at run time. `gobbtree.SIMD()` reports the choice, and `GODEBUG=simd=0` forces the scalar path.

## Benchmarks

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.99.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max. Sources: [Go `63a05de`](https://github.com/thesyncim/gobbtree/commit/63a05dec75f243f08b3ed214777bce33ac4c63b3), [Rust `1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). `uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results. Every workload runs with three key patterns: **mixed keys**, hashed keys spread over the full 64-bit range; **sequential keys**, consecutive even keys; misses and fresh keys fill the gaps; **clustered keys**, runs of 1,024 consecutive even keys at scattered bases.

![Change in Go operation time versus Rust across four sizes and multiple workloads, mixed keys](benchmarks/cpu-arm64.svg)

Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. Parallel rows use four workers; the mixed row replaces values on 10% of operations.

<details>
<summary>Exact timings, mixed keys — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 11.17 / 13.13 | 16.36 / 24.52 | 42.37 / 86.97 | 80.12 / 131.03 |
| Read miss | 10.97 / 12.36 | 15.57 / 23.35 | 33.87 / 68.87 | 65.66 / 122.70 |
| Batch hit · 8 keys | 11.46 / 12.91 | 13.80 / 23.14 | 33.22 / 51.57 | 58.09 / 81.92 |
| Batch miss · 8 keys | 11.05 / 12.89 | 13.21 / 20.73 | 28.24 / 42.31 | 48.57 / 60.46 |
| Replace value | 18.13 / 13.55 | 29.43 / 26.66 | 92.60 / 89.78 | 152.22 / 127.84 |
| Delete + reinsert same key | 47.81 / 80.24 | 69.42 / 117.44 | 172.50 / 255.19 | 299.62 / 347.89 |
| Delete + insert new key | 92.08 / 104.05 | 121.82 / 155.51 | 260.28 / 362.58 | 471.02 / 591.08 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.27 / 14.44 | 14.10 / 28.96 | 52.56 / 96.92 | 101.80 / 118.62 |
| Read miss | 9.55 / 12.89 | 12.57 / 26.03 | 35.59 / 75.32 | 75.49 / 106.17 |
| Batch hit · 8 keys | 8.83 / 14.24 | 12.97 / 23.06 | 41.38 / 55.52 | 71.22 / 72.78 |
| Batch miss · 8 keys | 9.21 / 13.26 | 11.16 / 20.36 | 31.73 / 46.20 | 63.13 / 60.20 |
| Replace value | 15.44 / 15.24 | 26.05 / 31.04 | 86.18 / 125.24 | 145.74 / 150.78 |
| Delete + reinsert same key | 78.20 / 78.99 | 99.85 / 112.62 | 229.65 / 266.82 | 356.20 / 337.95 |
| Delete + insert new key | 114.56 / 113.47 | 137.53 / 147.76 | 340.12 / 370.83 | 526.33 / 520.35 |
| 4 workers · reads | 2.43 / 4.50 | 3.63 / 7.53 | 17.12 / 25.97 | 26.20 / 37.01 |
| 4 workers · 10% writes | 4.59 / 6.15 | 4.53 / 9.18 | 18.45 / 27.14 | 28.17 / 39.92 |

</details>

![Go versus Rust peak RSS for Tree and Map at four sizes, mixed keys](benchmarks/memory-arm64.svg)

![Go versus Rust construction time for Tree and Map at four sizes, mixed keys](benchmarks/build-arm64.svg)

Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. Construction inserts every entry into an empty container.

<details>
<summary>Memory and build measurements, mixed keys — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.69 / 2.16 | 4.87 / 2.15 | 5.69 / 2.15 | 0.0005 / 0.0004 |
| Tree | 262K | 12.08 / 9.06 | 11.65 / 9.04 | 12.08 / 9.04 | 0.0222 / 0.0277 |
| Tree | 5M | 123.32 / 136.48 | 123.29 / 136.46 | 123.32 / 136.46 | 0.5721 / 0.6530 |
| Tree | 50M | 1,180.98 / 1,347.81 | 1,180.89 / 1,347.80 | 1,180.98 / 1,347.80 | 9.8391 / 10.6225 |
| Map | 4K | 5.90 / 2.16 | 5.03 / 2.15 | 5.90 / 2.15 | 0.0005 / 0.0004 |
| Map | 262K | 18.84 / 9.06 | 18.66 / 9.04 | 18.84 / 9.04 | 0.0312 / 0.0282 |
| Map | 5M | 127.45 / 136.51 | 127.39 / 136.50 | 127.45 / 136.50 | 0.6625 / 0.6312 |
| Map | 50M | 1,195.93 / 1,348.21 | 1,195.90 / 1,348.19 | 1,195.93 / 1,348.19 | 11.7872 / 10.8610 |

</details>

### Sequential keys

Consecutive even keys; misses and fresh keys fill the gaps.

![Change in Go operation time versus Rust, sequential keys](benchmarks/cpu-sequential-arm64.svg)

![Go versus Rust construction time, sequential keys](benchmarks/build-sequential-arm64.svg)

<details>
<summary>Exact timings, memory and build measurements, sequential keys — Go / Rust</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.67 / 11.90 | 14.65 / 23.97 | 39.27 / 77.54 | 76.82 / 97.81 |
| Read miss | 7.20 / 11.29 | 12.11 / 22.24 | 21.46 / 53.96 | 51.02 / 97.21 |
| Batch hit · 8 keys | 9.55 / 12.14 | 12.78 / 22.49 | 31.67 / 47.27 | 54.47 / 71.08 |
| Batch miss · 8 keys | 8.93 / 11.19 | 11.84 / 20.09 | 16.69 / 33.26 | 43.99 / 58.35 |
| Replace value | 15.92 / 12.20 | 26.75 / 25.47 | 77.85 / 84.25 | 151.85 / 125.15 |
| Delete + reinsert same key | 43.49 / 79.16 | 69.59 / 122.14 | 158.33 / 234.80 | 288.97 / 324.54 |
| Delete + insert new key | 64.15 / 63.93 | 80.08 / 115.96 | 192.74 / 243.89 | 299.21 / 310.99 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 5.93 / 13.12 | 9.08 / 28.57 | 47.56 / 95.03 | 97.20 / 116.99 |
| Read miss | 5.95 / 12.43 | 7.03 / 26.48 | 24.86 / 55.64 | 64.95 / 99.38 |
| Batch hit · 8 keys | 6.27 / 13.66 | 8.73 / 22.79 | 28.21 / 51.39 | 69.80 / 73.93 |
| Batch miss · 8 keys | 6.21 / 12.95 | 6.75 / 20.99 | 20.04 / 35.41 | 54.59 / 64.37 |
| Replace value | 11.56 / 15.60 | 19.44 / 32.02 | 81.91 / 106.86 | 134.72 / 164.50 |
| Delete + reinsert same key | 74.73 / 80.21 | 97.75 / 120.11 | 243.07 / 244.79 | 335.49 / 328.18 |
| Delete + insert new key | 88.97 / 85.09 | 94.29 / 113.34 | 240.03 / 259.08 | 332.50 / 307.93 |
| 4 workers · reads | 1.62 / 3.39 | 2.46 / 7.36 | 14.46 / 24.23 | 25.96 / 36.01 |
| 4 workers · 10% writes | 3.83 / 6.14 | 3.11 / 9.00 | 15.53 / 25.32 | 27.50 / 39.38 |

![Go versus Rust peak RSS, sequential keys](benchmarks/memory-sequential-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.62 / 2.16 | 4.75 / 2.15 | 5.62 / 2.15 | 0.0001 / 0.0001 |
| Tree | 262K | 10.98 / 8.26 | 10.55 / 8.24 | 10.98 / 8.24 | 0.0060 / 0.0034 |
| Tree | 5M | 105.09 / 120.24 | 105.02 / 120.23 | 105.09 / 120.23 | 0.0813 / 0.0440 |
| Tree | 50M | 972.90 / 1,184.04 | 972.85 / 1,184.02 | 972.90 / 1,184.02 | 0.8627 / 0.4197 |
| Map | 4K | 5.64 / 2.16 | 4.73 / 2.15 | 5.64 / 2.15 | 0.0001 / 0.0001 |
| Map | 262K | 16.68 / 8.26 | 16.53 / 8.24 | 16.68 / 8.24 | 0.0080 / 0.0043 |
| Map | 5M | 115.39 / 120.29 | 115.31 / 120.27 | 115.39 / 120.27 | 0.1023 / 0.0472 |
| Map | 50M | 985.56 / 1,184.48 | 985.50 / 1,184.46 | 985.56 / 1,184.46 | 1.0191 / 0.5177 |

</details>

### Clustered keys

Runs of 1,024 consecutive even keys at scattered bases.

![Change in Go operation time versus Rust, clustered keys](benchmarks/cpu-clustered-arm64.svg)

![Go versus Rust construction time, clustered keys](benchmarks/build-clustered-arm64.svg)

<details>
<summary>Exact timings, memory and build measurements, clustered keys — Go / Rust</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 15.09 / 26.61 | 37.49 / 46.64 | 76.09 / 164.09 | 129.91 / 341.53 |
| Read miss | 14.65 / 24.63 | 32.17 / 44.34 | 62.50 / 143.06 | 113.05 / 326.93 |
| Batch hit · 8 keys | 17.08 / 25.27 | 31.91 / 42.79 | 76.73 / 128.14 | 108.82 / 207.73 |
| Batch miss · 8 keys | 17.31 / 23.87 | 31.54 / 40.97 | 71.26 / 117.89 | 103.66 / 198.03 |
| Replace value | 19.28 / 27.55 | 43.74 / 51.66 | 97.02 / 202.35 | 218.41 / 377.35 |
| Delete + reinsert same key | 52.97 / 116.86 | 91.22 / 172.90 | 205.72 / 440.72 | 369.61 / 752.85 |
| Delete + insert new key | 73.84 / 102.61 | 121.94 / 169.43 | 258.44 / 478.51 | 421.78 / 752.41 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 31.94 / 40.11 | 38.13 / 63.16 | 90.59 / 204.48 | 166.44 / 377.31 |
| Read miss | 30.29 / 37.92 | 38.21 / 59.94 | 68.12 / 188.99 | 144.46 / 354.29 |
| Batch hit · 8 keys | 31.75 / 34.44 | 38.31 / 47.09 | 101.18 / 156.15 | 181.68 / 258.08 |
| Batch miss · 8 keys | 31.60 / 30.38 | 35.70 / 45.13 | 78.93 / 144.04 | 163.45 / 257.62 |
| Replace value | 38.87 / 37.65 | 52.68 / 63.64 | 126.70 / 219.40 | 252.83 / 384.91 |
| Delete + reinsert same key | 100.32 / 117.47 | 131.36 / 163.78 | 259.41 / 382.40 | 437.17 / 661.88 |
| Delete + insert new key | 113.08 / 119.80 | 127.51 / 165.13 | 262.20 / 409.19 | 414.73 / 671.84 |
| 4 workers · reads | 8.01 / 9.88 | 9.64 / 15.64 | 25.92 / 54.91 | 48.36 / 91.92 |
| 4 workers · 10% writes | 10.44 / 10.50 | 11.39 / 17.48 | 27.19 / 56.51 | 49.68 / 94.54 |

![Go versus Rust peak RSS, clustered keys](benchmarks/memory-clustered-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.64 / 2.18 | 4.77 / 2.16 | 5.64 / 2.16 | 0.0001 / 0.0002 |
| Tree | 262K | 11.08 / 11.70 | 10.72 / 11.68 | 11.08 / 11.68 | 0.0056 / 0.0264 |
| Tree | 5M | 105.51 / 187.09 | 105.48 / 187.07 | 105.51 / 187.07 | 0.1091 / 0.4210 |
| Tree | 50M | 1,000.29 / 1,853.29 | 1,000.26 / 1,853.28 | 1,000.29 / 1,853.28 | 1.1848 / 4.3775 |
| Map | 4K | 5.90 / 2.18 | 5.05 / 2.16 | 5.90 / 2.16 | 0.0002 / 0.0001 |
| Map | 262K | 16.94 / 11.70 | 16.71 / 11.68 | 16.94 / 11.68 | 0.0123 / 0.0212 |
| Map | 5M | 109.71 / 187.09 | 109.61 / 187.07 | 109.71 / 187.07 | 0.1649 / 0.4003 |
| Map | 50M | 1,017.12 / 1,853.33 | 1,017.05 / 1,853.31 | 1,017.12 / 1,853.31 | 1.6641 / 4.1990 |

</details>

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-26.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

To reproduce, use source checkpoint `63a05de` and build both drivers:

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

Both scripts take `--keys mixed` (the default), `--keys sequential` or `--keys clustered`; the charts cover all three.

`python3 internal/render_benchmarks.py` regenerates the charts and the tables above from the saved report.

How the comparison is kept fair:

- **Identical work.** Both languages get identical keys, queries and operation counts. Checksums are verified, including every bit of replaced values. Different-key churn uses disjoint key sets and checks that every delete and insert succeeds.
- **Several key distributions.** Each workload runs on hashed keys spread across the full 64-bit range, on consecutive keys, and on clustered runs of consecutive keys, so no result rests on one distribution.
- **Fresh processes.** Every backend runs in a fresh process, in rotated order. Serial cases are pinned to one OS thread. Parallel timings exclude starting workers and reducing checksums.
- **Public APIs.** Batches go through each library's public batch API, and the Rust code is unmodified upstream.
- **Unequal guarantees.** The concurrent APIs promise different things. Rust omits `len()` and reads payloads with plain loads ([design](https://github.com/XiangpengHao/bbtree/blob/1c72bd27994ed0a38694b029618e80115e22644f/design/09-concurrency.md)). Go keeps an exact O(1) `Len` and reads numeric payloads atomically.
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

CI also runs a live Rust oracle for containers and file interoperability, against upstream pinned at [`1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f).

Licensed under **MIT OR Apache-2.0**, like upstream. See [LICENSE](LICENSE).
