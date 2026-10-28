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
- **`NodeConfig` page sizes.** Nodes hold a fixed 32 entries. Go generics can index an array type parameter but cannot slice it or point into it, which shifting entries and the SIMD searches do, so another size would mean another copy of the node code.
- **The rest of `InlineKey`:** `WIDTH`, `hash_tag`, `cmp_prefix`, and `cmp_from` on custom keys. These tune upstream's node layout, and a generic Go container cannot call an optional key method without boxing the key.
- **Leaf annexes and their `*_with_leaf` methods.** They let `bbtree-store` manage manually allocated pages, which Go's garbage collector manages here.
- **The `inspect` and `check_invariants` debugging helpers.**

Additions beyond upstream:
- **`Map`:** `Len`, `First`, `Last`, `PopFirst`, `PopLast`, `LoadOrStoreFunc`, `UpdateAll`, `UpdateRange`, `WriteFile`, `Ascend` and `Descend`.
- **`Tree`:** `LowerBound`, `UpperBound`, `LoadOrStore`, `LoadOrStoreFunc` and `UpdateOrInsert`.
- **`FileMap`:** `ByteLen`, `WriteTo`, `Close`, `Ascend`, `Descend`, `Reverse` and `RangeReverse`.
- **Everywhere:** `Range(lo, hi)` as shorthand for `Scan(Between(lo, hi))`.

## SIMD

| Target | Search kernels |
|---|---|
| amd64 | AVX-512 or AVX2, with a 128-bit fallback |
| arm64 | NEON |
| Others | Portable Go SIMD where available, otherwise scalar |

The kernel is chosen at run time. `gobbtree.SIMD()` reports the choice, and `GODEBUG=simd=0` forces the scalar path.

