# gobbtree

[![Tests](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml)
[![amd64 SIMD](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml)

**Fast ordered maps and sets for Go, ported from Rust's [bbtree](https://github.com/XiangpengHao/bbtree).** It is a B+ tree whose nodes keep compact summaries of their keys and search them with SIMD, so most lookups never touch a full key.

1. Faster than the Rust original in 189 of the 192 `uint64` timings of the [benchmarks](#benchmarks): 2.1× its lookup throughput on one thread and 1.5× on four, on 5M keys. Rust still builds ascending keys several times faster and wins most byte-string cells.
2. `Map` is concurrent, with upstream's optimistic latching: lookups descend without locking, checking node versions, and writers latch only the leaf they change. On four threads it serves 2.0× the lookups of `sync.Map`, with its keys in order.
3. From 5M entries up, peak memory 20% below the Rust original's on clustered keys, and up to 16% above it on mixed and sequential ones.
4. Any `cmp.Ordered` key, your own orders through a two-method `Key` interface, and `[]byte` keys.
5. File maps in upstream's format: write a snapshot, then query it in place through `mmap`. Go and Rust read each other's files.
6. Pure Go: no assembly, cgo or dependencies. The SIMD comes from Go 1.27's `simd` package.
7. Checked against upstream itself: CI compares containers and files with a live Rust oracle, besides the race detector and fuzzing.

![gobbtree's Map against Rust bbtree's and sync.Map, get throughput at 4 threads](benchmarks/race-concurrent-arm64.svg)

## Usage

```sh
go get github.com/thesyncim/gobbtree
```

gobbtree needs Go 1.27. Build with `GOEXPERIMENT=simd` to search with [SIMD](#simd); without it, the same code searches with scalar loops.

### Concurrent map

```go
m := gobbtree.New[uint64, uint64]() // the zero value works too

var wg sync.WaitGroup
wg.Go(func() { m.Insert(1, 10) })
wg.Go(func() { m.Insert(2, 20) })
wg.Wait()

v, ok := m.Get(1)                                   // 10, true
m.Read(2, func(_, v uint64) { fmt.Println(v + 1) }) // 21

got := make([]gobbtree.Result[uint64], 3)
m.GetMany([]uint64{1, 2, 3}, got) // {10 true} {20 true} {0 false}
```

### Single owner

```go
t := gobbtree.NewTree[int, string]()
t.Insert(1, "one")
t.Insert(2, "two")
t.Insert(3, "three")

v, ok := t.Get(1) // "one", true

for k, v := range t.Range(1, 3) { // keys in [1, 3): 1, then 2
    fmt.Println(k, v)
}
for k, v := range t.ScanReverse(gobbtree.Above(1).AtMost(3)) { // 3, then 2
    fmt.Println(k, v)
}
```

### Containers

| Type | Made by | For |
|---|---|---|
| `Map[K, V]` | `New` | Concurrent readers and writers |
| `Tree[K, V]` | `NewTree` | A single owner; concurrent reads only while nobody writes |
| `Set[K]` | `NewSet` | A single owner's set, with union, intersection, difference and subset tests |
| `MapOf`, `TreeOf`, `SetOf` | `NewMapOf`, `NewTreeOf`, `NewSetOf` | The same three, for [custom keys](#custom-keys) and `[]byte` keys (`Bytes`) |
| `FileMap[K, V]` | `ReadFile`, `Mmap(path).Open()` | A read-only snapshot, searched in place ([file maps](#file-maps)) |

`Map` and `Tree` share one API, from `Get`, `Insert`, `Delete` and `Take` to batches (`GetMany`, `InsertMany`), read-modify-write (`Update`, `UpdateOrInsert`, `LoadOrStore`) and iterators (`All`, `Range`, `Scan` over any `Bounds`, `Ascend`, `Descend` and their reverses). `Map`'s read-modify-writes are atomic, and it adds callback reads (`Read`, `ReadMany`) and `DeleteIf`. `Tree` adds upstream's `Entry` API and ordered editing (`LowerBound`, `SplitOff`, `Retain`, `ExtractIf`, …). [Coming from bbtree](#coming-from-bbtree) lists every operation.

- Iterators are range-over-func sequences, so `break` stops them.
- Keys follow `cmp.Compare`: NaN sorts first and `-0 == +0`. Inserting an equal key keeps the stored key.
- Keys and values are copied shallowly.
- The in-memory containers' zero values are ready to use. Don't copy a container after first use.

### Custom keys

A type orders itself by implementing `Key`: `Compare` defines the order, and `Window` exposes it to the SIMD search.

```go
type Key[K any] interface {
    Compare(other K) int
    // Bytes offset..offset+8 of an encoding that sorts like Compare,
    // big-endian and zero past its end.
    Window(offset int) uint64
}
```

For example, names ordered case-insensitively:

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
tags.Insert("GO", 2) // the same key: replaces the value, keeps "Go"
```

Byte-slice keys are built in as `Bytes`:

```go
index := gobbtree.NewMapOf[gobbtree.Bytes, int]()
buf := []byte("user:42")
index.Insert(buf, 1)    // stores a copy: buf may be reused
v, ok := index.Get(buf) // lookups don't allocate or keep buf
```

A composite key encodes its fields one after another, for example a `uint32` tenant as four big-endian bytes followed by a name; `StringWindow`, `BytesWindow` and `Uint64Window` window the common encodings. Equal keys must have equal windows. Windows that tie are settled by `Compare`, so even a constant `Window` is correct, only slower. A key that refers to reusable memory also implements `Clone() K`, and containers store a clone of each new key.

### File maps

```go
tree := gobbtree.NewTree[string, int]()
tree.Insert("alpha", 1)
if err := tree.WriteFile("index.bbt"); err != nil { // Map and Set write files too
    log.Fatal(err)
}

disk, err := gobbtree.Mmap[string, int]("index.bbt").Open() // or ReadFile
if err != nil {
    log.Fatal(err)
}
defer disk.Close()
value, found := disk.Get("alpha") // 1, true
```

A file map searches the image in place, with no rebuild, and supports `Get`, `GetMany` and iteration in both directions over any bounds. Files use upstream's version 1 format, which the Rust oracle tests both ways.

- **Types:** integer and string keys; number, plain-array, empty-struct, string and byte-slice values.
- **Checking:** `ReadFile`, `ReadFileRange` and `OpenFileMap` check every page, as upstream's do. `Mmap(...).Open()` and `OpenFileMapUnchecked` check the header alone: a corrupt file may give wrong answers or panic, but is never read out of bounds.
- **Writing:** `WriteFile` replaces the file atomically, and `CreateFile` writes one from an ascending `iter.Seq2`. `NewFileWriter`, `WriteAt`, `ReadFileRange` and `Mmap(...).Range` handle snapshots inside larger files; `Mmap(...).Pages(OsPagesHuge)` advises huge pages on Linux.
- **Immutable:** to change a file map, write a new snapshot.

## Concurrency

`Map` ports the protocol of upstream's `BBTreeMap` ([design](https://github.com/XiangpengHao/bbtree/blob/1c72bd27994ed0a38694b029618e80115e22644f/design/09-concurrency.md)). Every node carries a latch word with a version. Lookups descend optimistically, checking each node's version, and write nothing. Writers latch only the leaf they change; a split or merge takes only the nodes it changes, without waiting, and starts over if another goroutine holds one. No lock or counter is shared by all writers, so writes scale with cores. Lookups of custom keys, and of string keys a leaf's summaries don't settle, read the leaf under its shared latch, as upstream's do for keys it can't read inline. A lookup whose leaf keeps changing takes that latch too, which lets readers progress behind a hot leaf.

Beyond upstream, every node keeps its fences, the bounds of the keys under it, and a table of hints jumps from a key straight to its leaf. A hint is checked against the leaf's version and fences before it is used, so a stale one costs only a descent from the root.

- **Iteration** keeps upstream's `range_cloned` contract, also for `Scan`: it copies a few leaves at a time and visits the copies with nothing held, so the loop body may use the map. (Upstream's `scan` runs its closure under each leaf's read latch.) Iteration is not a snapshot: an entry present throughout is visited exactly once, and entries inserted or removed meanwhile may or may not be.
- **Batches** are consistent per entry; a batch is not a transaction.
- **Callbacks** of `Read`, `ReadMany`, `Update`, `UpdateOrInsert` and `DeleteIf` run under the entry's leaf latch, as upstream's do, and must not use the same map. A panicking callback releases its latches and stores nothing.
- **No `Len`**, as upstream's shared map has none. `IsEmpty` reports whether the map held no entry at some moment during the call. `Clear`, like upstream's `clear(&mut self)`, is the owner's: no other goroutine may use the map until it returns.

## Coming from bbtree

The API follows upstream's operations with Go names. Results are multiple return values, and iterators are range-over-func sequences.

| Upstream | gobbtree |
|---|---|
| `get` · `get_copied` · `get_cloned` · `contains_key` · `get_key_value` | `Get` · `Contains` · `GetKeyValue` |
| `insert` · `remove` · `remove_entry` | `Insert` · `Delete` · `Take` |
| `get_many_*` · `insert_many` · `read_many` · `update_or_insert_many` | `GetMany` · `InsertMany` · `ReadMany` · `UpdateOrInsertMany` |
| `read` · `update` · `update_or_insert` · `get_or_insert` · `remove_if` | `Read` · `Update` · `UpdateOrInsert` · `LoadOrStore` · `DeleteIf`; callbacks see the stored key, and return results through the closure |
| `get_mut` · `iter_mut` · `values_mut` · `range_mut` | `Update` · `UpdateAll` · `UpdateRange`: Go can't lend a reference into a leaf, so values go out and back |
| `entry` · `first_entry` · `last_entry` | `Entry` · `FirstEntry` · `LastEntry`, with `OrInsert` · `OrInsertWith` · `OrInsertWithKey` · `OrDefault` · `AndModify` · `Insert` · `InsertEntry` · `Remove` |
| Ranges `a..b` · `a..=b` · `a..` · `..b` · `..=b` · `(Excluded(a), _)` | `Between(a, b)` · `AtLeast(a).AtMost(b)` · `AtLeast(a)` · `Below(b)` · `AtMost(b)` · `Above(a)…`; invalid bounds panic as upstream's do |
| `iter` · `range` / `range_cloned` / `scan` · `scan_rev` · `iter().rev()` | `All` · `Scan` · `ScanReverse` · `Reverse` |
| `keys` · `values` · `first_key_value` · `last_key_value` · `first_copied` · `pop_first` · `pop_last` | `Keys` · `Values` · `First` · `Last` · `FirstIn` · `PopFirst` · `PopLast` |
| `retain` · `extract_if` · `split_off` · `append` · `Extend` · `FromIterator` | `Retain` · `ExtractIf` · `SplitOff` · `Append` · `InsertAll` |
| set `contains_many` · `replace` · `take` · `union` · `intersection` · `difference` · `symmetric_difference` · `is_subset` | the same names on `Set` |
| `Clone` · `PartialEq`/`Eq` · `PartialOrd`/`Ord` · `Debug` · `into_iter`/`into_keys`/`into_values` | `Clone` · `EqualFunc`/`Equal` · `CompareFunc`/`Compare` · `String` · `Drain` |
| `InlineKey` (`inline_key_at`) · byte-string keys, which compare from where they differ (`cmp_from`) | `Key` (`Window`) · `Bytes`; string and `Bytes` keys compare from where they differ |
| `FileWriter::from_ascending` · `byte_len` · `write` | `NewFileWriter` · `ByteLen` · `WriteTo`; a container's `WriteFile` writes one directly |
| `FileMap::open` · `open_unchecked` · `NotAscending::index` | `OpenFileMap` · `OpenFileMapUnchecked` · `NotAscendingError.Index` |
| bbtree-mmap `create` · `write_at` · `read` · `read_range` · `mmap().range().pages().open()` | `CreateFile` · `WriteAt` · `ReadFile` · `ReadFileRange` · `Mmap().Range().Pages().Open()` |

Beyond upstream:

- **`Map`:** `First`, `Last`, `PopFirst`, `PopLast`, `LoadOrStoreFunc`, `UpdateAll` and `UpdateRange`.
- **`Tree`:** `LowerBound`, `UpperBound`, `LoadOrStore`, `LoadOrStoreFunc` and `UpdateOrInsert`.
- **`FileMap`:** `ByteLen`, `WriteTo` and `Close`.
- **Everywhere:** `Range(lo, hi)` and `RangeReverse` as shorthands for scanning `Between(lo, hi)`, and `Ascend(from)` and `Descend(from)` for scanning from a key.

Not carried over, because Go has no counterpart:

- **Rust-only traits:** no operator overloading for `Index` (use `Get`), and no `Hash`.
- **Allocators and `InsertError`:** Go's allocation failure is fatal, so `Insert` has no error path.
- **`NodeConfig` page sizes:** nodes hold a fixed 32 entries. Go generics can index an array type parameter but can't slice it or point into it, which shifting entries and the SIMD searches need, so another size would mean another copy of the node code.
- **The rest of `InlineKey`** (`WIDTH`, `hash_tag`, `cmp_prefix`, and `cmp_from` on custom keys): these tune upstream's node layout, and a generic Go container can't call an optional key method without boxing the key.
- **Leaf annexes and their `*_with_leaf` methods**, which let `bbtree-store` manage manually allocated pages; Go's collector manages memory here.
- **The `inspect` and `check_invariants` debugging helpers.**

## SIMD

| Target | Search kernels |
|---|---|
| amd64 | AVX-512 or AVX2, chosen at run time, with a 128-bit fallback |
| arm64 | NEON |
| Others | Go's portable SIMD where available, otherwise scalar |

`gobbtree.SIMD()` reports whether the build uses hardware SIMD, and its vector width. `GODEBUG=simd=0` forces the scalar path.

## Benchmarks

![gobbtree's Tree against Rust bbtree's and Go's map, get throughput on one thread](benchmarks/race-single-arm64.svg)

gobbtree's `Tree` and `Map` against Rust bbtree's `unsync::BBTreeMap` and `BBTreeMap` on an Apple M4 Max: Go 1.27.3 with `GOEXPERIMENT=simd` at [`38a61be`](https://github.com/thesyncim/gobbtree/commit/38a61be492c7c84e62a18fd3186ea3589eb8ecd2), Rust 1.99.1 with native CPU tuning, full LTO and mimalloc at [`1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). Each cell is the median of three fresh-process runs over identical keys, with checked results.

gobbtree is faster in 189 of the 192 `uint64` timings below; Rust wins Map 4 workers · 10% writes at 4K sequential keys; Tree delete + insert new key at 50M sequential keys; Map delete + insert new key at 50M sequential keys. Go builds faster in 7 of 24 build cells, at 0.16× to 2.30× Rust's speed. Go's process peaks 3.3–3.4 MB higher at 4K, 1.9–4.3 MB higher at 262K, from 37.7 MB lower to 10.5 MB higher at 5M, from 404.2 MB lower to 192.5 MB higher at 50M entries, against Rust's.

With byte-string keys, Go `Bytes` against Rust `String`, Go wins 8 of 22 cells.

The tables cover `uint64` keys and values from 4K to 50M entries, with mixed keys (hashed keys spread over the full 64-bit range), sequential keys (consecutive even keys; misses and fresh keys fill the gaps) and clustered keys (runs of 1,024 consecutive even keys at scattered bases). Batch cells are per key, churn cells per delete-and-insert pair, and parallel cells use four workers.

<details>
<summary>Times, mixed keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.67 / 12.55 | 10.94 / 24.04 | 41.69 / 85.70 | 67.76 / 117.84 |
| Read miss | 8.32 / 11.84 | 10.77 / 22.60 | 30.69 / 74.47 | 57.58 / 108.77 |
| Batch hit · 8 keys | 7.31 / 12.14 | 10.70 / 22.69 | 33.63 / 52.01 | 49.11 / 68.45 |
| Batch miss · 8 keys | 8.07 / 11.95 | 10.50 / 20.30 | 26.51 / 42.47 | 45.26 / 56.67 |
| Replace value | 10.16 / 12.79 | 16.98 / 26.19 | 61.24 / 92.49 | 96.62 / 112.68 |
| Delete + reinsert same key | 50.31 / 74.18 | 70.46 / 113.10 | 199.57 / 245.91 | 320.50 / 336.00 |
| Delete + insert new key | 59.75 / 97.84 | 94.20 / 150.49 | 263.69 / 362.10 | 445.21 / 508.48 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.26 / 13.59 | 14.93 / 27.94 | 61.22 / 102.53 | 81.76 / 113.36 |
| Read miss | 10.19 / 12.51 | 16.80 / 25.47 | 56.58 / 70.39 | 79.29 / 103.76 |
| Batch hit · 8 keys | 8.56 / 13.62 | 11.80 / 22.07 | 40.19 / 53.60 | 50.49 / 67.96 |
| Batch miss · 8 keys | 9.96 / 12.85 | 13.28 / 19.52 | 37.51 / 40.66 | 49.62 / 56.08 |
| Replace value | 11.83 / 14.73 | 24.61 / 29.63 | 94.25 / 116.03 | 135.46 / 150.65 |
| Delete + reinsert same key | 56.93 / 75.90 | 80.57 / 112.33 | 224.85 / 244.03 | 310.62 / 327.36 |
| Delete + insert new key | 64.47 / 105.10 | 104.95 / 145.40 | 320.05 / 351.40 | 491.27 / 511.24 |
| 4 workers · reads | 2.25 / 3.84 | 4.33 / 7.35 | 19.37 / 28.54 | 27.15 / 38.22 |
| 4 workers · 10% writes | 5.26 / 5.57 | 4.66 / 7.51 | 20.97 / 29.74 | 26.34 / 43.27 |

</details>

<details>
<summary>Times, sequential keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.74 / 11.84 | 9.70 / 24.14 | 40.61 / 78.89 | 62.42 / 102.32 |
| Read miss | 6.67 / 11.00 | 8.71 / 22.27 | 22.51 / 54.97 | 47.93 / 97.62 |
| Batch hit · 8 keys | 6.66 / 11.85 | 9.67 / 21.93 | 32.99 / 49.57 | 52.46 / 70.71 |
| Batch miss · 8 keys | 6.54 / 11.01 | 8.65 / 20.06 | 20.37 / 33.19 | 40.14 / 61.04 |
| Replace value | 8.47 / 12.39 | 15.83 / 25.47 | 63.14 / 86.41 | 100.90 / 124.34 |
| Delete + reinsert same key | 49.32 / 78.41 | 67.90 / 122.54 | 190.14 / 234.44 | 294.32 / 319.02 |
| Delete + insert new key | 45.61 / 64.11 | 66.90 / 116.26 | 201.94 / 248.00 | 310.60 / 307.48 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.65 / 13.46 | 13.51 / 28.26 | 52.17 / 95.78 | 76.20 / 117.10 |
| Read miss | 7.49 / 12.58 | 13.74 / 26.15 | 48.21 / 56.54 | 79.60 / 99.33 |
| Batch hit · 8 keys | 7.91 / 13.73 | 10.70 / 22.36 | 34.77 / 49.70 | 52.19 / 73.26 |
| Batch miss · 8 keys | 7.97 / 13.07 | 10.74 / 20.49 | 34.28 / 34.67 | 49.51 / 61.62 |
| Replace value | 11.59 / 15.00 | 22.23 / 31.57 | 84.08 / 110.36 | 126.16 / 161.03 |
| Delete + reinsert same key | 56.83 / 78.70 | 80.10 / 120.65 | 207.59 / 237.84 | 307.75 / 315.56 |
| Delete + insert new key | 50.69 / 84.85 | 77.12 / 113.90 | 224.31 / 251.85 | 324.86 / 315.80 |
| 4 workers · reads | 2.07 / 3.42 | 4.07 / 7.46 | 17.93 / 25.16 | 26.61 / 37.00 |
| 4 workers · 10% writes | 6.56 / 4.87 | 4.38 / 7.59 | 20.48 / 28.24 | 26.96 / 42.51 |

</details>

<details>
<summary>Times, clustered keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.15 / 26.86 | 15.76 / 48.53 | 64.16 / 178.04 | 127.58 / 350.70 |
| Read miss | 9.14 / 25.71 | 15.23 / 45.26 | 40.68 / 146.64 | 90.09 / 339.08 |
| Batch hit · 8 keys | 6.95 / 24.70 | 14.45 / 42.88 | 51.08 / 134.82 | 97.84 / 213.54 |
| Batch miss · 8 keys | 6.97 / 24.07 | 12.99 / 41.23 | 36.69 / 124.43 | 86.13 / 202.75 |
| Replace value | 10.87 / 28.72 | 21.72 / 52.25 | 87.83 / 229.20 | 146.97 / 392.68 |
| Delete + reinsert same key | 51.99 / 119.45 | 76.67 / 178.15 | 260.04 / 456.75 | 357.79 / 774.97 |
| Delete + insert new key | 49.63 / 104.14 | 81.76 / 176.20 | 268.82 / 500.23 | 385.10 / 784.32 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.08 / 37.61 | 19.12 / 61.47 | 83.50 / 208.72 | 146.49 / 378.16 |
| Read miss | 7.95 / 34.99 | 18.57 / 58.75 | 74.17 / 189.10 | 131.59 / 345.83 |
| Batch hit · 8 keys | 8.30 / 34.96 | 15.50 / 47.31 | 58.62 / 157.67 | 104.27 / 259.96 |
| Batch miss · 8 keys | 8.15 / 26.31 | 15.23 / 46.76 | 52.75 / 145.08 | 101.83 / 244.08 |
| Replace value | 12.60 / 38.82 | 30.43 / 63.01 | 123.80 / 216.11 | 193.85 / 378.49 |
| Delete + reinsert same key | 57.61 / 125.25 | 86.54 / 165.61 | 262.51 / 393.75 | 370.48 / 662.77 |
| Delete + insert new key | 53.01 / 120.56 | 91.92 / 166.18 | 281.04 / 407.90 | 398.61 / 651.91 |
| 4 workers · reads | 2.18 / 9.72 | 5.31 / 16.49 | 23.79 / 55.90 | 38.16 / 92.65 |
| 4 workers · 10% writes | 5.63 / 9.62 | 6.05 / 15.92 | 27.77 / 56.75 | 42.45 / 94.80 |

</details>

<details>
<summary>Memory and build — Go / Rust</summary>

Whole-process RSS, the Go runtime included, under Go's default GC settings: at its peak, after a full read pass, and at the end, after a forced GC in Go. Build is the time to insert every entry, one at a time.

**Mixed keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.49 / 2.15 | 4.67 / 2.13 | 5.49 / 2.13 | 0.0005 / 0.0004 |
| Tree | 262K | 13.09 / 9.04 | 12.93 / 9.03 | 13.09 / 9.03 | 0.0281 / 0.0271 |
| Tree | 5M | 141.28 / 136.46 | 140.94 / 136.45 | 141.28 / 136.45 | 0.7239 / 0.6651 |
| Tree | 50M | 1,499.25 / 1,347.80 | 1,499.17 / 1,347.78 | 1,499.25 / 1,347.78 | 12.4286 / 10.8945 |
| Map | 4K | 5.55 / 2.15 | 4.69 / 2.13 | 5.55 / 2.13 | 0.0003 / 0.0003 |
| Map | 262K | 13.09 / 9.04 | 12.88 / 9.03 | 13.09 / 9.03 | 0.0319 / 0.0286 |
| Map | 5M | 141.49 / 136.50 | 141.15 / 136.48 | 141.49 / 136.48 | 0.8075 / 0.6796 |
| Map | 50M | 1,500.27 / 1,348.19 | 1,500.15 / 1,348.17 | 1,500.27 / 1,348.17 | 14.4299 / 11.0973 |

**Sequential keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.49 / 2.15 | 4.73 / 2.13 | 5.49 / 2.13 | 0.0003 / 0.0001 |
| Tree | 262K | 12.44 / 8.24 | 12.09 / 8.22 | 12.44 / 8.22 | 0.0168 / 0.0033 |
| Tree | 5M | 130.48 / 120.23 | 130.30 / 120.21 | 130.48 / 120.21 | 0.1951 / 0.0449 |
| Tree | 50M | 1,376.57 / 1,184.02 | 1,376.50 / 1,184.01 | 1,376.57 / 1,184.01 | 2.6311 / 0.4184 |
| Map | 4K | 5.49 / 2.15 | 4.69 / 2.13 | 5.49 / 2.13 | 0.0003 / 0.0001 |
| Map | 262K | 12.53 / 8.24 | 12.14 / 8.22 | 12.53 / 8.22 | 0.0201 / 0.0046 |
| Map | 5M | 130.81 / 120.27 | 130.55 / 120.26 | 130.81 / 120.26 | 0.2177 / 0.0594 |
| Map | 50M | 1,375.81 / 1,184.45 | 1,375.70 / 1,184.43 | 1,375.81 / 1,184.43 | 2.9990 / 0.5107 |

**Clustered keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.49 / 2.16 | 4.75 / 2.15 | 5.49 / 2.15 | 0.0002 / 0.0002 |
| Tree | 262K | 13.62 / 11.68 | 13.47 / 11.67 | 13.62 / 11.67 | 0.0118 / 0.0272 |
| Tree | 5M | 149.45 / 187.07 | 149.19 / 187.06 | 149.45 / 187.06 | 0.2207 / 0.3943 |
| Tree | 50M | 1,449.49 / 1,853.28 | 1,449.38 / 1,853.26 | 1,449.49 / 1,853.26 | 2.5709 / 4.1005 |
| Map | 4K | 5.46 / 2.16 | 4.70 / 2.15 | 5.46 / 2.15 | 0.0004 / 0.0002 |
| Map | 262K | 13.55 / 11.68 | 13.42 / 11.67 | 13.55 / 11.67 | 0.0212 / 0.0251 |
| Map | 5M | 149.42 / 187.09 | 149.18 / 187.07 | 149.42 / 187.07 | 0.2585 / 0.3769 |
| Map | 50M | 1,449.13 / 1,853.31 | 1,449.03 / 1,853.29 | 1,449.13 / 1,853.29 | 2.9231 / 4.0042 |

</details>

<details>
<summary>Byte-string keys — Go / Rust, ns/op</summary>

Go `Bytes` keys, byte slices a program can reuse, against Rust `String`s, each a mixed key written as `user:` and 16 hex digits, with `uint64` values. Rust's containers move in keys made before the timed build; Go's copy each key's bytes into their leaves. Rust replaces values with `update` and `get_mut`, and its churn reinserts the key its removal returned. In parentheses, Go's time against Rust's: negative is faster.

| Workload | 262K | 5M |
|---|---:|---:|
| Tree · Build, per insert | 145.5 / 93.9 (+55%) | 306.4 / 202.0 (+52%) |
| Tree · Read hit | 36.6 / 38.9 (-6%) | 108.8 / 183.7 (-41%) |
| Tree · Read miss | 34.9 / 32.2 (+8%) | 84.4 / 125.0 (-32%) |
| Tree · Replace value | 45.6 / 40.4 (+13%) | 155.8 / 181.5 (-14%) |
| Tree · Delete + reinsert same key | 146.8 / 147.7 (-1%) | 348.8 / 395.4 (-12%) |
| Map · Build, per insert | 170.6 / 106.3 (+60%) | 381.9 / 219.4 (+74%) |
| Map · Read hit | 51.5 / 43.7 (+18%) | 140.0 / 171.8 (-19%) |
| Map · Read miss | 47.9 / 34.7 (+38%) | 132.9 / 93.9 (+42%) |
| Map · Replace value | 83.6 / 48.7 (+72%) | 261.5 / 179.2 (+46%) |
| Map · Delete + reinsert same key | 195.6 / 154.2 (+27%) | 443.8 / 404.4 (+10%) |
| Map · 4 workers · reads | 13.6 / 11.9 (+14%) | 39.4 / 65.0 (-39%) |

</details>

Small gaps are inconclusive: a macOS system service used one or two cores throughout the run. The [raw report](benchmarks/arm64-2026-10-29.json) holds every run, with Go's built-in `map` and `sync.Map` at 4K to 5M mixed keys; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

How the comparison is kept fair:

- **Identical work.** Both languages get the same keys, queries and operation counts. Checksums are verified, including every bit of replaced values; new-key churn uses disjoint key sets and checks that every delete and insert succeeds.
- **Fresh processes.** Every backend runs in a fresh process, in rotated order. Serial cases are pinned to one OS thread, and parallel timings exclude starting workers and reducing checksums.
- **Public APIs.** Batches go through each library's public batch API, and the Rust code is unmodified upstream.
- **Equal guarantees.** Both concurrent maps omit `len()`, and both read payloads with plain loads validated by node versions.

<details>
<summary>Commands, from source checkpoint `38a61be`</summary>

Build the drivers:

```sh
GOEXPERIMENT=simd go build -o compare-go ./cmd/compare
GOEXPERIMENT=simd go build -o memory-go ./cmd/memory
RUSTFLAGS='-C target-cpu=native' cargo build --release --locked \
  --manifest-path internal/oracle/Cargo.toml --bin compare --bin memory
```

Time the operations, here at 5M entries; `--standard` adds Go's `map` and `sync.Map`. At 50M, drop `--standard`; at 4K and 262K, use `--queries 8192 --rounds 512`.

```sh
python3 internal/cpu_compare.py --go ./compare-go --rust internal/oracle/target/release/compare \
  --size 5000000 --queries 1048576 --rounds 4 --threads 4 --repeat 3 --standard --mutations --distinct
```

Measure memory and build time:

```sh
python3 internal/rss_compare.py --go ./memory-go --rust internal/oracle/target/release/memory \
  --size 50000000 --repeat 3 --kinds tree map
```

Both scripts take `--keys mixed` (the default), `--keys sequential` or `--keys clustered`. `cpu_compare.py --text --mutations` runs the string-key cells, which the report has at 262K and 5M entries. `python3 internal/render_benchmarks.py` redraws the cards and tables above from the saved report.

</details>

## Testing

```sh
GOEXPERIMENT=simd go test -race ./...
GOEXPERIMENT=simd GODEBUG=simd=0 go test ./...   # the scalar path
GOEXPERIMENT=simd go test -run '^$' -fuzz FuzzTree -fuzztime 20s
```

The tests check every container against independent references: randomized operation sequences with tree invariants, concurrent mutation and panicking callbacks, SIMD boundaries, steady-state allocations and custom key orders. CI also fuzzes file maps, runs the amd64 kernels at each vector width, and checks containers and files against a live Rust oracle of upstream at [`1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f):

```sh
cargo build --release --locked --manifest-path internal/oracle/Cargo.toml
GOBBTREE_ORACLE="$PWD/internal/oracle/target/release/gobbtree-oracle" \
GOBBTREE_FILE_ORACLE="$PWD/internal/oracle/target/release/file-oracle" \
GOEXPERIMENT=simd go test -run 'TestUpstream.*Oracle' -v
```

## License

Licensed under either of the MIT License or the Apache License 2.0, at your option, like upstream; [LICENSE](LICENSE) holds both texts.
