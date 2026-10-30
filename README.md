# gobbtree

[![Tests](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml)
[![amd64 SIMD](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml)

**Fast ordered maps and sets for Go, ported from Rust's [bbtree](https://github.com/XiangpengHao/bbtree).** It is a B+ tree whose nodes keep compact summaries of their keys and search them with SIMD, so most lookups never touch a full key.

1. Faster than the Rust original in 188 of the 192 `uint64` timings of the [benchmarks](#benchmarks): 2.1× its lookup throughput on one thread and 1.7× on four, on 5M keys. Rust still builds ascending keys about twice as fast and wins 12 of the 22 byte-string cells.
2. `Map` is concurrent, with upstream's optimistic latching: lookups descend without locking, checking node versions, and writers latch only the leaf they change. On four threads it serves 2.3× the lookups of `sync.Map`, with its keys in order.
3. From 5M entries up, peak memory 20–22% below the Rust original's on clustered keys, and 4–16% above it on mixed and sequential ones.
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

gobbtree's `Tree` and `Map` against Rust bbtree's `unsync::BBTreeMap` and `BBTreeMap` on an Apple M4 Max: Go 1.27.3 with `GOEXPERIMENT=simd` at [`d9d3697`](https://github.com/thesyncim/gobbtree/commit/d9d36975733f36124c86fa41afb8e290afc7310a), Rust 1.99.1 with native CPU tuning, full LTO and mimalloc at [`1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). Each cell is the median of three fresh-process runs over identical keys, with checked results.

gobbtree is faster in 188 of the 192 `uint64` timings below; Rust wins Map 4 workers · 10% writes at 4K mixed keys; Map 4 workers · 10% writes at 4K sequential keys; Tree delete + insert new key at 50M sequential keys; Map delete + insert new key at 50M sequential keys. Go builds faster in 7 of 24 build cells, at 0.31× to 1.90× Rust's speed. Go's process peaks 3.3–3.4 MB higher at 4K, 1.9–4.1 MB higher at 262K, from 37.7 MB lower to 10.8 MB higher at 5M, from 404.2 MB lower to 192.4 MB higher at 50M entries, against Rust's.

With byte-string keys, Go `Bytes` against Rust `String`, Go wins 10 of 22 cells.

The tables cover `uint64` keys and values from 4K to 50M entries, with mixed keys (hashed keys spread over the full 64-bit range), sequential keys (consecutive even keys; misses and fresh keys fill the gaps) and clustered keys (runs of 1,024 consecutive even keys at scattered bases). Batch cells are per key, churn cells per delete-and-insert pair, and parallel cells use four workers.

<details>
<summary>Times, mixed keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.54 / 12.54 | 10.91 / 23.74 | 38.86 / 81.00 | 64.43 / 109.08 |
| Read miss | 7.72 / 11.72 | 10.05 / 22.46 | 28.32 / 65.42 | 54.32 / 105.57 |
| Batch hit · 8 keys | 7.23 / 12.06 | 10.60 / 22.64 | 32.20 / 49.84 | 49.03 / 68.10 |
| Batch miss · 8 keys | 8.10 / 11.95 | 10.31 / 20.28 | 25.45 / 38.86 | 43.77 / 56.41 |
| Replace value | 9.19 / 12.92 | 16.21 / 26.45 | 61.64 / 83.85 | 93.37 / 109.50 |
| Delete + reinsert same key | 48.38 / 73.21 | 67.16 / 110.97 | 193.66 / 234.19 | 291.28 / 309.46 |
| Delete + insert new key | 58.16 / 96.63 | 92.04 / 145.60 | 254.25 / 333.40 | 400.47 / 499.06 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.32 / 13.55 | 14.56 / 28.03 | 51.14 / 95.77 | 82.81 / 114.23 |
| Read miss | 9.16 / 12.43 | 13.98 / 25.22 | 40.73 / 66.95 | 70.37 / 99.92 |
| Batch hit · 8 keys | 8.53 / 13.55 | 11.84 / 21.84 | 34.13 / 51.55 | 49.94 / 67.19 |
| Batch miss · 8 keys | 9.25 / 12.79 | 11.52 / 19.44 | 28.52 / 40.35 | 44.13 / 55.77 |
| Replace value | 12.33 / 14.68 | 24.22 / 29.49 | 85.20 / 112.87 | 132.30 / 144.51 |
| Delete + reinsert same key | 54.81 / 75.04 | 77.53 / 109.68 | 199.54 / 237.53 | 301.16 / 318.35 |
| Delete + insert new key | 63.21 / 103.39 | 102.05 / 139.74 | 306.71 / 333.82 | 488.44 / 497.96 |
| 4 workers · reads | 2.27 / 3.84 | 4.14 / 7.38 | 17.43 / 28.82 | 26.59 / 39.14 |
| 4 workers · 10% writes | 5.35 / 4.96 | 4.56 / 7.24 | 19.36 / 28.73 | 25.84 / 43.15 |

</details>

<details>
<summary>Times, sequential keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.40 / 11.82 | 9.48 / 23.96 | 39.00 / 77.33 | 63.73 / 100.23 |
| Read miss | 6.52 / 11.14 | 8.41 / 22.04 | 20.49 / 51.74 | 49.37 / 93.83 |
| Batch hit · 8 keys | 6.58 / 11.81 | 9.65 / 21.85 | 32.39 / 48.36 | 50.58 / 69.53 |
| Batch miss · 8 keys | 6.53 / 11.02 | 8.49 / 19.86 | 19.93 / 33.10 | 39.85 / 57.23 |
| Replace value | 7.91 / 12.31 | 15.13 / 25.40 | 52.77 / 82.01 | 95.58 / 119.94 |
| Delete + reinsert same key | 47.53 / 78.27 | 65.62 / 122.22 | 184.87 / 230.53 | 292.38 / 311.66 |
| Delete + insert new key | 44.06 / 63.88 | 65.54 / 114.48 | 196.68 / 236.31 | 305.28 / 293.88 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.76 / 13.37 | 12.87 / 28.31 | 50.66 / 92.12 | 77.98 / 115.32 |
| Read miss | 7.32 / 12.58 | 10.73 / 26.16 | 29.83 / 55.43 | 60.16 / 96.55 |
| Batch hit · 8 keys | 7.95 / 13.66 | 10.37 / 22.50 | 36.56 / 49.22 | 50.37 / 71.90 |
| Batch miss · 8 keys | 7.80 / 12.91 | 9.62 / 20.41 | 22.57 / 33.57 | 40.45 / 58.85 |
| Replace value | 11.73 / 14.45 | 23.07 / 31.55 | 81.57 / 106.02 | 126.69 / 160.41 |
| Delete + reinsert same key | 54.72 / 78.34 | 78.43 / 118.86 | 197.92 / 229.46 | 301.33 / 324.98 |
| Delete + insert new key | 49.51 / 84.25 | 75.40 / 113.02 | 207.85 / 239.93 | 319.10 / 301.60 |
| 4 workers · reads | 2.07 / 3.40 | 3.57 / 7.31 | 15.93 / 24.78 | 23.68 / 36.03 |
| 4 workers · 10% writes | 5.19 / 4.87 | 4.22 / 7.37 | 19.67 / 27.46 | 26.46 / 41.24 |

</details>

<details>
<summary>Times, clustered keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.85 / 26.60 | 15.50 / 47.86 | 62.37 / 169.28 | 126.31 / 347.74 |
| Read miss | 9.30 / 25.37 | 15.29 / 45.07 | 38.98 / 144.99 | 90.65 / 340.24 |
| Batch hit · 8 keys | 6.91 / 23.28 | 14.35 / 42.64 | 51.64 / 134.41 | 95.52 / 207.13 |
| Batch miss · 8 keys | 6.90 / 23.61 | 13.05 / 41.00 | 36.24 / 120.22 | 81.11 / 199.62 |
| Replace value | 10.06 / 28.09 | 21.50 / 51.26 | 82.01 / 211.85 | 139.50 / 391.63 |
| Delete + reinsert same key | 50.39 / 117.03 | 74.14 / 173.25 | 243.26 / 444.01 | 356.40 / 779.66 |
| Delete + insert new key | 48.05 / 103.22 | 80.03 / 172.34 | 264.48 / 496.86 | 378.17 / 773.30 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.04 / 35.88 | 19.50 / 61.48 | 79.45 / 204.16 | 151.94 / 367.81 |
| Read miss | 7.74 / 35.77 | 17.21 / 57.70 | 60.37 / 188.25 | 122.85 / 342.34 |
| Batch hit · 8 keys | 8.26 / 32.94 | 16.05 / 47.19 | 57.66 / 152.07 | 108.42 / 253.49 |
| Batch miss · 8 keys | 8.10 / 31.54 | 14.42 / 47.07 | 42.04 / 141.88 | 93.87 / 242.17 |
| Replace value | 12.33 / 38.38 | 30.69 / 62.23 | 113.08 / 217.31 | 197.66 / 377.80 |
| Delete + reinsert same key | 56.84 / 122.49 | 86.09 / 168.56 | 255.54 / 380.02 | 368.87 / 642.33 |
| Delete + insert new key | 51.50 / 121.30 | 92.42 / 160.49 | 276.10 / 400.49 | 393.05 / 637.59 |
| 4 workers · reads | 2.22 / 10.23 | 5.38 / 16.06 | 23.40 / 53.05 | 42.43 / 91.77 |
| 4 workers · 10% writes | 5.55 / 9.00 | 5.82 / 14.92 | 27.46 / 55.58 | 42.84 / 93.90 |

</details>

<details>
<summary>Memory and build — Go / Rust</summary>

Whole-process RSS, the Go runtime included, under Go's default GC settings: at its peak, after a full read pass, and at the end, after a forced GC in Go. Build is the time to insert every entry, one at a time.

**Mixed keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.55 / 2.15 | 4.64 / 2.13 | 5.55 / 2.13 | 0.0005 / 0.0004 |
| Tree | 262K | 13.03 / 9.04 | 12.88 / 9.03 | 13.03 / 9.03 | 0.0285 / 0.0281 |
| Tree | 5M | 141.28 / 136.46 | 141.02 / 136.45 | 141.28 / 136.45 | 0.6563 / 0.6465 |
| Tree | 50M | 1,500.40 / 1,347.80 | 1,500.32 / 1,347.78 | 1,500.40 / 1,347.78 | 11.5313 / 10.5823 |
| Map | 4K | 5.46 / 2.15 | 4.70 / 2.13 | 5.46 / 2.13 | 0.0006 / 0.0004 |
| Map | 262K | 12.86 / 9.04 | 12.75 / 9.03 | 12.86 / 9.03 | 0.0314 / 0.0275 |
| Map | 5M | 141.48 / 136.50 | 141.18 / 136.48 | 141.48 / 136.48 | 0.7722 / 0.6516 |
| Map | 50M | 1,486.08 / 1,348.19 | 1,486.00 / 1,348.17 | 1,486.08 / 1,348.17 | 13.6675 / 10.6485 |

**Sequential keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.51 / 2.15 | 4.62 / 2.13 | 5.51 / 2.13 | 0.0002 / 0.0001 |
| Tree | 262K | 12.26 / 8.24 | 11.89 / 8.22 | 12.26 / 8.22 | 0.0067 / 0.0033 |
| Tree | 5M | 130.99 / 120.23 | 130.78 / 120.21 | 130.99 / 120.21 | 0.0827 / 0.0444 |
| Tree | 50M | 1,376.42 / 1,184.02 | 1,376.35 / 1,184.01 | 1,376.42 / 1,184.01 | 0.8668 / 0.4072 |
| Map | 4K | 5.44 / 2.15 | 4.60 / 2.13 | 5.44 / 2.13 | 0.0001 / 0.0001 |
| Map | 262K | 12.34 / 8.24 | 12.04 / 8.22 | 12.34 / 8.22 | 0.0083 / 0.0046 |
| Map | 5M | 130.58 / 120.27 | 130.40 / 120.26 | 130.58 / 120.26 | 0.1027 / 0.0590 |
| Map | 50M | 1,376.78 / 1,184.46 | 1,376.70 / 1,184.45 | 1,376.78 / 1,184.45 | 0.9889 / 0.5050 |

**Clustered keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.55 / 2.16 | 4.70 / 2.15 | 5.55 / 2.15 | 0.0001 / 0.0002 |
| Tree | 262K | 13.55 / 11.68 | 13.45 / 11.67 | 13.55 / 11.67 | 0.0156 / 0.0274 |
| Tree | 5M | 149.37 / 187.09 | 149.13 / 187.07 | 149.37 / 187.07 | 0.2060 / 0.3919 |
| Tree | 50M | 1,449.38 / 1,853.28 | 1,449.26 / 1,853.26 | 1,449.38 / 1,853.26 | 2.3520 / 4.0313 |
| Map | 4K | 5.57 / 2.16 | 4.72 / 2.15 | 5.57 / 2.15 | 0.0003 / 0.0002 |
| Map | 262K | 13.60 / 11.68 | 13.50 / 11.67 | 13.60 / 11.67 | 0.0192 / 0.0259 |
| Map | 5M | 149.60 / 187.07 | 149.41 / 187.06 | 149.60 / 187.06 | 0.2389 / 0.3863 |
| Map | 50M | 1,449.13 / 1,853.31 | 1,449.02 / 1,853.29 | 1,449.13 / 1,853.29 | 2.6430 / 3.8956 |

</details>

<details>
<summary>Byte-string keys — Go / Rust, ns/op</summary>

Go `Bytes` keys, byte slices a program can reuse, against Rust `String`s, each a mixed key written as `user:` and 16 hex digits, with `uint64` values. Rust's containers move in keys made before the timed build; Go's copy each key's bytes into their leaves. Rust replaces values with `update` and `get_mut`, and its churn reinserts the key its removal returned. In parentheses, Go's time against Rust's: negative is faster.

| Workload | 262K | 5M |
|---|---:|---:|
| Tree · Build, per insert | 111.9 / 92.1 (+21%) | 254.5 / 202.2 (+26%) |
| Tree · Read hit | 36.9 / 38.1 (-3%) | 102.3 / 181.9 (-44%) |
| Tree · Read miss | 33.5 / 31.6 (+6%) | 82.6 / 125.4 (-34%) |
| Tree · Replace value | 43.9 / 39.0 (+13%) | 146.0 / 182.6 (-20%) |
| Tree · Delete + reinsert same key | 138.7 / 142.0 (-2%) | 341.7 / 387.7 (-12%) |
| Map · Build, per insert | 117.5 / 101.8 (+15%) | 262.7 / 214.5 (+22%) |
| Map · Read hit | 43.2 / 42.9 (+1%) | 125.4 / 170.6 (-26%) |
| Map · Read miss | 37.3 / 34.0 (+10%) | 94.7 / 91.7 (+3%) |
| Map · Replace value | 50.2 / 46.8 (+7%) | 173.8 / 178.1 (-2%) |
| Map · Delete + reinsert same key | 167.1 / 151.0 (+11%) | 391.3 / 399.2 (-2%) |
| Map · 4 workers · reads | 12.0 / 11.8 (+2%) | 36.2 / 64.5 (-44%) |

</details>

Small gaps are inconclusive: a macOS system service used one or two cores throughout the run. The [raw report](benchmarks/arm64-2026-10-30.json) holds every run, with Go's built-in `map` and `sync.Map` at 4K to 5M mixed keys; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

How the comparison is kept fair:

- **Identical work.** Both languages get the same keys, queries and operation counts. Checksums are verified, including every bit of replaced values; new-key churn uses disjoint key sets and checks that every delete and insert succeeds.
- **Fresh processes.** Every backend runs in a fresh process, in rotated order. Serial cases are pinned to one OS thread, and parallel timings exclude starting workers and reducing checksums.
- **Public APIs.** Batches go through each library's public batch API, and the Rust code is unmodified upstream.
- **Equal guarantees.** Both concurrent maps omit `len()`, and both read payloads with plain loads validated by node versions.

<details>
<summary>Commands, from source checkpoint `d9d3697`</summary>

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