## Benchmarks

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.99.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max. Sources: [Go `e7543ac`](https://github.com/thesyncim/gobbtree/commit/e7543ac2fb0ac0e800a901ed7b34979a42b59e70), [Rust `1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). `uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results. Every workload runs with three key patterns: **mixed keys**, hashed keys spread over the full 64-bit range; **sequential keys**, consecutive even keys; misses and fresh keys fill the gaps; **clustered keys**, runs of 1,024 consecutive even keys at scattered bases.

![Change in Go operation time versus Rust across four sizes and multiple workloads, mixed keys](benchmarks/cpu-arm64.svg)

Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. Parallel rows use four workers; the mixed row replaces values on 10% of operations.

<details>
<summary>Exact timings, mixed keys — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.63 / 12.51 | 18.04 / 23.78 | 42.11 / 94.82 | 85.34 / 109.93 |
| Read miss | 10.50 / 11.94 | 17.93 / 22.75 | 33.53 / 80.80 | 72.50 / 109.15 |
| Batch hit · 8 keys | 11.04 / 12.32 | 21.08 / 22.93 | 32.42 / 57.21 | 62.27 / 69.88 |
| Batch miss · 8 keys | 10.77 / 12.19 | 13.09 / 20.29 | 31.80 / 45.94 | 54.82 / 57.49 |
| Replace value | 17.16 / 12.79 | 32.77 / 26.05 | 130.08 / 93.22 | 157.74 / 114.15 |
| Delete + reinsert same key | 46.45 / 73.89 | 75.15 / 114.35 | 211.78 / 260.75 | 299.60 / 323.02 |
| Delete + insert new key | 89.42 / 96.77 | 129.33 / 149.24 | 275.53 / 375.63 | 453.49 / 512.34 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.10 / 14.40 | 14.41 / 28.44 | 60.08 / 99.47 | 100.05 / 119.57 |
| Read miss | 9.12 / 12.36 | 12.21 / 26.09 | 44.10 / 74.87 | 75.14 / 104.96 |
| Batch hit · 8 keys | 8.59 / 13.89 | 12.76 / 22.86 | 45.45 / 53.30 | 71.98 / 72.83 |
| Batch miss · 8 keys | 8.55 / 12.87 | 11.48 / 20.08 | 37.86 / 43.25 | 60.35 / 58.83 |
| Replace value | 14.92 / 14.67 | 25.86 / 30.51 | 89.42 / 117.42 | 154.43 / 155.82 |
| Delete + reinsert same key | 76.41 / 73.77 | 98.80 / 114.10 | 223.73 / 247.31 | 422.80 / 345.23 |
| Delete + insert new key | 113.20 / 103.26 | 135.01 / 149.86 | 333.25 / 352.73 | 565.05 / 545.60 |
| 4 workers · reads | 2.43 / 4.37 | 4.22 / 7.74 | 17.34 / 26.96 | 27.94 / 40.58 |
| 4 workers · 10% writes | 4.85 / 6.40 | 5.20 / 10.28 | 18.94 / 28.35 | 29.46 / 44.43 |

</details>

![Go versus Rust peak RSS for Tree and Map at four sizes, mixed keys](benchmarks/memory-arm64.svg)

![Go versus Rust construction time for Tree and Map at four sizes, mixed keys](benchmarks/build-arm64.svg)

Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. Construction inserts every entry into an empty container.

<details>
<summary>Memory and build measurements, mixed keys — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.60 / 2.16 | 4.78 / 2.15 | 5.60 / 2.15 | 0.0005 / 0.0003 |
| Tree | 262K | 12.24 / 9.06 | 11.67 / 9.04 | 12.24 / 9.04 | 0.0265 / 0.0281 |
| Tree | 5M | 125.29 / 136.48 | 125.26 / 136.46 | 125.29 / 136.46 | 0.6045 / 0.6551 |
| Tree | 50M | 1,190.95 / 1,347.81 | 1,190.89 / 1,347.80 | 1,190.95 / 1,347.80 | 10.1859 / 11.1529 |
| Map | 4K | 5.85 / 2.16 | 4.95 / 2.15 | 5.85 / 2.15 | 0.0004 / 0.0004 |
| Map | 262K | 19.02 / 9.06 | 18.87 / 9.04 | 19.02 / 9.04 | 0.0303 / 0.0271 |
| Map | 5M | 129.06 / 136.51 | 128.97 / 136.50 | 129.06 / 136.50 | 0.7267 / 0.6826 |
| Map | 50M | 1,199.46 / 1,348.21 | 1,199.41 / 1,348.19 | 1,199.46 / 1,348.19 | 12.3135 / 12.0528 |

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
| Read hit | 9.39 / 11.91 | 14.65 / 24.02 | 41.57 / 76.38 | 75.08 / 96.71 |
| Read miss | 7.79 / 11.13 | 12.11 / 22.35 | 21.91 / 48.79 | 53.85 / 94.19 |
| Batch hit · 8 keys | 10.25 / 12.08 | 12.74 / 22.10 | 31.57 / 47.94 | 55.13 / 70.40 |
| Batch miss · 8 keys | 9.47 / 11.30 | 11.92 / 20.25 | 20.11 / 30.73 | 44.23 / 59.48 |
| Replace value | 16.04 / 12.40 | 26.79 / 25.55 | 74.73 / 82.96 | 160.71 / 125.66 |
| Delete + reinsert same key | 43.80 / 80.18 | 70.69 / 122.86 | 164.34 / 229.94 | 287.68 / 318.70 |
| Delete + insert new key | 63.11 / 64.15 | 82.71 / 116.30 | 177.93 / 236.19 | 301.17 / 303.55 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.30 / 13.15 | 9.11 / 28.47 | 44.24 / 92.58 | 97.93 / 126.47 |
| Read miss | 5.71 / 12.46 | 6.91 / 26.59 | 20.61 / 53.29 | 70.33 / 101.79 |
| Batch hit · 8 keys | 6.07 / 13.78 | 8.63 / 22.88 | 27.30 / 48.61 | 69.26 / 75.11 |
| Batch miss · 8 keys | 6.46 / 13.07 | 6.81 / 20.93 | 14.68 / 33.16 | 54.58 / 59.85 |
| Replace value | 11.62 / 15.63 | 19.44 / 32.07 | 69.76 / 107.22 | 141.73 / 162.13 |
| Delete + reinsert same key | 77.13 / 80.65 | 97.49 / 120.35 | 205.02 / 236.72 | 339.20 / 334.96 |
| Delete + insert new key | 90.91 / 85.75 | 94.70 / 113.94 | 242.77 / 240.96 | 331.66 / 324.08 |
| 4 workers · reads | 1.61 / 3.45 | 2.44 / 7.36 | 12.94 / 24.14 | 25.63 / 35.12 |
| 4 workers · 10% writes | 4.43 / 6.18 | 3.37 / 8.47 | 14.00 / 25.63 | 28.09 / 39.45 |

![Go versus Rust peak RSS, sequential keys](benchmarks/memory-sequential-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.46 / 2.16 | 4.73 / 2.15 | 5.46 / 2.15 | 0.0000 / 0.0000 |
| Tree | 262K | 10.93 / 8.26 | 10.53 / 8.24 | 10.93 / 8.24 | 0.0023 / 0.0033 |
| Tree | 5M | 103.12 / 120.24 | 103.09 / 120.23 | 103.12 / 120.23 | 0.0467 / 0.0451 |
| Tree | 50M | 972.26 / 1,184.04 | 972.16 / 1,184.02 | 972.26 / 1,184.02 | 0.4254 / 0.4179 |
| Map | 4K | 5.65 / 2.16 | 4.80 / 2.15 | 5.65 / 2.15 | 0.0001 / 0.0001 |
| Map | 262K | 16.73 / 8.26 | 16.60 / 8.24 | 16.73 / 8.24 | 0.0057 / 0.0044 |
| Map | 5M | 113.30 / 120.29 | 113.26 / 120.27 | 113.30 / 120.27 | 0.0704 / 0.0600 |
| Map | 50M | 986.86 / 1,184.48 | 986.81 / 1,184.46 | 986.86 / 1,184.46 | 0.6357 / 0.5137 |

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
| Read hit | 15.79 / 25.73 | 36.92 / 47.90 | 78.22 / 168.85 | 136.32 / 353.63 |
| Read miss | 14.99 / 25.87 | 35.58 / 45.58 | 63.01 / 138.31 | 113.02 / 334.27 |
| Batch hit · 8 keys | 17.21 / 25.43 | 35.40 / 44.86 | 78.96 / 126.68 | 112.30 / 215.26 |
| Batch miss · 8 keys | 17.23 / 24.20 | 36.07 / 42.79 | 70.89 / 115.20 | 108.03 / 198.61 |
| Replace value | 19.43 / 28.00 | 44.55 / 52.67 | 109.56 / 199.51 | 227.16 / 393.21 |
| Delete + reinsert same key | 54.25 / 119.47 | 96.14 / 185.09 | 218.01 / 473.55 | 387.85 / 781.97 |
| Delete + insert new key | 72.97 / 104.46 | 129.27 / 179.18 | 260.15 / 507.25 | 414.42 / 768.30 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 35.74 / 41.15 | 40.57 / 64.37 | 91.82 / 209.80 | 169.46 / 373.82 |
| Read miss | 44.12 / 37.88 | 37.08 / 62.67 | 70.14 / 192.57 | 146.74 / 345.13 |
| Batch hit · 8 keys | 34.86 / 33.54 | 38.87 / 49.39 | 101.49 / 158.40 | 181.84 / 255.25 |
| Batch miss · 8 keys | 41.59 / 27.93 | 35.43 / 45.32 | 79.33 / 151.88 | 157.63 / 246.27 |
| Replace value | 54.85 / 36.33 | 53.51 / 64.62 | 126.66 / 212.58 | 244.48 / 380.98 |
| Delete + reinsert same key | 107.37 / 121.62 | 134.03 / 168.06 | 267.57 / 402.68 | 416.80 / 666.36 |
| Delete + insert new key | 117.81 / 123.25 | 131.10 / 170.24 | 275.60 / 390.87 | 400.22 / 674.12 |
| 4 workers · reads | 23.09 / 10.81 | 10.00 / 16.29 | 26.30 / 54.97 | 49.88 / 92.15 |
| 4 workers · 10% writes | 26.48 / 12.08 | 11.37 / 17.23 | 27.77 / 57.27 | 51.31 / 95.10 |

![Go versus Rust peak RSS, clustered keys](benchmarks/memory-clustered-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.64 / 2.18 | 4.85 / 2.16 | 5.64 / 2.16 | 0.0001 / 0.0001 |
| Tree | 262K | 11.06 / 11.70 | 10.65 / 11.68 | 11.06 / 11.68 | 0.0066 / 0.0274 |
| Tree | 5M | 106.79 / 187.09 | 106.76 / 187.07 | 106.79 / 187.07 | 0.1076 / 0.4122 |
| Tree | 50M | 1,003.90 / 1,853.29 | 1,003.85 / 1,853.28 | 1,003.90 / 1,853.28 | 1.0830 / 4.3224 |
| Map | 4K | 5.75 / 2.18 | 5.00 / 2.16 | 5.75 / 2.16 | 0.0002 / 0.0002 |
| Map | 262K | 17.06 / 11.70 | 16.92 / 11.68 | 17.06 / 11.68 | 0.0124 / 0.0262 |
| Map | 5M | 111.56 / 187.11 | 111.49 / 187.09 | 111.56 / 187.09 | 0.1616 / 0.3932 |
| Map | 50M | 1,024.69 / 1,853.33 | 1,024.64 / 1,853.31 | 1,024.69 / 1,853.31 | 1.5762 / 4.1903 |

</details>

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-26.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

To reproduce, use source checkpoint `e7543ac` and build both drivers:

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
