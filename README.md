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

A `Map` of word-sized keys and values (integers, floats, bools) shares one tree as upstream's `BBTreeMap` does ([design](https://github.com/XiangpengHao/bbtree/blob/1c72bd27994ed0a38694b029618e80115e22644f/design/09-concurrency.md)). Every node carries a latch word with a version. Lookups descend optimistically, checking each node's version after reading its child's, and write nothing. Writers latch only the leaf they change. A split or a merge takes, without waiting, only the nodes it changes, and starts over if another goroutine holds one. No lock, epoch or counter is shared by all writers, so writes scale with cores. A lookup whose leaf keeps changing takes the leaf's shared latch, so readers make progress behind a hot leaf. Freed nodes keep their latch word for life and come back only as nodes of their kind, so a stale pointer always fails its version check.

Beyond upstream, every node also keeps its fences, the bounds of the keys under it, and a table of hints jumps from a key straight to its leaf. A hint is never trusted: the leaf's version and fences are checked before it is used, and a stale one costs only a descent from the root. Lookups read with plain loads, ordered by address dependencies rather than acquire loads, so independent lookups overlap their cache misses; race-detector builds use atomic loads instead. A full leaf first moves entries to a sibling with room, as `Tree` does, so leaves stay about four-fifths full; an ascending run splits its leaf nearly full instead.

`Map`s with other keys (strings, `MapOf` custom keys) still lock single leaves under a structural lock that splits and merges take exclusively.

