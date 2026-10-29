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

Go 1.27.3 with `GOEXPERIMENT=simd` against the Rust original (1.99.1, native CPU tuning, full LTO, mimalloc) on an Apple M4 Max. Sources: [Go `4aa441c`](https://github.com/thesyncim/gobbtree/commit/4aa441c1600f5bb55c0dcbefbde8057ede0a248e), [Rust `1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). `uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results. Every workload runs with three key patterns: **mixed keys**, hashed keys spread over the full 64-bit range; **sequential keys**, consecutive even keys; misses and fresh keys fill the gaps; **clustered keys**, runs of 1,024 consecutive even keys at scattered bases.

![Change in Go operation time versus Rust across four sizes and multiple workloads, mixed keys](benchmarks/cpu-arm64.svg)

Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. Parallel rows use four workers; the mixed row replaces values on 10% of operations.

<details>
<summary>Exact timings, mixed keys — Go / Rust, ns/op; lower is better</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.34 / 12.39 | 9.86 / 23.56 | 34.08 / 80.95 | 65.94 / 110.92 |
| Read miss | 8.63 / 11.59 | 12.04 / 22.24 | 38.77 / 65.25 | 76.89 / 106.04 |
| Batch hit · 8 keys | 10.90 / 12.21 | 12.73 / 22.71 | 32.44 / 50.96 | 55.94 / 69.30 |
| Batch miss · 8 keys | 10.64 / 11.78 | 12.36 / 20.52 | 27.08 / 40.81 | 47.85 / 58.48 |
| Replace value | 12.00 / 12.88 | 17.64 / 26.34 | 48.43 / 84.03 | 92.29 / 109.72 |
| Delete + reinsert same key | 42.32 / 72.60 | 65.76 / 108.38 | 154.38 / 229.58 | 270.22 / 304.24 |
| Delete + insert new key | 65.30 / 94.73 | 103.72 / 141.53 | 233.34 / 340.50 | 438.79 / 487.66 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.15 / 13.59 | 16.20 / 28.41 | 52.28 / 96.67 | 94.45 / 113.18 |
| Read miss | 10.45 / 12.40 | 14.86 / 25.07 | 35.12 / 66.29 | 77.78 / 100.77 |
| Batch hit · 8 keys | 11.02 / 13.64 | 14.26 / 21.75 | 33.59 / 50.94 | 49.14 / 66.19 |
| Batch miss · 8 keys | 11.38 / 12.77 | 13.79 / 19.24 | 26.90 / 39.34 | 45.01 / 55.48 |
| Replace value | 11.69 / 14.64 | 20.23 / 29.34 | 61.08 / 110.33 | 122.31 / 143.83 |
| Delete + reinsert same key | 49.09 / 72.67 | 66.54 / 107.70 | 163.82 / 231.53 | 266.01 / 324.66 |
| Delete + insert new key | 55.64 / 101.02 | 89.60 / 138.52 | 234.71 / 334.64 | 431.42 / 484.60 |
| 4 workers · reads | 2.73 / 4.03 | 4.42 / 7.36 | 15.68 / 28.09 | 26.54 / 37.60 |
| 4 workers · 10% writes | 4.78 / 5.04 | 5.00 / 7.26 | 18.07 / 29.33 | 28.27 / 41.99 |

</details>

![Go versus Rust peak RSS for Tree and Map at four sizes, mixed keys](benchmarks/memory-arm64.svg)

![Go versus Rust construction time for Tree and Map at four sizes, mixed keys](benchmarks/build-arm64.svg)

Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. Construction inserts every entry into an empty container.

<details>
<summary>Memory and build measurements, mixed keys — each cell is Go / Rust</summary>

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.73 / 2.16 | 4.92 / 2.15 | 5.73 / 2.15 | 0.0006 / 0.0004 |
| Tree | 262K | 12.55 / 9.06 | 12.44 / 9.04 | 12.55 / 9.04 | 0.0281 / 0.0285 |
| Tree | 5M | 127.94 / 136.48 | 127.93 / 136.46 | 127.94 / 136.46 | 0.5649 / 0.6237 |
| Tree | 50M | 1,226.11 / 1,347.81 | 1,226.06 / 1,347.80 | 1,226.11 / 1,347.80 | 10.4141 / 10.4344 |
| Map | 4K | 5.69 / 2.16 | 4.93 / 2.15 | 5.69 / 2.15 | 0.0006 / 0.0004 |
| Map | 262K | 12.47 / 9.06 | 12.16 / 9.04 | 12.47 / 9.04 | 0.0317 / 0.0295 |
| Map | 5M | 132.53 / 136.51 | 132.50 / 136.50 | 132.53 / 136.50 | 0.6670 / 0.6442 |
| Map | 50M | 1,252.33 / 1,348.21 | 1,252.28 / 1,348.19 | 1,252.33 / 1,348.19 | 13.0256 / 10.5533 |

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
| Read hit | 5.38 / 11.84 | 8.38 / 24.04 | 33.86 / 76.52 | 86.70 / 97.41 |
| Read miss | 6.02 / 11.00 | 8.94 / 22.09 | 28.62 / 51.11 | 62.59 / 93.50 |
| Batch hit · 8 keys | 10.09 / 11.85 | 12.63 / 21.88 | 31.56 / 47.61 | 58.78 / 69.89 |
| Batch miss · 8 keys | 9.35 / 11.02 | 11.63 / 19.79 | 19.38 / 32.06 | 47.82 / 56.76 |
| Replace value | 11.53 / 12.46 | 16.50 / 25.61 | 43.11 / 84.61 | 92.49 / 119.94 |
| Delete + reinsert same key | 41.37 / 78.66 | 68.18 / 120.32 | 146.94 / 230.99 | 281.85 / 301.62 |
| Delete + insert new key | 45.85 / 63.75 | 80.97 / 114.39 | 174.41 / 233.82 | 317.63 / 293.28 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.28 / 12.98 | 14.37 / 28.80 | 50.93 / 92.37 | 114.25 / 114.60 |
| Read miss | 8.02 / 12.37 | 11.39 / 26.27 | 23.09 / 54.81 | 68.29 / 94.61 |
| Batch hit · 8 keys | 10.13 / 13.57 | 12.46 / 22.22 | 30.96 / 48.70 | 49.36 / 71.24 |
| Batch miss · 8 keys | 9.62 / 12.84 | 11.06 / 20.40 | 17.34 / 36.35 | 39.18 / 58.39 |
| Replace value | 10.74 / 15.43 | 18.34 / 31.73 | 59.07 / 108.60 | 117.69 / 155.13 |
| Delete + reinsert same key | 48.57 / 79.24 | 64.91 / 118.79 | 159.32 / 232.46 | 264.56 / 313.29 |
| Delete + insert new key | 40.71 / 84.35 | 60.78 / 112.76 | 161.25 / 244.97 | 274.29 / 303.47 |
| 4 workers · reads | 2.47 / 3.36 | 4.23 / 7.43 | 14.51 / 25.14 | 25.96 / 36.54 |
| 4 workers · 10% writes | 4.47 / 4.82 | 4.58 / 7.46 | 17.63 / 27.92 | 27.22 / 41.04 |

![Go versus Rust peak RSS, sequential keys](benchmarks/memory-sequential-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.47 / 2.16 | 4.62 / 2.15 | 5.47 / 2.15 | 0.0000 / 0.0001 |
| Tree | 262K | 11.19 / 8.26 | 10.85 / 8.24 | 11.19 / 8.24 | 0.0033 / 0.0025 |
| Tree | 5M | 105.68 / 120.24 | 105.64 / 120.23 | 105.68 / 120.23 | 0.0414 / 0.0452 |
| Tree | 50M | 1,008.93 / 1,184.04 | 1,008.91 / 1,184.02 | 1,008.93 / 1,184.02 | 0.3870 / 0.4106 |
| Map | 4K | 5.72 / 2.16 | 4.90 / 2.15 | 5.72 / 2.15 | 0.0001 / 0.0001 |
| Map | 262K | 11.93 / 8.26 | 11.53 / 8.24 | 11.93 / 8.24 | 0.0060 / 0.0048 |
| Map | 5M | 119.31 / 120.29 | 119.24 / 120.27 | 119.31 / 120.27 | 0.0679 / 0.0607 |
| Map | 50M | 1,136.84 / 1,184.48 | 1,136.69 / 1,184.46 | 1,136.84 / 1,184.46 | 0.5554 / 0.5120 |

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
| Read hit | 5.83 / 27.09 | 11.61 / 49.51 | 46.36 / 168.74 | 109.44 / 340.55 |
| Read miss | 6.79 / 26.01 | 13.28 / 46.00 | 45.46 / 145.84 | 99.38 / 338.40 |
| Batch hit · 8 keys | 16.44 / 25.01 | 29.25 / 42.90 | 78.73 / 133.00 | 108.66 / 206.57 |
| Batch miss · 8 keys | 16.86 / 23.75 | 27.43 / 41.18 | 68.64 / 118.83 | 102.59 / 199.36 |
| Replace value | 11.21 / 28.51 | 19.20 / 53.66 | 64.98 / 215.56 | 133.79 / 397.91 |
| Delete + reinsert same key | 51.51 / 116.84 | 92.27 / 174.58 | 197.72 / 440.80 | 352.03 / 759.55 |
| Delete + insert new key | 57.98 / 102.86 | 107.71 / 170.09 | 254.24 / 484.86 | 407.41 / 763.09 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.98 / 37.65 | 24.78 / 62.02 | 86.74 / 203.30 | 166.24 / 364.58 |
| Read miss | 8.48 / 33.77 | 21.84 / 58.01 | 54.11 / 185.07 | 127.90 / 342.87 |
| Batch hit · 8 keys | 10.55 / 28.15 | 22.61 / 47.33 | 60.12 / 155.22 | 109.22 / 252.94 |
| Batch miss · 8 keys | 10.11 / 26.10 | 21.22 / 47.09 | 45.50 / 139.07 | 98.54 / 241.33 |
| Replace value | 11.06 / 36.55 | 29.40 / 61.00 | 99.20 / 212.47 | 175.52 / 377.27 |
| Delete + reinsert same key | 49.80 / 117.81 | 77.16 / 164.54 | 206.87 / 375.27 | 334.11 / 642.62 |
| Delete + insert new key | 43.78 / 119.45 | 83.68 / 160.61 | 221.25 / 401.98 | 354.86 / 635.51 |
| 4 workers · reads | 2.66 / 9.81 | 6.59 / 16.05 | 24.50 / 53.12 | 41.49 / 91.47 |
| 4 workers · 10% writes | 4.53 / 8.81 | 7.02 / 15.39 | 26.87 / 54.97 | 43.43 / 94.03 |

![Go versus Rust peak RSS, clustered keys](benchmarks/memory-clustered-arm64.svg)

| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.69 / 2.18 | 4.85 / 2.16 | 5.69 / 2.16 | 0.0001 / 0.0001 |
| Tree | 262K | 13.37 / 11.70 | 13.27 / 11.68 | 13.37 / 11.68 | 0.0158 / 0.0291 |
| Tree | 5M | 132.60 / 187.09 | 132.58 / 187.07 | 132.60 / 187.07 | 0.1895 / 0.3878 |
| Tree | 50M | 1,138.97 / 1,853.29 | 1,138.95 / 1,853.28 | 1,138.97 / 1,853.28 | 1.8186 / 4.0355 |
| Map | 4K | 5.69 / 2.18 | 4.93 / 2.16 | 5.69 / 2.16 | 0.0002 / 0.0001 |
| Map | 262K | 12.80 / 11.70 | 12.68 / 11.68 | 12.80 / 11.68 | 0.0194 / 0.0234 |
| Map | 5M | 124.86 / 187.09 | 124.76 / 187.07 | 124.86 / 187.07 | 0.2244 / 0.3692 |
| Map | 50M | 1,171.18 / 1,853.33 | 1,171.16 / 1,853.31 | 1,171.18 / 1,853.31 | 2.8701 / 3.9235 |

</details>

Small gaps are within run-to-run noise. The [raw report](benchmarks/arm64-2026-10-29.json) adds Go's built-in `map` and `sync.Map`; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

To reproduce, use source checkpoint `4aa441c` and build both drivers:

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
