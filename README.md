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

Beyond upstream, every node also keeps its fences, the bounds of the keys under it, and a table of hints jumps from a key straight to its leaf. A hint is never trusted: the leaf's version and fences are checked before it is used, and a stale one costs only a descent from the root. Lookups read with plain loads, ordered by address dependencies rather than acquire loads, so independent lookups overlap their cache misses; race-detector builds use atomic loads instead.

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

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.99.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max. Sources: [Go `1a1a75a`](https://github.com/thesyncim/gobbtree/commit/1a1a75afc83da658c0610cce13bdc8e021e12a03), [Rust `1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). `uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results. Every workload runs with three key patterns: **mixed keys**, hashed keys spread over the full 64-bit range; **sequential keys**, consecutive even keys; misses and fresh keys fill the gaps; **clustered keys**, runs of 1,024 consecutive even keys at scattered bases.

![Change in Go operation time versus Rust across four sizes and multiple workloads, mixed keys](benchmarks/cpu-arm64.svg)

Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. Parallel rows use four workers; the mixed row replaces values on 10% of operations.

<details>
<summary>Exact timings, mixed keys — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.32 / 12.14 | 9.77 / 23.00 | 33.37 / 82.39 | 67.42 / 110.97 |
| Read miss | 8.51 / 11.50 | 12.14 / 21.83 | 38.64 / 67.08 | 75.96 / 104.09 |
| Batch hit · 8 keys | 10.96 / 11.94 | 12.73 / 22.78 | 31.07 / 50.49 | 54.20 / 67.14 |
| Batch miss · 8 keys | 10.53 / 11.74 | 12.23 / 20.29 | 26.92 / 41.54 | 47.72 / 57.97 |
| Replace value | 11.73 / 12.70 | 16.40 / 25.41 | 48.63 / 85.04 | 91.72 / 111.00 |
| Delete + reinsert same key | 41.52 / 71.39 | 66.54 / 107.56 | 147.61 / 232.58 | 270.04 / 307.27 |
| Delete + insert new key | 62.80 / 94.36 | 101.98 / 136.66 | 230.83 / 336.51 | 438.66 / 486.77 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.33 / 13.15 | 16.28 / 27.19 | 52.06 / 98.65 | 95.92 / 112.36 |
| Read miss | 10.36 / 12.07 | 14.72 / 24.87 | 36.57 / 68.22 | 80.61 / 100.25 |
| Batch hit · 8 keys | 11.12 / 13.28 | 14.14 / 21.60 | 32.75 / 50.69 | 51.88 / 68.38 |
| Batch miss · 8 keys | 11.28 / 12.59 | 13.63 / 19.29 | 28.26 / 39.01 | 46.92 / 55.33 |
| Replace value | 12.45 / 14.54 | 21.48 / 29.22 | 64.81 / 109.92 | 123.52 / 143.25 |
| Delete + reinsert same key | 49.26 / 71.78 | 67.85 / 106.76 | 160.40 / 231.69 | 263.74 / 318.97 |
| Delete + insert new key | 54.89 / 99.22 | 88.28 / 134.76 | 222.12 / 331.54 | 413.19 / 492.83 |
| 4 workers · reads | 2.81 / 3.81 | 4.72 / 7.36 | 16.36 / 28.25 | 27.15 / 37.51 |
| 4 workers · 10% writes | 4.76 / 4.96 | 4.90 / 7.28 | 18.82 / 29.50 | 28.25 / 42.49 |

</details>

![Go versus Rust peak RSS for Tree and Map at four sizes, mixed keys](benchmarks/memory-arm64.svg)

![Go versus Rust construction time for Tree and Map at four sizes, mixed keys](benchmarks/build-arm64.svg)

Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. Construction inserts every entry into an empty container.

<details>
<summary>Memory and build measurements, mixed keys — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.75 / 2.16 | 4.93 / 2.15 | 5.75 / 2.15 | 0.0004 / 0.0004 |
| Tree | 262K | 12.58 / 9.06 | 12.48 / 9.04 | 12.58 / 9.04 | 0.0292 / 0.0294 |
| Tree | 5M | 127.81 / 136.48 | 127.81 / 136.46 | 127.81 / 136.46 | 0.5552 / 0.6323 |
| Tree | 50M | 1,225.92 / 1,347.81 | 1,225.88 / 1,347.80 | 1,225.92 / 1,347.80 | 10.4302 / 10.4508 |
| Map | 4K | 5.75 / 2.16 | 4.96 / 2.15 | 5.75 / 2.15 | 0.0005 / 0.0004 |
| Map | 262K | 13.30 / 9.06 | 13.25 / 9.04 | 13.30 / 9.04 | 0.0289 / 0.0305 |
| Map | 5M | 146.55 / 136.51 | 146.55 / 136.50 | 146.55 / 136.50 | 0.6649 / 0.6568 |
| Map | 50M | 1,415.53 / 1,348.21 | 1,415.46 / 1,348.19 | 1,415.53 / 1,348.19 | 12.0213 / 10.5279 |

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
| Read hit | 5.35 / 11.86 | 8.44 / 24.08 | 37.50 / 75.75 | 91.46 / 96.05 |
| Read miss | 6.04 / 11.04 | 9.01 / 22.13 | 28.61 / 51.32 | 67.88 / 93.97 |
| Batch hit · 8 keys | 10.05 / 11.87 | 12.70 / 21.89 | 31.77 / 49.07 | 58.22 / 73.38 |
| Batch miss · 8 keys | 9.41 / 11.05 | 11.97 / 19.90 | 18.61 / 32.37 | 46.40 / 58.78 |
| Replace value | 11.31 / 12.44 | 15.49 / 25.87 | 48.81 / 83.87 | 121.70 / 119.37 |
| Delete + reinsert same key | 41.85 / 78.09 | 68.45 / 119.65 | 146.65 / 228.80 | 274.52 / 310.07 |
| Delete + insert new key | 45.91 / 63.79 | 80.50 / 114.67 | 179.37 / 235.54 | 312.10 / 295.52 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.50 / 12.98 | 14.65 / 28.51 | 73.81 / 93.13 | 151.37 / 117.90 |
| Read miss | 8.17 / 12.38 | 11.64 / 26.26 | 36.99 / 54.72 | 68.99 / 94.52 |
| Batch hit · 8 keys | 10.36 / 13.60 | 12.36 / 22.14 | 51.18 / 49.14 | 49.23 / 72.81 |
| Batch miss · 8 keys | 9.58 / 12.87 | 11.31 / 20.40 | 34.70 / 35.01 | 39.36 / 59.72 |
| Replace value | 11.49 / 15.46 | 19.25 / 31.64 | 81.06 / 106.02 | 121.02 / 155.97 |
| Delete + reinsert same key | 48.19 / 79.52 | 64.39 / 120.05 | 177.65 / 234.89 | 254.53 / 303.49 |
| Delete + insert new key | 41.86 / 85.18 | 60.09 / 114.49 | 186.77 / 242.59 | 270.24 / 299.93 |
| 4 workers · reads | 2.54 / 3.37 | 4.35 / 7.45 | 18.98 / 24.73 | 25.84 / 36.43 |
| 4 workers · 10% writes | 4.53 / 4.82 | 4.92 / 7.46 | 21.52 / 27.48 | 27.02 / 41.21 |

![Go versus Rust peak RSS, sequential keys](benchmarks/memory-sequential-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.54 / 2.16 | 4.75 / 2.15 | 5.54 / 2.15 | 0.0001 / 0.0001 |
| Tree | 262K | 11.22 / 8.26 | 10.93 / 8.24 | 11.22 / 8.24 | 0.0038 / 0.0034 |
| Tree | 5M | 105.40 / 120.24 | 105.38 / 120.23 | 105.40 / 120.23 | 0.0470 / 0.0471 |
| Tree | 50M | 989.10 / 1,184.02 | 989.10 / 1,184.01 | 989.10 / 1,184.01 | 0.3907 / 0.4116 |
| Map | 4K | 5.65 / 2.16 | 4.96 / 2.15 | 5.65 / 2.15 | 0.0002 / 0.0001 |
| Map | 262K | 13.01 / 8.26 | 12.91 / 8.24 | 13.01 / 8.24 | 0.0094 / 0.0045 |
| Map | 5M | 136.84 / 120.29 | 136.84 / 120.27 | 136.84 / 120.27 | 0.1053 / 0.0594 |
| Map | 50M | 1,331.31 / 1,184.48 | 1,331.25 / 1,184.46 | 1,331.31 / 1,184.46 | 1.0277 / 0.5157 |

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
| Read hit | 5.91 / 26.77 | 11.58 / 49.51 | 46.73 / 168.53 | 114.03 / 347.57 |
| Read miss | 6.81 / 24.99 | 13.34 / 45.40 | 45.03 / 144.33 | 97.28 / 341.44 |
| Batch hit · 8 keys | 17.09 / 22.80 | 29.35 / 43.59 | 79.47 / 131.91 | 108.97 / 207.22 |
| Batch miss · 8 keys | 17.04 / 23.78 | 28.33 / 41.94 | 68.12 / 118.03 | 101.08 / 199.51 |
| Replace value | 10.79 / 28.77 | 18.41 / 53.12 | 64.51 / 214.56 | 129.26 / 400.26 |
| Delete + reinsert same key | 52.09 / 117.36 | 91.37 / 175.07 | 196.61 / 445.79 | 348.80 / 779.15 |
| Delete + insert new key | 57.96 / 103.66 | 107.94 / 170.52 | 251.97 / 489.37 | 404.81 / 771.28 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.10 / 35.05 | 26.00 / 58.26 | 88.29 / 204.76 | 178.49 / 363.53 |
| Read miss | 8.69 / 33.14 | 23.09 / 56.30 | 61.80 / 186.88 | 130.27 / 339.64 |
| Batch hit · 8 keys | 10.98 / 34.78 | 23.34 / 46.84 | 60.85 / 153.64 | 112.75 / 252.13 |
| Batch miss · 8 keys | 10.26 / 29.86 | 22.09 / 45.39 | 49.30 / 139.81 | 105.19 / 241.55 |
| Replace value | 11.72 / 35.32 | 30.92 / 61.88 | 105.64 / 212.97 | 181.39 / 377.29 |
| Delete + reinsert same key | 50.32 / 117.38 | 77.57 / 163.52 | 211.24 / 375.09 | 333.23 / 631.01 |
| Delete + insert new key | 42.68 / 120.55 | 82.42 / 163.73 | 220.02 / 399.26 | 359.25 / 634.68 |
| 4 workers · reads | 2.72 / 10.16 | 6.98 / 16.11 | 25.18 / 53.24 | 42.69 / 91.75 |
| 4 workers · 10% writes | 4.59 / 8.84 | 7.32 / 16.21 | 27.98 / 55.70 | 43.58 / 93.97 |

![Go versus Rust peak RSS, clustered keys](benchmarks/memory-clustered-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.72 / 2.18 | 4.92 / 2.16 | 5.72 / 2.16 | 0.0002 / 0.0002 |
| Tree | 262K | 13.39 / 11.70 | 13.24 / 11.68 | 13.39 / 11.68 | 0.0167 / 0.0264 |
| Tree | 5M | 132.40 / 187.09 | 132.37 / 187.07 | 132.40 / 187.07 | 0.1882 / 0.3860 |
| Tree | 50M | 1,138.95 / 1,853.29 | 1,138.95 / 1,853.28 | 1,138.95 / 1,853.28 | 1.8200 / 4.0093 |
| Map | 4K | 5.82 / 2.18 | 5.00 / 2.16 | 5.82 / 2.16 | 0.0003 / 0.0001 |
| Map | 262K | 14.39 / 11.70 | 14.25 / 11.68 | 14.39 / 11.68 | 0.0160 / 0.0270 |
| Map | 5M | 160.69 / 187.11 | 160.69 / 187.09 | 160.69 / 187.09 | 0.1858 / 0.3749 |
| Map | 50M | 1,504.26 / 1,853.33 | 1,504.23 / 1,853.31 | 1,504.26 / 1,853.31 | 2.1920 / 3.9078 |

</details>

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-29.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

To reproduce, use source checkpoint `1a1a75a` and build both drivers:

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
