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

`Map`s of other keys or values (strings, `MapOf` custom keys, values wider than a word) run the same protocol over nodes of any key and value. A separator is a boxed key, never changed once boxed, so a racing descent compares it whole. Text keys route as upstream's do: the root keeps the prefix every key shares, and a query without it reads no node; an internal node's prefix is what its fences share, which every key under it has, so a descent skips it unchecked; and the node keeps a 4-byte summary of each separator past it, all 32 in one 128-byte line, comparing the boxed separators only where summaries tie. Upstream keeps no separator keys and latches the leaf whose head key settles a tie; a boxed key needs no latch. A text leaf keeps each key's next 16 bytes and length, so a key no longer than those settles within the leaf. A lookup reads its leaf without latching it when codes settle the key: integer and float keys, and text keys with a single candidate, whose key header and value it copies racing and uses only once the leaf's version holds. Upstream copies only `Copy` values that way; Go's collector keeps alive whatever a torn copy points at, and the copy is dropped unread, so any value can be. Other lookups, and custom keys, take the leaf's shared latch, as upstream does for keys it can't read inline. Freed nodes go to the collector, which keeps one alive while a stale pointer reaches it; its version, bumped as it is freed, fails that pointer's check.

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

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.99.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max, from [Go `7f9ff12`](https://github.com/thesyncim/gobbtree/commit/7f9ff128de5ac2ae7f7712840be515db23a559a2) and [Rust `1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f): medians of three fresh-process runs over identical datasets, with checked results.

![Go's speed relative to Rust for each workload, Tree and Map, uint64 and string keys](benchmarks/summary-arm64.svg)

Each dot is one measurement; `uint64` rows cover 4K to 50M entries with mixed keys (hashed keys spread over the full 64-bit range), sequential keys (consecutive even keys; misses and fresh keys fill the gaps), clustered keys (runs of 1,024 consecutive even keys at scattered bases). Batches are per key, churn is one deletion and one insertion, and parallel rows use four workers. Memory is whole-process peak RSS, including the Go runtime and GC headroom.

<details>
<summary>Times, mixed keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.44 / 12.80 | 9.99 / 24.07 | 33.35 / 85.08 | 69.77 / 112.72 |
| Read miss | 8.72 / 11.92 | 12.27 / 22.93 | 37.78 / 64.29 | 80.13 / 108.86 |
| Batch hit · 8 keys | 11.09 / 12.26 | 13.13 / 22.80 | 32.61 / 52.55 | 61.34 / 69.07 |
| Batch miss · 8 keys | 11.08 / 11.90 | 12.75 / 20.49 | 28.59 / 40.29 | 66.26 / 56.97 |
| Replace value | 12.07 / 12.81 | 17.19 / 26.41 | 56.26 / 87.15 | 98.44 / 119.07 |
| Delete + reinsert same key | 33.19 / 74.15 | 50.06 / 115.86 | 132.90 / 242.69 | 243.24 / 351.30 |
| Delete + insert new key | 59.91 / 98.94 | 91.56 / 147.78 | 226.89 / 355.22 | 400.83 / 546.21 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.39 / 13.61 | 16.49 / 28.28 | 54.77 / 97.21 | 92.10 / 119.65 |
| Read miss | 10.53 / 12.64 | 15.28 / 25.63 | 35.72 / 71.11 | 80.83 / 108.46 |
| Batch hit · 8 keys | 11.26 / 13.91 | 14.69 / 22.15 | 33.27 / 52.19 | 55.74 / 67.93 |
| Batch miss · 8 keys | 11.52 / 12.89 | 13.95 / 19.88 | 24.46 / 40.73 | 48.07 / 57.47 |
| Replace value | 12.05 / 14.93 | 22.48 / 29.53 | 70.86 / 113.78 | 125.86 / 150.10 |
| Delete + reinsert same key | 50.35 / 75.88 | 70.92 / 111.38 | 174.19 / 245.89 | 303.93 / 351.80 |
| Delete + insert new key | 56.93 / 103.00 | 93.52 / 144.95 | 252.01 / 349.25 | 468.14 / 533.66 |
| 4 workers · reads | 2.76 / 4.64 | 4.38 / 7.68 | 16.98 / 25.62 | 24.64 / 37.45 |
| 4 workers · 10% writes | 6.40 / 7.70 | 7.48 / 9.17 | 18.12 / 26.88 | 26.59 / 39.77 |

</details>

<details>
<summary>Times, sequential keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 5.35 / 11.90 | 8.54 / 24.21 | 35.15 / 76.76 | 87.31 / 106.24 |
| Read miss | 5.93 / 11.06 | 8.80 / 22.21 | 29.07 / 51.78 | 62.02 / 98.05 |
| Batch hit · 8 keys | 10.16 / 11.94 | 12.85 / 21.98 | 30.26 / 47.73 | 58.23 / 70.73 |
| Batch miss · 8 keys | 9.70 / 11.08 | 11.66 / 19.99 | 19.52 / 31.66 | 47.91 / 59.22 |
| Replace value | 11.37 / 12.37 | 15.52 / 25.61 | 44.64 / 82.14 | 87.71 / 122.74 |
| Delete + reinsert same key | 34.17 / 79.24 | 50.27 / 124.13 | 120.50 / 234.30 | 238.08 / 324.95 |
| Delete + insert new key | 34.41 / 64.67 | 56.25 / 116.54 | 153.61 / 239.01 | 268.90 / 309.17 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.32 / 13.46 | 14.47 / 28.61 | 52.73 / 92.79 | 114.23 / 122.93 |
| Read miss | 8.16 / 12.63 | 11.55 / 26.33 | 28.59 / 54.70 | 71.38 / 100.72 |
| Batch hit · 8 keys | 10.28 / 13.73 | 12.79 / 22.51 | 32.25 / 49.26 | 48.24 / 76.12 |
| Batch miss · 8 keys | 9.72 / 13.01 | 11.10 / 20.58 | 17.74 / 31.98 | 40.00 / 59.27 |
| Replace value | 10.97 / 15.13 | 19.41 / 31.79 | 65.82 / 105.69 | 123.08 / 165.26 |
| Delete + reinsert same key | 49.37 / 79.68 | 66.18 / 120.36 | 166.40 / 237.10 | 272.14 / 325.13 |
| Delete + insert new key | 41.52 / 87.15 | 61.77 / 115.34 | 179.57 / 246.10 | 284.09 / 318.19 |
| 4 workers · reads | 2.51 / 3.42 | 3.91 / 7.33 | 15.80 / 23.62 | 25.09 / 36.81 |
| 4 workers · 10% writes | 4.82 / 5.76 | 5.36 / 8.56 | 17.78 / 25.77 | 26.09 / 39.62 |

</details>

<details>
<summary>Times, clustered keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 5.79 / 26.67 | 11.59 / 48.54 | 47.04 / 169.83 | 112.42 / 356.58 |
| Read miss | 6.72 / 25.67 | 13.25 / 45.01 | 47.77 / 148.28 | 105.59 / 340.30 |
| Batch hit · 8 keys | 17.33 / 24.21 | 27.79 / 42.84 | 79.32 / 129.68 | 114.02 / 209.56 |
| Batch miss · 8 keys | 17.23 / 23.41 | 28.05 / 40.98 | 69.23 / 117.54 | 107.33 / 201.82 |
| Replace value | 10.88 / 28.20 | 18.46 / 52.24 | 66.22 / 216.26 | 136.94 / 398.62 |
| Delete + reinsert same key | 34.61 / 120.93 | 54.31 / 177.24 | 163.44 / 456.88 | 302.36 / 764.47 |
| Delete + insert new key | 36.53 / 104.40 | 63.05 / 176.43 | 196.57 / 497.57 | 359.39 / 738.89 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.03 / 37.19 | 21.60 / 60.89 | 80.03 / 205.67 | 174.47 / 373.03 |
| Read miss | 8.71 / 33.88 | 18.31 / 59.80 | 51.88 / 187.38 | 135.38 / 342.73 |
| Batch hit · 8 keys | 10.72 / 33.86 | 18.99 / 50.58 | 55.59 / 157.34 | 112.87 / 255.68 |
| Batch miss · 8 keys | 10.21 / 30.45 | 17.60 / 47.01 | 40.35 / 143.69 | 105.16 / 242.86 |
| Replace value | 11.60 / 39.99 | 26.65 / 63.50 | 95.67 / 213.07 | 180.22 / 381.13 |
| Delete + reinsert same key | 51.20 / 121.82 | 75.12 / 170.91 | 220.17 / 381.69 | 342.65 / 668.81 |
| Delete + insert new key | 44.76 / 121.22 | 78.68 / 161.99 | 235.60 / 403.60 | 369.75 / 659.88 |
| 4 workers · reads | 2.69 / 9.67 | 5.66 / 16.19 | 22.41 / 54.11 | 41.25 / 92.46 |
| 4 workers · 10% writes | 5.45 / 10.87 | 7.51 / 17.00 | 24.59 / 56.94 | 42.07 / 95.39 |

</details>

<details>
<summary>Memory and build — Go / Rust</summary>

**Mixed keys**

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.67 / 2.15 | 4.83 / 2.13 | 5.67 / 2.13 | 0.0005 / 0.0004 |
| Tree | 262K | 12.63 / 9.04 | 12.53 / 9.03 | 12.63 / 9.03 | 0.0193 / 0.0276 |
| Tree | 5M | 127.84 / 136.46 | 127.84 / 136.45 | 127.84 / 136.45 | 0.5746 / 0.6463 |
| Tree | 50M | 1,225.92 / 1,347.80 | 1,225.90 / 1,347.78 | 1,225.92 / 1,347.78 | 10.7388 / 11.0083 |
| Map | 4K | 5.70 / 2.15 | 4.87 / 2.13 | 5.70 / 2.13 | 0.0003 / 0.0004 |
| Map | 262K | 12.52 / 9.04 | 12.21 / 9.03 | 12.52 / 9.03 | 0.0276 / 0.0288 |
| Map | 5M | 130.38 / 136.50 | 130.38 / 136.48 | 130.38 / 136.48 | 0.6319 / 0.6571 |
| Map | 50M | 1,252.66 / 1,348.19 | 1,252.59 / 1,348.17 | 1,252.66 / 1,348.17 | 11.0923 / 11.1433 |

**Sequential keys**

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.49 / 2.15 | 4.59 / 2.13 | 5.49 / 2.13 | 0.0000 / 0.0001 |
| Tree | 262K | 11.37 / 8.24 | 10.94 / 8.22 | 11.37 / 8.22 | 0.0034 / 0.0031 |
| Tree | 5M | 105.69 / 120.23 | 105.66 / 120.21 | 105.69 / 120.21 | 0.0399 / 0.0434 |
| Tree | 50M | 1,008.14 / 1,184.02 | 1,008.14 / 1,184.01 | 1,008.14 / 1,184.01 | 0.4117 / 0.4239 |
| Map | 4K | 5.57 / 2.15 | 4.64 / 2.13 | 5.57 / 2.13 | 0.0001 / 0.0001 |
| Map | 262K | 11.81 / 8.24 | 11.47 / 8.22 | 11.81 / 8.22 | 0.0049 / 0.0027 |
| Map | 5M | 119.37 / 120.27 | 119.34 / 120.26 | 119.37 / 120.26 | 0.0640 / 0.0598 |
| Map | 50M | 1,136.87 / 1,184.46 | 1,136.79 / 1,184.45 | 1,136.87 / 1,184.45 | 0.5468 / 0.5190 |

**Clustered keys**

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.70 / 2.16 | 4.80 / 2.15 | 5.70 / 2.15 | 0.0001 / 0.0002 |
| Tree | 262K | 13.32 / 11.68 | 13.22 / 11.67 | 13.32 / 11.67 | 0.0107 / 0.0267 |
| Tree | 5M | 132.43 / 187.09 | 132.42 / 187.07 | 132.43 / 187.07 | 0.1920 / 0.4177 |
| Tree | 50M | 1,139.02 / 1,853.28 | 1,139.02 / 1,853.26 | 1,139.02 / 1,853.26 | 1.8514 / 4.4251 |
| Map | 4K | 5.75 / 2.16 | 4.78 / 2.15 | 5.75 / 2.15 | 0.0002 / 0.0001 |
| Map | 262K | 13.29 / 11.68 | 13.14 / 11.67 | 13.29 / 11.67 | 0.0176 / 0.0253 |
| Map | 5M | 141.48 / 187.09 | 141.43 / 187.07 | 141.48 / 187.07 | 0.2213 / 0.3964 |
| Map | 50M | 1,238.19 / 1,853.31 | 1,238.19 / 1,853.29 | 1,238.19 / 1,853.29 | 2.4620 / 4.2186 |

</details>

<details>
<summary>String keys — Go / Rust, ns/op</summary>

`string` keys against Rust's `String`, each mixed key written as `user:` and 16 hex digits, with `uint64` values. Rust's containers move in keys made beforehand; Go's copy each key's bytes into their leaves, in buffers the collector doesn't scan. Rust replaces values with `update` and `get_mut`, and its churn inserts back the key its removal returned.

| Workload | 262K | 5M |
|---|---:|---:|
| Tree · Build, per insert | 130.1 / 103.4 (+26%) | 255.8 / 216.8 (+18%) |
| Tree · Read hit | 37.0 / 41.1 (-10%) | 106.6 / 188.4 (-43%) |
| Tree · Read miss | 34.8 / 33.5 (+4%) | 89.6 / 133.2 (-33%) |
| Tree · Replace value | 46.8 / 42.2 (+11%) | 170.7 / 181.1 (-6%) |
| Tree · Delete + reinsert same key | 152.7 / 153.2 (-0%) | 343.1 / 404.9 (-15%) |
| Map · Build, per insert | 152.8 / 109.8 (+39%) | 327.5 / 226.9 (+44%) |
| Map · Read hit | 55.5 / 44.3 (+25%) | 148.3 / 172.5 (-14%) |
| Map · Read miss | 50.6 / 35.9 (+41%) | 139.2 / 96.5 (+44%) |
| Map · Replace value | 91.2 / 51.1 (+78%) | 257.7 / 182.1 (+41%) |
| Map · Delete + reinsert same key | 208.4 / 158.3 (+32%) | 428.3 / 418.6 (+2%) |
| Map · 4 workers · reads | 14.1 / 12.0 (+18%) | 41.2 / 60.1 (-31%) |

</details>

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-29.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

<details>
<summary>Build the drivers and run the comparisons, from source checkpoint `7f9ff12`</summary>

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

Both scripts take `--keys mixed` (the default), `--keys sequential` or `--keys clustered`; the chart covers all three. `cpu_compare.py --text --mutations` runs the string-keyed cells, at 262K and 5M entries. `python3 internal/render_benchmarks.py` regenerates the chart and the tables above from the saved report.

</details>

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
