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

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.99.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max. Sources: [Go `2647c1e`](https://github.com/thesyncim/gobbtree/commit/2647c1eb5e86e303caad96f9b95feb84c619fac1), [Rust `1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). `uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results. Every workload runs with three key patterns: **mixed keys**, hashed keys spread over the full 64-bit range; **sequential keys**, consecutive even keys; misses and fresh keys fill the gaps; **clustered keys**, runs of 1,024 consecutive even keys at scattered bases.

![Change in Go operation time versus Rust across four sizes and multiple workloads, mixed keys](benchmarks/cpu-arm64.svg)

Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. Parallel rows use four workers; the mixed row replaces values on 10% of operations.

<details>
<summary>Exact timings, mixed keys — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.26 / 12.35 | 9.73 / 23.20 | 34.05 / 81.66 | 66.72 / 108.11 |
| Read miss | 8.47 / 11.79 | 12.50 / 22.44 | 40.42 / 64.23 | 76.78 / 103.59 |
| Batch hit · 8 keys | 10.82 / 11.97 | 12.73 / 22.77 | 31.18 / 49.85 | 55.15 / 66.39 |
| Batch miss · 8 keys | 10.57 / 11.77 | 12.31 / 20.30 | 27.57 / 39.51 | 49.22 / 55.40 |
| Replace value | 11.82 / 12.74 | 17.09 / 25.77 | 46.87 / 81.73 | 96.54 / 110.64 |
| Delete + reinsert same key | 41.71 / 71.58 | 64.59 / 106.19 | 146.17 / 226.54 | 263.49 / 309.78 |
| Delete + insert new key | 64.66 / 93.24 | 102.35 / 138.48 | 234.60 / 335.95 | 436.81 / 488.81 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.18 / 13.38 | 16.11 / 27.41 | 52.45 / 96.45 | 90.72 / 115.62 |
| Read miss | 10.45 / 12.31 | 15.12 / 24.80 | 33.15 / 65.25 | 72.36 / 100.05 |
| Batch hit · 8 keys | 10.96 / 13.87 | 14.82 / 21.70 | 33.17 / 50.35 | 48.59 / 68.24 |
| Batch miss · 8 keys | 11.35 / 12.52 | 13.74 / 19.27 | 27.22 / 38.96 | 44.88 / 55.41 |
| Replace value | 11.74 / 14.65 | 20.15 / 29.56 | 61.54 / 109.63 | 121.60 / 141.33 |
| Delete + reinsert same key | 48.76 / 71.64 | 65.98 / 105.90 | 168.09 / 230.30 | 265.88 / 311.88 |
| Delete + insert new key | 54.65 / 99.38 | 88.99 / 135.54 | 225.79 / 326.67 | 422.50 / 482.90 |
| 4 workers · reads | 2.77 / 3.81 | 4.61 / 7.27 | 15.13 / 27.77 | 26.54 / 37.94 |
| 4 workers · 10% writes | 4.73 / 4.96 | 4.86 / 7.26 | 18.21 / 29.56 | 28.04 / 42.18 |

</details>

![Go versus Rust peak RSS for Tree and Map at four sizes, mixed keys](benchmarks/memory-arm64.svg)

![Go versus Rust construction time for Tree and Map at four sizes, mixed keys](benchmarks/build-arm64.svg)

Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. Construction inserts every entry into an empty container.

<details>
<summary>Memory and build measurements, mixed keys — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.67 / 2.16 | 4.78 / 2.15 | 5.67 / 2.15 | 0.0004 / 0.0002 |
| Tree | 262K | 12.73 / 9.06 | 12.58 / 9.04 | 12.73 / 9.04 | 0.0290 / 0.0218 |
| Tree | 5M | 127.71 / 136.48 | 127.68 / 136.46 | 127.71 / 136.46 | 0.5480 / 0.6465 |
| Tree | 50M | 1,224.67 / 1,347.81 | 1,224.65 / 1,347.80 | 1,224.67 / 1,347.80 | 10.3231 / 10.3614 |
| Map | 4K | 5.75 / 2.16 | 4.93 / 2.15 | 5.75 / 2.15 | 0.0005 / 0.0003 |
| Map | 262K | 12.55 / 9.06 | 12.14 / 9.04 | 12.55 / 9.04 | 0.0299 / 0.0288 |
| Map | 5M | 132.64 / 136.51 | 132.58 / 136.50 | 132.64 / 136.50 | 0.6169 / 0.6581 |
| Map | 50M | 1,252.64 / 1,348.21 | 1,252.61 / 1,348.19 | 1,252.64 / 1,348.19 | 10.4859 / 10.4590 |

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
| Read hit | 5.43 / 11.83 | 8.37 / 23.90 | 33.55 / 76.75 | 83.11 / 95.79 |
| Read miss | 5.98 / 10.97 | 8.60 / 22.12 | 25.89 / 51.01 | 56.45 / 92.69 |
| Batch hit · 8 keys | 10.10 / 11.81 | 12.64 / 21.88 | 31.45 / 47.95 | 56.35 / 69.84 |
| Batch miss · 8 keys | 9.34 / 11.02 | 11.66 / 19.86 | 19.70 / 32.26 | 47.89 / 57.93 |
| Replace value | 11.34 / 12.41 | 15.85 / 25.59 | 43.81 / 80.67 | 93.97 / 117.58 |
| Delete + reinsert same key | 41.41 / 78.06 | 67.49 / 119.63 | 143.70 / 220.14 | 278.61 / 304.05 |
| Delete + insert new key | 46.02 / 63.55 | 80.14 / 114.44 | 179.14 / 232.96 | 316.07 / 296.38 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.27 / 13.05 | 14.40 / 28.45 | 50.71 / 93.79 | 111.67 / 109.95 |
| Read miss | 8.04 / 12.37 | 11.35 / 26.18 | 23.60 / 55.07 | 69.75 / 97.55 |
| Batch hit · 8 keys | 10.44 / 13.48 | 12.26 / 22.16 | 30.78 / 49.07 | 49.67 / 71.22 |
| Batch miss · 8 keys | 9.70 / 12.85 | 11.04 / 20.36 | 17.58 / 33.34 | 38.74 / 58.67 |
| Replace value | 10.69 / 15.39 | 18.27 / 31.79 | 58.77 / 102.29 | 117.39 / 156.05 |
| Delete + reinsert same key | 48.43 / 79.20 | 65.23 / 119.37 | 152.66 / 229.48 | 261.98 / 311.21 |
| Delete + insert new key | 41.53 / 84.46 | 60.66 / 113.25 | 159.22 / 243.68 | 279.65 / 298.08 |
| 4 workers · reads | 2.49 / 3.37 | 4.24 / 7.46 | 15.30 / 24.88 | 25.80 / 36.44 |
| 4 workers · 10% writes | 4.59 / 4.80 | 4.57 / 7.42 | 17.80 / 27.44 | 27.30 / 40.39 |

![Go versus Rust peak RSS, sequential keys](benchmarks/memory-sequential-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.57 / 2.16 | 4.78 / 2.15 | 5.57 / 2.15 | 0.0001 / 0.0000 |
| Tree | 262K | 11.29 / 8.26 | 10.90 / 8.24 | 11.29 / 8.24 | 0.0038 / 0.0035 |
| Tree | 5M | 105.74 / 120.24 | 105.71 / 120.23 | 105.74 / 120.23 | 0.0464 / 0.0459 |
| Tree | 50M | 1,008.94 / 1,184.04 | 1,008.91 / 1,184.02 | 1,008.94 / 1,184.02 | 0.3858 / 0.4036 |
| Map | 4K | 5.78 / 2.16 | 5.00 / 2.15 | 5.78 / 2.15 | 0.0002 / 0.0001 |
| Map | 262K | 11.99 / 8.26 | 11.53 / 8.24 | 11.99 / 8.24 | 0.0059 / 0.0046 |
| Map | 5M | 119.29 / 120.29 | 119.26 / 120.27 | 119.29 / 120.27 | 0.0667 / 0.0613 |
| Map | 50M | 1,136.87 / 1,184.48 | 1,136.82 / 1,184.46 | 1,136.87 / 1,184.46 | 0.5685 / 0.5131 |

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
| Read hit | 5.84 / 26.71 | 11.60 / 50.22 | 45.74 / 166.93 | 107.96 / 340.24 |
| Read miss | 6.86 / 25.71 | 13.43 / 46.13 | 45.21 / 142.89 | 95.59 / 337.05 |
| Batch hit · 8 keys | 17.12 / 24.89 | 34.24 / 42.85 | 78.44 / 132.32 | 108.42 / 206.51 |
| Batch miss · 8 keys | 16.93 / 24.27 | 31.70 / 40.96 | 67.27 / 118.47 | 101.13 / 199.04 |
| Replace value | 10.88 / 28.87 | 18.67 / 52.65 | 62.32 / 211.80 | 131.65 / 390.47 |
| Delete + reinsert same key | 52.40 / 118.16 | 92.06 / 171.18 | 196.56 / 440.08 | 333.47 / 752.26 |
| Delete + insert new key | 57.93 / 103.03 | 108.99 / 168.41 | 251.09 / 478.58 | 410.94 / 758.83 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.91 / 35.27 | 21.31 / 61.59 | 79.74 / 203.33 | 168.03 / 358.08 |
| Read miss | 8.54 / 32.81 | 18.24 / 58.81 | 48.36 / 183.91 | 128.76 / 338.86 |
| Batch hit · 8 keys | 10.80 / 31.75 | 18.62 / 47.54 | 51.34 / 152.61 | 107.89 / 251.69 |
| Batch miss · 8 keys | 10.09 / 25.65 | 17.44 / 46.03 | 37.99 / 141.53 | 99.40 / 240.88 |
| Replace value | 11.07 / 36.44 | 26.05 / 61.10 | 91.17 / 213.10 | 172.58 / 374.00 |
| Delete + reinsert same key | 49.86 / 118.25 | 73.86 / 163.35 | 213.57 / 375.26 | 332.13 / 631.93 |
| Delete + insert new key | 43.74 / 119.26 | 78.21 / 161.14 | 219.84 / 403.60 | 355.67 / 634.12 |
| 4 workers · reads | 2.65 / 11.15 | 5.80 / 15.62 | 21.42 / 52.57 | 41.64 / 92.02 |
| 4 workers · 10% writes | 4.80 / 8.76 | 6.23 / 15.62 | 25.81 / 54.57 | 42.48 / 94.18 |

![Go versus Rust peak RSS, clustered keys](benchmarks/memory-clustered-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.59 / 2.18 | 4.75 / 2.16 | 5.59 / 2.16 | 0.0002 / 0.0002 |
| Tree | 262K | 13.37 / 11.70 | 13.21 / 11.68 | 13.37 / 11.68 | 0.0162 / 0.0298 |
| Tree | 5M | 132.45 / 187.09 | 132.45 / 187.07 | 132.45 / 187.07 | 0.1897 / 0.3888 |
| Tree | 50M | 1,138.95 / 1,853.29 | 1,138.95 / 1,853.28 | 1,138.95 / 1,853.28 | 1.8220 / 3.9816 |
| Map | 4K | 5.77 / 2.18 | 4.83 / 2.16 | 5.77 / 2.16 | 0.0002 / 0.0002 |
| Map | 262K | 13.16 / 11.70 | 13.06 / 11.68 | 13.16 / 11.68 | 0.0181 / 0.0276 |
| Map | 5M | 141.52 / 187.11 | 141.51 / 187.09 | 141.52 / 187.09 | 0.2097 / 0.3755 |
| Map | 50M | 1,234.32 / 1,853.33 | 1,234.32 / 1,853.31 | 1,234.32 / 1,853.31 | 2.3825 / 3.8903 |

</details>

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-29.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

To reproduce, use source checkpoint `2647c1e` and build both drivers:

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