- Iteration (`Scan`, `Range`, `Ascend`, `Descend`, `All`, their reverses, `First`, `Last`) keeps upstream's `range_cloned` contract, also for `Scan`: it copies a leaf at a time, under its shared latch, and visits the copy with nothing held, so the loop body may use the map. Upstream's `scan` instead runs its closure under each leaf's read latch, which holds back that leaf's writers and forbids writing the map from the closure. Iteration is not a snapshot: an entry present throughout is visited exactly once; entries inserted or removed meanwhile may or may not be.
- Each batch entry is consistent on its own; a batch is not a transaction.
- `Read`, `Update`, `UpdateOrInsert`, `DeleteIf` and `LoadOrStore` run under the entry's leaf latch, as upstream's do; their callbacks must not call back into the same map. A panicking callback releases its locks and stores nothing.
- As upstream's shared map, `Map` has no `Len`. `IsEmpty` reports whether the map held no entry at some moment during the call. `Clear`, like upstream's `clear(&mut self)`, is the owner's: no other goroutine may use the map until it returns.

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
- **`Map`:** `First`, `Last`, `PopFirst`, `PopLast`, `LoadOrStoreFunc`, `UpdateAll`, `UpdateRange`, `WriteFile`, `Ascend` and `Descend`.
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

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.99.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max. Sources: [Go `a4ff0ed`](https://github.com/thesyncim/gobbtree/commit/a4ff0ed88bb9c0c6597a36076442b1923f3835ed), [Rust `1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). `uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results. Every workload runs with three key patterns: **mixed keys**, hashed keys spread over the full 64-bit range; **sequential keys**, consecutive even keys; misses and fresh keys fill the gaps; **clustered keys**, runs of 1,024 consecutive even keys at scattered bases.

![Change in Go operation time versus Rust across four sizes and multiple workloads, mixed keys](benchmarks/cpu-arm64.svg)

Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. Parallel rows use four workers; the mixed row replaces values on 10% of operations.

<details>
<summary>Exact timings, mixed keys — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.32 / 12.59 | 9.78 / 23.59 | 34.05 / 81.88 | 66.44 / 109.83 |
| Read miss | 8.61 / 11.84 | 12.01 / 22.50 | 37.95 / 69.15 | 75.48 / 104.21 |
| Batch hit · 8 keys | 11.15 / 12.19 | 12.73 / 22.92 | 31.60 / 50.89 | 54.54 / 66.85 |
| Batch miss · 8 keys | 10.60 / 12.01 | 12.29 / 20.33 | 27.05 / 39.51 | 48.58 / 56.16 |
| Replace value | 12.24 / 13.02 | 16.92 / 26.27 | 47.35 / 84.26 | 91.75 / 112.81 |
| Delete + reinsert same key | 43.13 / 75.22 | 66.48 / 113.51 | 151.16 / 236.17 | 271.60 / 314.18 |
| Delete + insert new key | 66.90 / 100.08 | 105.07 / 148.03 | 238.77 / 343.16 | 439.59 / 485.06 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.52 / 13.62 | 16.53 / 28.03 | 53.88 / 98.33 | 96.57 / 116.90 |
| Read miss | 10.72 / 12.37 | 15.16 / 25.33 | 35.26 / 68.16 | 78.01 / 102.66 |
| Batch hit · 8 keys | 11.16 / 13.70 | 14.21 / 21.70 | 32.43 / 51.27 | 49.91 / 69.04 |
| Batch miss · 8 keys | 11.35 / 12.87 | 13.74 / 19.28 | 27.58 / 39.07 | 45.21 / 56.31 |
| Replace value | 13.34 / 14.70 | 20.53 / 29.48 | 60.83 / 110.54 | 114.42 / 150.06 |
| Delete + reinsert same key | 51.12 / 74.00 | 68.28 / 110.21 | 170.01 / 239.49 | 267.35 / 317.95 |
| Delete + insert new key | 57.69 / 104.97 | 93.40 / 144.53 | 247.07 / 339.81 | 453.34 / 487.26 |
| 4 workers · reads | 2.79 / 3.84 | 4.77 / 7.39 | 15.55 / 27.73 | 26.52 / 38.14 |
| 4 workers · 10% writes | 4.81 / 4.96 | 5.11 / 7.18 | 18.27 / 29.48 | 28.33 / 42.37 |

</details>

![Go versus Rust peak RSS for Tree and Map at four sizes, mixed keys](benchmarks/memory-arm64.svg)

![Go versus Rust construction time for Tree and Map at four sizes, mixed keys](benchmarks/build-arm64.svg)

Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. Construction inserts every entry into an empty container.

<details>
<summary>Memory and build measurements, mixed keys — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.65 / 2.16 | 4.85 / 2.15 | 5.65 / 2.15 | 0.0005 / 0.0003 |
| Tree | 262K | 12.78 / 9.06 | 12.55 / 9.04 | 12.78 / 9.04 | 0.0286 / 0.0284 |
| Tree | 5M | 127.84 / 136.48 | 127.84 / 136.46 | 127.84 / 136.46 | 0.5534 / 0.6464 |
| Tree | 50M | 1,225.98 / 1,347.81 | 1,225.95 / 1,347.80 | 1,225.98 / 1,347.80 | 10.3818 / 10.3758 |
| Map | 4K | 5.62 / 2.16 | 4.83 / 2.15 | 5.62 / 2.15 | 0.0006 / 0.0002 |
| Map | 262K | 12.45 / 9.06 | 12.12 / 9.04 | 12.45 / 9.04 | 0.0324 / 0.0288 |
| Map | 5M | 132.46 / 136.51 | 132.45 / 136.50 | 132.46 / 136.50 | 0.7075 / 0.6533 |
| Map | 50M | 1,252.41 / 1,348.21 | 1,252.33 / 1,348.19 | 1,252.41 / 1,348.19 | 13.0620 / 10.5127 |

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
| Read hit | 5.41 / 11.84 | 8.39 / 23.97 | 34.34 / 76.60 | 85.52 / 101.79 |
| Read miss | 5.98 / 10.95 | 8.55 / 22.09 | 26.43 / 53.29 | 56.35 / 94.51 |
| Batch hit · 8 keys | 10.14 / 11.82 | 12.63 / 21.85 | 32.25 / 47.60 | 56.49 / 69.56 |
| Batch miss · 8 keys | 9.40 / 11.08 | 11.65 / 19.86 | 18.97 / 32.27 | 47.09 / 57.84 |
| Replace value | 11.35 / 12.45 | 15.30 / 25.66 | 41.66 / 81.43 | 90.33 / 121.26 |
| Delete + reinsert same key | 41.59 / 78.47 | 68.78 / 120.41 | 150.23 / 226.61 | 279.80 / 301.99 |
| Delete + insert new key | 45.75 / 63.39 | 80.38 / 115.14 | 177.88 / 234.00 | 317.60 / 294.32 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.40 / 13.01 | 14.51 / 28.45 | 51.95 / 93.75 | 112.48 / 113.94 |
| Read miss | 8.22 / 12.36 | 11.60 / 26.25 | 25.16 / 56.19 | 68.31 / 97.44 |
| Batch hit · 8 keys | 10.22 / 13.89 | 12.16 / 22.16 | 31.63 / 48.83 | 49.48 / 71.16 |
| Batch miss · 8 keys | 9.63 / 12.91 | 11.10 / 20.48 | 19.14 / 34.12 | 39.72 / 59.40 |
| Replace value | 12.02 / 15.40 | 18.58 / 31.63 | 63.98 / 110.45 | 116.63 / 158.34 |
| Delete + reinsert same key | 49.96 / 79.80 | 66.11 / 119.44 | 160.49 / 232.40 | 267.64 / 305.54 |
| Delete + insert new key | 42.09 / 84.72 | 61.79 / 112.87 | 167.93 / 244.35 | 277.19 / 301.15 |
| 4 workers · reads | 2.52 / 3.36 | 4.32 / 7.34 | 15.37 / 25.07 | 25.96 / 36.33 |
| 4 workers · 10% writes | 4.49 / 4.83 | 4.67 / 7.44 | 18.17 / 27.91 | 27.20 / 40.08 |

![Go versus Rust peak RSS, sequential keys](benchmarks/memory-sequential-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.62 / 2.16 | 4.65 / 2.15 | 5.62 / 2.15 | 0.0001 / 0.0001 |
| Tree | 262K | 11.27 / 8.26 | 10.93 / 8.24 | 11.27 / 8.24 | 0.0040 / 0.0031 |
| Tree | 5M | 105.74 / 120.24 | 105.73 / 120.23 | 105.74 / 120.23 | 0.0425 / 0.0458 |
| Tree | 50M | 1,009.12 / 1,184.04 | 1,009.09 / 1,184.02 | 1,009.12 / 1,184.02 | 0.3855 / 0.4105 |
| Map | 4K | 5.75 / 2.16 | 4.88 / 2.15 | 5.75 / 2.15 | 0.0002 / 0.0001 |
| Map | 262K | 11.80 / 8.26 | 11.42 / 8.24 | 11.80 / 8.24 | 0.0056 / 0.0049 |
| Map | 5M | 119.29 / 120.29 | 119.24 / 120.27 | 119.29 / 120.27 | 0.0685 / 0.0605 |
| Map | 50M | 1,136.79 / 1,184.48 | 1,136.77 / 1,184.46 | 1,136.79 / 1,184.46 | 0.5573 / 0.5083 |

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
| Read hit | 5.87 / 26.79 | 11.59 / 50.73 | 46.68 / 165.27 | 108.22 / 348.51 |
| Read miss | 6.79 / 25.52 | 13.25 / 48.23 | 45.95 / 141.22 | 97.13 / 340.17 |
| Batch hit · 8 keys | 16.48 / 24.58 | 32.43 / 43.09 | 78.33 / 132.28 | 109.19 / 208.03 |
| Batch miss · 8 keys | 17.01 / 23.18 | 31.74 / 40.85 | 69.31 / 117.85 | 101.22 / 199.57 |
| Replace value | 10.90 / 28.35 | 18.54 / 53.27 | 68.16 / 215.84 | 128.29 / 398.26 |
| Delete + reinsert same key | 52.46 / 118.10 | 92.00 / 174.31 | 194.83 / 440.15 | 337.40 / 764.59 |
| Delete + insert new key | 57.80 / 103.74 | 109.56 / 170.13 | 252.01 / 484.74 | 408.69 / 771.58 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.05 / 38.65 | 25.14 / 59.16 | 86.27 / 202.97 | 165.70 / 366.25 |
| Read miss | 8.70 / 34.75 | 22.15 / 56.47 | 54.43 / 185.08 | 125.33 / 339.77 |
| Batch hit · 8 keys | 10.57 / 30.00 | 22.62 / 48.88 | 60.23 / 154.41 | 109.60 / 250.33 |
| Batch miss · 8 keys | 9.99 / 34.11 | 21.20 / 44.82 | 46.55 / 139.18 | 99.05 / 240.60 |
| Replace value | 12.43 / 40.97 | 29.45 / 61.95 | 93.02 / 211.34 | 167.62 / 378.71 |
| Delete + reinsert same key | 51.23 / 117.22 | 79.07 / 160.26 | 210.65 / 373.85 | 336.05 / 629.47 |
| Delete + insert new key | 44.81 / 119.43 | 85.74 / 159.03 | 233.51 / 399.38 | 357.99 / 644.77 |
| 4 workers · reads | 2.71 / 9.70 | 6.71 / 16.48 | 23.22 / 52.92 | 41.21 / 91.68 |
| 4 workers · 10% writes | 4.69 / 8.91 | 7.15 / 16.14 | 27.25 / 55.51 | 42.66 / 94.15 |

![Go versus Rust peak RSS, clustered keys](benchmarks/memory-clustered-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.62 / 2.18 | 4.77 / 2.16 | 5.62 / 2.16 | 0.0002 / 0.0002 |
| Tree | 262K | 13.45 / 11.70 | 13.29 / 11.68 | 13.45 / 11.68 | 0.0117 / 0.0291 |
| Tree | 5M | 132.51 / 187.11 | 132.50 / 187.09 | 132.51 / 187.09 | 0.1865 / 0.3863 |
| Tree | 50M | 1,139.10 / 1,853.28 | 1,139.05 / 1,853.26 | 1,139.10 / 1,853.26 | 1.8046 / 3.9882 |
| Map | 4K | 5.73 / 2.18 | 4.93 / 2.16 | 5.73 / 2.16 | 0.0002 / 0.0002 |
| Map | 262K | 12.70 / 11.70 | 12.62 / 11.68 | 12.70 / 11.68 | 0.0197 / 0.0268 |
| Map | 5M | 124.85 / 187.09 | 124.78 / 187.07 | 124.85 / 187.07 | 0.2281 / 0.3688 |
| Map | 50M | 1,238.09 / 1,853.33 | 1,238.04 / 1,853.31 | 1,238.09 / 1,853.31 | 2.9046 / 3.9188 |

</details>

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-29.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

To reproduce, use source checkpoint `a4ff0ed` and build both drivers:

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
- **Equal guarantees.** Both concurrent maps omit `len()`, and both read payloads with plain loads validated by node versions ([design](https://github.com/XiangpengHao/bbtree/blob/1c72bd27994ed0a38694b029618e80115e22644f/design/09-concurrency.md)).
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
