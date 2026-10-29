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

`Map`s of other keys or values (strings, `MapOf` custom keys, values wider than a word) run the same protocol over nodes of any key and value. A separator is a boxed key, never changed once boxed, so a racing descent compares it whole; a text node skips the prefix its keys share, so its codes tell keys apart where they differ. A lookup reads its leaf without latching it when codes settle the key: integer and float keys, and text keys with a single candidate, whose key header and value it copies racing and uses only once the leaf's version holds. Upstream copies only `Copy` values that way; Go's collector keeps alive whatever a torn copy points at, and the copy is dropped unread, so any value can be. Other lookups, and custom keys, take the leaf's shared latch, as upstream does for keys it can't read inline. Freed nodes go to the collector, which keeps one alive while a stale pointer reaches it; its version, bumped as it is freed, fails that pointer's check.

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

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.99.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max. Sources: [Go `7ff4098`](https://github.com/thesyncim/gobbtree/commit/7ff4098e7f75bb23fa7e1496d2981147e3e4c3c5), [Rust `1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). `uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results. Every workload runs with three key patterns: **mixed keys**, hashed keys spread over the full 64-bit range; **sequential keys**, consecutive even keys; misses and fresh keys fill the gaps; **clustered keys**, runs of 1,024 consecutive even keys at scattered bases.

![Change in Go operation time versus Rust across four sizes and multiple workloads, mixed keys](benchmarks/cpu-arm64.svg)

Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. Parallel rows use four workers; the mixed row replaces values on 10% of operations.

<details>
<summary>Exact timings, mixed keys — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.26 / 12.30 | 9.80 / 23.20 | 33.36 / 81.49 | 66.00 / 110.01 |
| Read miss | 8.57 / 11.60 | 11.94 / 22.12 | 40.14 / 63.85 | 74.68 / 102.65 |
| Batch hit · 8 keys | 10.99 / 12.04 | 12.75 / 22.67 | 31.27 / 49.95 | 54.56 / 67.76 |
| Batch miss · 8 keys | 10.79 / 11.85 | 12.40 / 20.32 | 27.79 / 38.63 | 49.95 / 55.86 |
| Replace value | 11.85 / 12.99 | 16.66 / 25.86 | 50.13 / 83.60 | 92.10 / 113.47 |
| Delete + reinsert same key | 32.48 / 72.82 | 48.44 / 108.93 | 121.34 / 228.22 | 225.26 / 308.88 |
| Delete + insert new key | 54.97 / 94.84 | 87.54 / 141.53 | 217.68 / 338.20 | 386.56 / 482.44 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.19 / 13.51 | 16.27 / 27.75 | 52.33 / 98.17 | 90.62 / 117.55 |
| Read miss | 10.44 / 12.41 | 14.79 / 25.05 | 34.47 / 66.35 | 73.08 / 103.81 |
| Batch hit · 8 keys | 11.04 / 13.61 | 14.22 / 21.62 | 32.47 / 51.61 | 49.26 / 66.28 |
| Batch miss · 8 keys | 11.33 / 12.73 | 13.69 / 19.24 | 27.05 / 38.84 | 44.69 / 60.17 |
| Replace value | 11.88 / 14.70 | 20.68 / 29.16 | 62.17 / 113.07 | 120.30 / 144.51 |
| Delete + reinsert same key | 49.42 / 72.46 | 66.13 / 106.63 | 161.19 / 230.13 | 267.08 / 317.56 |
| Delete + insert new key | 54.97 / 101.36 | 88.38 / 137.38 | 229.16 / 330.68 | 425.35 / 489.47 |
| 4 workers · reads | 2.74 / 3.82 | 4.73 / 7.39 | 16.07 / 28.01 | 26.45 / 38.38 |
| 4 workers · 10% writes | 4.88 / 5.00 | 5.04 / 7.19 | 18.06 / 29.45 | 28.34 / 42.72 |

</details>

![Go versus Rust peak RSS for Tree and Map at four sizes, mixed keys](benchmarks/memory-arm64.svg)

![Go versus Rust construction time for Tree and Map at four sizes, mixed keys](benchmarks/build-arm64.svg)

Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. Construction inserts every entry into an empty container.

<details>
<summary>Memory and build measurements, mixed keys — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.60 / 2.16 | 4.83 / 2.15 | 5.60 / 2.15 | 0.0007 / 0.0004 |
| Tree | 262K | 12.65 / 9.06 | 12.57 / 9.04 | 12.65 / 9.04 | 0.0292 / 0.0266 |
| Tree | 5M | 127.81 / 136.48 | 127.76 / 136.46 | 127.81 / 136.46 | 0.5694 / 0.6364 |
| Tree | 50M | 1,225.87 / 1,347.81 | 1,225.85 / 1,347.80 | 1,225.87 / 1,347.80 | 10.3633 / 10.3517 |
| Map | 4K | 5.64 / 2.16 | 4.87 / 2.15 | 5.64 / 2.15 | 0.0005 / 0.0004 |
| Map | 262K | 12.42 / 9.06 | 12.14 / 9.04 | 12.42 / 9.04 | 0.0288 / 0.0301 |
| Map | 5M | 132.53 / 136.51 | 132.53 / 136.50 | 132.53 / 136.50 | 0.6097 / 0.6463 |
| Map | 50M | 1,252.72 / 1,348.21 | 1,252.66 / 1,348.19 | 1,252.72 / 1,348.19 | 10.5715 / 10.5016 |

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
| Read hit | 5.37 / 11.84 | 8.38 / 23.90 | 33.67 / 74.53 | 82.62 / 94.69 |
| Read miss | 6.06 / 10.98 | 8.94 / 22.13 | 29.45 / 52.36 | 62.16 / 92.40 |
| Batch hit · 8 keys | 10.18 / 11.86 | 12.62 / 21.86 | 33.45 / 47.36 | 55.88 / 69.85 |
| Batch miss · 8 keys | 9.39 / 11.04 | 11.69 / 19.82 | 18.76 / 32.35 | 46.67 / 58.79 |
| Replace value | 11.33 / 12.44 | 15.13 / 25.55 | 44.92 / 84.37 | 89.47 / 119.93 |
| Delete + reinsert same key | 33.90 / 79.08 | 49.76 / 119.11 | 120.64 / 223.44 | 224.69 / 303.50 |
| Delete + insert new key | 34.08 / 63.27 | 54.87 / 114.46 | 145.19 / 230.52 | 262.42 / 292.18 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.28 / 12.99 | 14.22 / 28.45 | 51.12 / 97.95 | 110.26 / 114.24 |
| Read miss | 8.03 / 12.36 | 11.40 / 26.23 | 24.98 / 58.46 | 67.66 / 95.40 |
| Batch hit · 8 keys | 10.23 / 13.58 | 12.60 / 22.23 | 30.74 / 50.92 | 48.50 / 71.28 |
| Batch miss · 8 keys | 9.55 / 12.86 | 10.99 / 20.39 | 19.32 / 33.80 | 39.52 / 59.12 |
| Replace value | 10.82 / 15.43 | 19.11 / 31.59 | 59.32 / 106.85 | 118.04 / 157.64 |
| Delete + reinsert same key | 48.29 / 79.56 | 64.74 / 120.35 | 160.95 / 227.71 | 264.95 / 308.93 |
| Delete + insert new key | 40.72 / 84.58 | 60.35 / 113.52 | 163.01 / 242.96 | 275.14 / 301.28 |
| 4 workers · reads | 2.47 / 3.36 | 4.22 / 7.43 | 14.60 / 24.91 | 26.03 / 35.95 |
| 4 workers · 10% writes | 4.47 / 4.83 | 4.59 / 7.45 | 17.50 / 28.42 | 27.77 / 40.99 |

![Go versus Rust peak RSS, sequential keys](benchmarks/memory-sequential-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.52 / 2.16 | 4.70 / 2.15 | 5.52 / 2.15 | 0.0001 / 0.0001 |
| Tree | 262K | 11.22 / 8.26 | 10.90 / 8.24 | 11.22 / 8.24 | 0.0038 / 0.0034 |
| Tree | 5M | 105.69 / 120.24 | 105.68 / 120.23 | 105.69 / 120.23 | 0.0453 / 0.0488 |
| Tree | 50M | 1,008.04 / 1,184.04 | 1,008.01 / 1,184.02 | 1,008.04 / 1,184.02 | 0.3838 / 0.4059 |
| Map | 4K | 5.72 / 2.16 | 5.01 / 2.15 | 5.72 / 2.15 | 0.0001 / 0.0001 |
| Map | 262K | 11.93 / 8.26 | 11.55 / 8.24 | 11.93 / 8.24 | 0.0062 / 0.0046 |
| Map | 5M | 119.26 / 120.29 | 119.26 / 120.27 | 119.26 / 120.27 | 0.0672 / 0.0504 |
| Map | 50M | 1,136.87 / 1,184.48 | 1,136.79 / 1,184.46 | 1,136.87 / 1,184.46 | 0.5404 / 0.5091 |

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
| Read hit | 5.86 / 26.70 | 11.71 / 49.18 | 46.32 / 164.97 | 109.28 / 336.52 |
| Read miss | 6.74 / 25.50 | 13.27 / 47.21 | 45.33 / 142.90 | 99.27 / 333.85 |
| Batch hit · 8 keys | 17.19 / 24.95 | 30.13 / 42.80 | 78.94 / 130.09 | 109.98 / 205.63 |
| Batch miss · 8 keys | 16.91 / 23.18 | 28.07 / 40.96 | 69.80 / 120.21 | 102.34 / 197.73 |
| Replace value | 10.88 / 27.59 | 18.39 / 52.53 | 61.49 / 211.03 | 133.81 / 394.20 |
| Delete + reinsert same key | 34.52 / 117.30 | 53.53 / 174.45 | 152.12 / 439.60 | 286.97 / 756.33 |
| Delete + insert new key | 36.58 / 102.69 | 61.66 / 169.63 | 187.16 / 480.58 | 325.69 / 761.65 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.96 / 36.43 | 21.62 / 61.77 | 83.41 / 206.94 | 165.91 / 360.66 |
| Read miss | 8.55 / 33.03 | 18.28 / 57.00 | 46.71 / 185.43 | 129.53 / 339.21 |
| Batch hit · 8 keys | 10.69 / 27.11 | 18.98 / 47.99 | 50.89 / 153.86 | 106.96 / 252.14 |
| Batch miss · 8 keys | 9.90 / 33.80 | 17.52 / 44.88 | 37.71 / 142.88 | 100.27 / 239.92 |
| Replace value | 11.51 / 42.01 | 25.80 / 61.58 | 92.39 / 212.93 | 177.22 / 374.06 |
| Delete + reinsert same key | 50.06 / 120.83 | 73.84 / 165.75 | 211.78 / 375.29 | 333.62 / 635.89 |
| Delete + insert new key | 43.99 / 119.82 | 77.58 / 163.08 | 229.61 / 394.65 | 353.77 / 632.77 |
| 4 workers · reads | 2.66 / 10.00 | 5.84 / 16.18 | 21.97 / 52.47 | 41.54 / 92.34 |
| 4 workers · 10% writes | 5.45 / 9.12 | 6.14 / 16.00 | 25.96 / 55.09 | 42.20 / 94.24 |

![Go versus Rust peak RSS, clustered keys](benchmarks/memory-clustered-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.72 / 2.18 | 4.78 / 2.16 | 5.72 / 2.16 | 0.0001 / 0.0001 |
| Tree | 262K | 13.34 / 11.70 | 13.24 / 11.68 | 13.34 / 11.68 | 0.0159 / 0.0280 |
| Tree | 5M | 132.53 / 187.09 | 132.53 / 187.07 | 132.53 / 187.07 | 0.1870 / 0.3891 |
| Tree | 50M | 1,139.03 / 1,853.29 | 1,139.03 / 1,853.28 | 1,139.03 / 1,853.28 | 1.8003 / 3.9650 |
| Map | 4K | 5.73 / 2.18 | 4.88 / 2.16 | 5.73 / 2.16 | 0.0003 / 0.0002 |
| Map | 262K | 13.22 / 11.70 | 13.11 / 11.68 | 13.22 / 11.68 | 0.0192 / 0.0259 |
| Map | 5M | 141.67 / 187.09 | 141.62 / 187.07 | 141.67 / 187.07 | 0.2113 / 0.3682 |
| Map | 50M | 1,234.40 / 1,853.33 | 1,234.39 / 1,853.31 | 1,234.40 / 1,853.31 | 2.3810 / 3.8540 |

</details>

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-29.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

To reproduce, use source checkpoint `7ff4098` and build both drivers:

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
