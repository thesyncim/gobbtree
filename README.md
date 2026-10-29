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

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.99.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max. Sources: [Go `7087d76`](https://github.com/thesyncim/gobbtree/commit/7087d76923bf5a1795029b64bf5a6223b0dde3d4), [Rust `1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). `uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results. Every workload runs with three key patterns: **mixed keys**, hashed keys spread over the full 64-bit range; **sequential keys**, consecutive even keys; misses and fresh keys fill the gaps; **clustered keys**, runs of 1,024 consecutive even keys at scattered bases.

![Change in Go operation time versus Rust across four sizes and multiple workloads, mixed keys](benchmarks/cpu-arm64.svg)

Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. Parallel rows use four workers; the mixed row replaces values on 10% of operations.

<details>
<summary>Exact timings, mixed keys — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.16 / 12.26 | 9.84 / 23.25 | 33.72 / 81.53 | 65.88 / 112.95 |
| Read miss | 8.24 / 11.60 | 11.90 / 21.79 | 37.43 / 66.23 | 74.69 / 104.09 |
| Batch hit · 8 keys | 10.86 / 11.79 | 12.73 / 22.64 | 31.35 / 49.92 | 53.65 / 67.23 |
| Batch miss · 8 keys | 10.90 / 11.68 | 12.42 / 20.32 | 27.33 / 39.41 | 48.60 / 56.95 |
| Replace value | 11.57 / 12.57 | 16.46 / 25.32 | 49.05 / 83.10 | 92.27 / 111.77 |
| Delete + reinsert same key | 32.56 / 70.61 | 47.95 / 108.34 | 121.81 / 232.32 | 226.25 / 313.76 |
| Delete + insert new key | 56.24 / 92.56 | 85.80 / 140.94 | 215.44 / 334.54 | 389.89 / 494.71 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.18 / 13.32 | 16.13 / 27.34 | 50.13 / 96.51 | 92.03 / 112.19 |
| Read miss | 10.46 / 12.26 | 14.78 / 24.77 | 34.86 / 66.96 | 73.61 / 101.65 |
| Batch hit · 8 keys | 11.03 / 13.31 | 14.48 / 21.81 | 32.26 / 51.17 | 49.11 / 67.18 |
| Batch miss · 8 keys | 11.46 / 12.57 | 13.86 / 19.51 | 27.24 / 40.48 | 45.35 / 56.21 |
| Replace value | 11.84 / 14.52 | 20.60 / 29.32 | 62.97 / 109.33 | 121.17 / 146.89 |
| Delete + reinsert same key | 49.30 / 72.27 | 67.01 / 107.51 | 160.03 / 232.62 | 266.04 / 321.26 |
| Delete + insert new key | 54.84 / 98.31 | 89.56 / 137.66 | 233.57 / 328.17 | 433.49 / 488.58 |
| 4 workers · reads | 2.74 / 3.80 | 4.56 / 7.39 | 16.42 / 27.81 | 26.59 / 38.27 |
| 4 workers · 10% writes | 4.66 / 4.94 | 5.08 / 7.43 | 18.20 / 29.40 | 28.10 / 42.14 |

</details>

![Go versus Rust peak RSS for Tree and Map at four sizes, mixed keys](benchmarks/memory-arm64.svg)

![Go versus Rust construction time for Tree and Map at four sizes, mixed keys](benchmarks/build-arm64.svg)

Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. Construction inserts every entry into an empty container.

<details>
<summary>Memory and build measurements, mixed keys — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.64 / 2.15 | 4.87 / 2.13 | 5.64 / 2.13 | 0.0005 / 0.0004 |
| Tree | 262K | 12.50 / 9.04 | 12.42 / 9.03 | 12.50 / 9.03 | 0.0276 / 0.0276 |
| Tree | 5M | 127.84 / 136.46 | 127.84 / 136.45 | 127.84 / 136.45 | 0.5664 / 0.6592 |
| Tree | 50M | 1,225.92 / 1,347.80 | 1,225.88 / 1,347.78 | 1,225.92 / 1,347.78 | 10.5388 / 10.6067 |
| Map | 4K | 5.67 / 2.15 | 4.73 / 2.13 | 5.67 / 2.13 | 0.0005 / 0.0004 |
| Map | 262K | 12.40 / 9.04 | 12.03 / 9.03 | 12.40 / 9.03 | 0.0314 / 0.0257 |
| Map | 5M | 132.58 / 136.50 | 132.56 / 136.48 | 132.58 / 136.48 | 0.6232 / 0.6717 |
| Map | 50M | 1,252.49 / 1,348.19 | 1,252.46 / 1,348.17 | 1,252.49 / 1,348.17 | 10.7743 / 10.8320 |

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
| Read hit | 5.36 / 11.87 | 8.39 / 24.04 | 33.92 / 76.33 | 84.68 / 96.91 |
| Read miss | 5.79 / 11.01 | 8.65 / 22.21 | 26.77 / 51.79 | 55.38 / 97.57 |
| Batch hit · 8 keys | 10.13 / 11.84 | 12.63 / 21.90 | 31.63 / 48.42 | 56.38 / 72.61 |
| Batch miss · 8 keys | 9.71 / 10.98 | 11.71 / 19.89 | 19.22 / 32.87 | 47.68 / 57.51 |
| Replace value | 11.07 / 12.33 | 15.00 / 25.38 | 44.16 / 82.55 | 88.70 / 124.33 |
| Delete + reinsert same key | 34.16 / 78.50 | 49.85 / 120.90 | 120.01 / 228.79 | 236.65 / 318.11 |
| Delete + insert new key | 34.21 / 63.74 | 56.20 / 115.29 | 146.09 / 239.67 | 265.30 / 305.58 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.19 / 13.49 | 14.22 / 28.28 | 52.05 / 93.23 | 110.69 / 113.18 |
| Read miss | 8.04 / 12.55 | 11.43 / 26.20 | 23.74 / 57.72 | 69.50 / 95.70 |
| Batch hit · 8 keys | 10.26 / 13.66 | 12.57 / 22.32 | 31.71 / 49.84 | 49.33 / 72.90 |
| Batch miss · 8 keys | 9.75 / 12.94 | 11.11 / 20.38 | 19.16 / 34.86 | 39.73 / 58.81 |
| Replace value | 10.85 / 14.49 | 19.11 / 31.58 | 61.62 / 107.62 | 120.99 / 154.42 |
| Delete + reinsert same key | 48.73 / 79.02 | 65.34 / 119.34 | 160.85 / 231.98 | 268.30 / 314.68 |
| Delete + insert new key | 40.82 / 84.08 | 60.82 / 114.06 | 164.48 / 245.86 | 278.70 / 306.64 |
| 4 workers · reads | 2.48 / 3.39 | 4.25 / 7.46 | 15.32 / 25.25 | 26.04 / 36.04 |
| 4 workers · 10% writes | 4.38 / 4.91 | 4.52 / 7.44 | 17.84 / 27.87 | 27.51 / 40.86 |

![Go versus Rust peak RSS, sequential keys](benchmarks/memory-sequential-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.59 / 2.15 | 4.75 / 2.13 | 5.59 / 2.13 | 0.0000 / 0.0001 |
| Tree | 262K | 11.34 / 8.24 | 10.96 / 8.22 | 11.34 / 8.22 | 0.0027 / 0.0037 |
| Tree | 5M | 105.76 / 120.23 | 105.74 / 120.21 | 105.76 / 120.21 | 0.0502 / 0.0475 |
| Tree | 50M | 1,008.81 / 1,184.02 | 1,008.81 / 1,184.01 | 1,008.81 / 1,184.01 | 0.4044 / 0.4133 |
| Map | 4K | 5.65 / 2.15 | 4.80 / 2.13 | 5.65 / 2.13 | 0.0001 / 0.0001 |
| Map | 262K | 11.81 / 8.24 | 11.45 / 8.22 | 11.81 / 8.22 | 0.0058 / 0.0051 |
| Map | 5M | 119.31 / 120.27 | 119.29 / 120.26 | 119.31 / 120.26 | 0.0547 / 0.0623 |
| Map | 50M | 1,136.77 / 1,184.46 | 1,136.71 / 1,184.45 | 1,136.77 / 1,184.45 | 0.5424 / 0.5166 |

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
| Read hit | 5.78 / 26.75 | 11.69 / 47.81 | 46.83 / 170.75 | 109.48 / 340.34 |
| Read miss | 6.76 / 25.56 | 13.30 / 45.81 | 47.37 / 146.74 | 100.80 / 343.90 |
| Batch hit · 8 keys | 17.60 / 24.99 | 28.68 / 44.10 | 78.71 / 134.32 | 112.83 / 208.88 |
| Batch miss · 8 keys | 17.41 / 23.64 | 28.88 / 41.26 | 69.90 / 121.31 | 104.74 / 199.78 |
| Replace value | 10.77 / 28.48 | 18.44 / 51.45 | 63.35 / 222.31 | 133.58 / 395.65 |
| Delete + reinsert same key | 35.10 / 118.90 | 53.74 / 177.21 | 159.16 / 457.36 | 289.42 / 766.90 |
| Delete + insert new key | 36.87 / 103.59 | 62.70 / 173.21 | 198.22 / 497.06 | 332.25 / 764.52 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.93 / 36.41 | 21.46 / 61.86 | 81.10 / 205.12 | 172.48 / 364.97 |
| Read miss | 8.54 / 33.18 | 18.36 / 60.36 | 48.59 / 190.38 | 131.68 / 342.63 |
| Batch hit · 8 keys | 10.53 / 27.82 | 18.81 / 49.14 | 53.13 / 156.34 | 112.63 / 254.81 |
| Batch miss · 8 keys | 9.81 / 28.18 | 17.42 / 46.33 | 39.08 / 142.37 | 103.47 / 243.86 |
| Replace value | 11.49 / 38.99 | 26.30 / 61.25 | 100.13 / 214.79 | 179.71 / 378.00 |
| Delete + reinsert same key | 50.69 / 121.60 | 74.30 / 163.94 | 217.24 / 388.20 | 335.44 / 646.08 |
| Delete + insert new key | 43.81 / 120.50 | 77.91 / 164.07 | 230.83 / 404.47 | 363.34 / 648.49 |
| 4 workers · reads | 2.67 / 9.50 | 5.72 / 16.52 | 22.42 / 54.05 | 42.14 / 92.30 |
| 4 workers · 10% writes | 4.69 / 9.23 | 6.49 / 16.09 | 26.17 / 56.77 | 43.00 / 94.89 |

![Go versus Rust peak RSS, clustered keys](benchmarks/memory-clustered-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.57 / 2.16 | 4.78 / 2.15 | 5.57 / 2.15 | 0.0001 / 0.0002 |
| Tree | 262K | 13.21 / 11.68 | 13.06 / 11.67 | 13.21 / 11.67 | 0.0160 / 0.0299 |
| Tree | 5M | 132.33 / 187.09 | 132.33 / 187.07 | 132.33 / 187.07 | 0.1883 / 0.3960 |
| Tree | 50M | 1,139.06 / 1,853.28 | 1,139.06 / 1,853.26 | 1,139.06 / 1,853.26 | 1.8292 / 4.1002 |
| Map | 4K | 5.77 / 2.16 | 4.83 / 2.15 | 5.77 / 2.15 | 0.0002 / 0.0002 |
| Map | 262K | 13.17 / 11.70 | 13.07 / 11.68 | 13.17 / 11.68 | 0.0189 / 0.0274 |
| Map | 5M | 141.46 / 187.09 | 141.46 / 187.07 | 141.46 / 187.07 | 0.2173 / 0.3796 |
| Map | 50M | 1,234.09 / 1,853.31 | 1,234.09 / 1,853.29 | 1,234.09 / 1,853.29 | 2.4078 / 3.9613 |

</details>

### String keys

`string` keys against Rust's `String`, each mixed key written as `user:` and 16 hex digits, with `uint64` values. Rust's containers own their keys and Go's share the caller's immutable strings, so no timed operation allocates a key on either side: the build moves keys made beforehand, Rust replaces values with `update` and `get_mut`, and its churn inserts back the key its removal returned. Go's `Tree` runs string keys on the `Map`'s tree, as its single writer: its lookups and writes take no latches.

| Workload | 262K: Go / Rust, ns | 5M: Go / Rust, ns |
|---|---:|---:|
| Tree · Build, per insert | 107.2 / 93.1 (+15%) | 230.4 / 203.0 (+14%) |
| Tree · Read hit | 37.6 / 39.0 (-4%) | 109.8 / 182.7 (-40%) |
| Tree · Read miss | 34.0 / 32.5 (+5%) | 90.2 / 123.9 (-27%) |
| Tree · Replace value | 46.4 / 40.1 (+16%) | 163.9 / 179.1 (-8%) |
| Tree · Delete + reinsert same key | 153.0 / 143.8 (+6%) | 336.4 / 386.6 (-13%) |
| Map · Build, per insert | 134.8 / 103.5 (+30%) | 297.8 / 213.8 (+39%) |
| Map · Read hit | 55.9 / 43.9 (+28%) | 154.6 / 168.3 (-8%) |
| Map · Read miss | 51.5 / 34.4 (+50%) | 140.0 / 93.3 (+50%) |
| Map · Replace value | 90.4 / 47.7 (+89%) | 275.3 / 188.6 (+46%) |
| Map · Delete + reinsert same key | 196.7 / 149.6 (+31%) | 407.8 / 411.3 (-1%) |
| Map · 4 workers · reads | 15.0 / 11.8 (+27%) | 44.9 / 64.7 (-31%) |

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-29.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

To reproduce, use source checkpoint `7087d76` and build both drivers:

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
