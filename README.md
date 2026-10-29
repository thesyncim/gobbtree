# gobbtree

[![Tests](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml)
[![amd64 SIMD](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml)

**Fast ordered maps and sets for Go, ported from Rust's [bbtree](https://github.com/XiangpengHao/bbtree).** It is a B+ tree whose nodes keep compact summaries of their keys and search them with SIMD, so most lookups never touch a full key.

1. Faster than the Rust original in every `uint64` timing of the [benchmarks](#benchmarks): 2.5× its lookup throughput on one thread and 1.6× on four, on 5M keys. Rust still wins most string-key cells.
2. `Map` is concurrent, with upstream's optimistic latching: lookups descend without locking, checking node versions, and writers latch only the leaf they change. On four threads it serves 2.2× the lookups of `sync.Map`, with its keys in order.
3. Lower peak memory than the Rust original from 5M entries up, as much as 38% lower on clustered keys.
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

gobbtree's `Tree` and `Map` against Rust bbtree's `unsync::BBTreeMap` and `BBTreeMap` on an Apple M4 Max: Go 1.27.3 with `GOEXPERIMENT=simd` at [`c55ab0c`](https://github.com/thesyncim/gobbtree/commit/c55ab0c8f7a943599570bade089bae583d719081), Rust 1.99.1 with native CPU tuning, full LTO and mimalloc at [`1c72bd2`](https://github.com/XiangpengHao/bbtree/tree/1c72bd27994ed0a38694b029618e80115e22644f). Each cell is the median of three fresh-process runs over identical keys, with checked results.

gobbtree is faster in every `uint64` timing below. Building `uint64` maps is faster in Rust at 4K entries, and 1.4× faster for the sequential `Tree` at 262K and 5M; Go builds clustered keys 1.5–2.3× faster from 262K up, and the other builds are within 20% either way. Go's process peaks 1.4–3.6 MB higher at 4K and 262K entries, and lower from 5M up. With string keys Rust still wins 14 of 22 cells, among them every build and most of the `Map`'s.

The tables cover `uint64` keys and values from 4K to 50M entries, with mixed keys (hashed keys spread over the full 64-bit range), sequential keys (consecutive even keys; misses and fresh keys fill the gaps) and clustered keys (runs of 1,024 consecutive even keys at scattered bases). Batch cells are per key, churn cells per delete-and-insert pair, and parallel cells use four workers.

<details>
<summary>Times, mixed keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.82 / 13.19 | 10.05 / 23.98 | 34.23 / 84.33 | 71.23 / 129.28 |
| Read miss | 8.66 / 12.20 | 12.13 / 23.28 | 40.71 / 74.76 | 80.26 / 117.71 |
| Batch hit · 8 keys | 11.72 / 12.52 | 12.88 / 24.32 | 34.24 / 51.24 | 69.40 / 76.06 |
| Batch miss · 8 keys | 11.37 / 12.30 | 12.79 / 20.92 | 25.83 / 42.81 | 55.64 / 65.64 |
| Replace value | 12.28 / 13.26 | 17.21 / 26.89 | 52.09 / 87.46 | 98.17 / 138.91 |
| Delete + reinsert same key | 34.84 / 77.53 | 51.54 / 119.04 | 127.78 / 249.71 | 263.11 / 370.27 |
| Delete + insert new key | 61.51 / 101.53 | 89.37 / 154.30 | 222.58 / 366.35 | 423.05 / 581.11 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 10.36 / 14.16 | 16.59 / 29.57 | 58.78 / 98.94 | 99.21 / 141.46 |
| Read miss | 11.17 / 12.79 | 15.40 / 26.03 | 41.24 / 69.23 | 83.98 / 124.02 |
| Batch hit · 8 keys | 11.43 / 14.02 | 15.06 / 22.81 | 36.39 / 51.14 | 57.67 / 76.38 |
| Batch miss · 8 keys | 11.87 / 13.26 | 14.05 / 19.91 | 29.43 / 45.41 | 51.24 / 64.60 |
| Replace value | 12.28 / 14.83 | 22.10 / 31.76 | 85.17 / 126.02 | 128.41 / 168.16 |
| Delete + reinsert same key | 51.83 / 78.39 | 70.51 / 113.60 | 226.59 / 263.41 | 291.70 / 418.16 |
| Delete + insert new key | 57.73 / 108.68 | 95.82 / 149.87 | 314.35 / 381.66 | 464.83 / 633.18 |
| 4 workers · reads | 2.79 / 4.20 | 4.36 / 7.53 | 17.57 / 28.38 | 25.52 / 38.48 |
| 4 workers · 10% writes | 7.11 / 7.40 | 7.76 / 9.30 | 18.85 / 29.21 | 26.94 / 41.03 |

</details>

<details>
<summary>Times, sequential keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 5.39 / 11.99 | 8.50 / 24.36 | 38.37 / 89.86 | 86.76 / 100.28 |
| Read miss | 5.89 / 11.14 | 8.72 / 22.41 | 29.71 / 66.93 | 56.91 / 95.55 |
| Batch hit · 8 keys | 10.27 / 11.93 | 12.89 / 22.24 | 36.06 / 53.18 | 56.97 / 71.01 |
| Batch miss · 8 keys | 9.88 / 11.17 | 12.03 / 20.10 | 23.37 / 38.96 | 49.79 / 58.04 |
| Replace value | 11.31 / 12.55 | 15.48 / 25.95 | 52.80 / 101.63 | 91.30 / 135.19 |
| Delete + reinsert same key | 34.88 / 79.53 | 51.59 / 124.30 | 167.21 / 265.45 | 241.31 / 331.54 |
| Delete + insert new key | 34.36 / 65.71 | 58.13 / 118.13 | 206.65 / 273.17 | 272.78 / 325.13 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.39 / 13.54 | 14.25 / 29.11 | 63.69 / 106.14 | 113.79 / 116.76 |
| Read miss | 8.39 / 12.73 | 11.54 / 26.49 | 30.14 / 64.83 | 69.34 / 104.22 |
| Batch hit · 8 keys | 10.64 / 13.84 | 12.37 / 23.09 | 34.50 / 53.57 | 51.27 / 72.07 |
| Batch miss · 8 keys | 10.20 / 13.05 | 11.24 / 20.56 | 23.74 / 38.64 | 40.09 / 60.40 |
| Replace value | 11.06 / 15.31 | 19.38 / 32.50 | 85.76 / 122.22 | 121.90 / 161.77 |
| Delete + reinsert same key | 50.07 / 81.06 | 67.54 / 123.37 | 203.76 / 255.02 | 276.01 / 332.09 |
| Delete + insert new key | 41.61 / 86.30 | 61.85 / 114.33 | 211.00 / 268.76 | 286.30 / 316.94 |
| 4 workers · reads | 2.50 / 3.43 | 3.90 / 7.36 | 17.43 / 26.04 | 25.20 / 35.82 |
| 4 workers · 10% writes | 6.04 / 6.91 | 5.32 / 9.09 | 19.05 / 28.16 | 26.45 / 40.31 |

</details>

<details>
<summary>Times, clustered keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 5.84 / 26.81 | 11.64 / 48.29 | 44.03 / 162.44 | 108.39 / 340.39 |
| Read miss | 6.60 / 24.99 | 13.23 / 45.22 | 43.42 / 140.31 | 99.63 / 333.13 |
| Batch hit · 8 keys | 16.98 / 24.11 | 31.27 / 42.96 | 76.28 / 125.14 | 105.87 / 208.60 |
| Batch miss · 8 keys | 17.06 / 23.77 | 31.52 / 41.90 | 67.02 / 112.97 | 101.52 / 199.94 |
| Replace value | 10.81 / 28.25 | 18.50 / 51.95 | 59.58 / 200.97 | 130.82 / 402.21 |
| Delete + reinsert same key | 35.16 / 119.63 | 54.03 / 176.59 | 151.90 / 439.24 | 283.02 / 758.51 |
| Delete + insert new key | 36.43 / 103.85 | 63.25 / 174.29 | 186.45 / 484.16 | 330.33 / 740.40 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.87 / 37.02 | 21.64 / 63.03 | 76.54 / 200.70 | 169.57 / 361.03 |
| Read miss | 8.87 / 34.29 | 18.31 / 60.34 | 44.47 / 183.22 | 126.10 / 337.69 |
| Batch hit · 8 keys | 10.90 / 27.06 | 18.72 / 48.98 | 50.49 / 153.43 | 112.43 / 248.58 |
| Batch miss · 8 keys | 10.38 / 28.97 | 17.71 / 48.01 | 35.78 / 143.37 | 103.65 / 238.21 |
| Replace value | 11.57 / 38.64 | 26.35 / 63.34 | 88.40 / 208.06 | 179.23 / 376.36 |
| Delete + reinsert same key | 50.57 / 125.76 | 74.82 / 162.38 | 211.59 / 400.23 | 333.94 / 640.53 |
| Delete + insert new key | 43.99 / 122.04 | 78.92 / 164.58 | 222.83 / 391.91 | 358.66 / 641.77 |
| 4 workers · reads | 2.67 / 9.46 | 5.75 / 16.10 | 21.57 / 52.83 | 41.64 / 91.27 |
| 4 workers · 10% writes | 5.15 / 9.65 | 6.70 / 16.83 | 24.63 / 54.07 | 42.23 / 96.26 |

</details>

<details>
<summary>Memory and build — Go / Rust</summary>

Whole-process RSS, the Go runtime included, under Go's default GC settings: at its peak, after a full read pass, and at the end, after a forced GC in Go. Build is the time to insert every entry, one at a time.

**Mixed keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.60 / 2.15 | 4.78 / 2.13 | 5.60 / 2.13 | 0.0005 / 0.0002 |
| Tree | 262K | 12.65 / 9.04 | 12.57 / 9.03 | 12.65 / 9.03 | 0.0278 / 0.0285 |
| Tree | 5M | 127.78 / 136.46 | 127.73 / 136.45 | 127.78 / 136.45 | 0.5429 / 0.6311 |
| Tree | 50M | 1,225.92 / 1,347.80 | 1,225.87 / 1,347.78 | 1,225.92 / 1,347.78 | 10.3872 / 10.4440 |
| Map | 4K | 5.69 / 2.15 | 4.88 / 2.13 | 5.69 / 2.13 | 0.0006 / 0.0004 |
| Map | 262K | 12.47 / 9.04 | 12.14 / 9.03 | 12.47 / 9.03 | 0.0286 / 0.0287 |
| Map | 5M | 132.51 / 136.50 | 132.48 / 136.48 | 132.51 / 136.48 | 0.6027 / 0.6369 |
| Map | 50M | 1,252.67 / 1,348.19 | 1,252.56 / 1,348.17 | 1,252.67 / 1,348.17 | 10.7269 / 10.6927 |

**Sequential keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.59 / 2.15 | 4.72 / 2.13 | 5.59 / 2.13 | 0.0000 / 0.0000 |
| Tree | 262K | 11.34 / 8.24 | 10.94 / 8.22 | 11.34 / 8.22 | 0.0036 / 0.0025 |
| Tree | 5M | 105.69 / 120.23 | 105.66 / 120.21 | 105.69 / 120.21 | 0.0496 / 0.0356 |
| Tree | 50M | 1,008.94 / 1,184.01 | 1,008.91 / 1,183.99 | 1,008.94 / 1,183.99 | 0.3972 / 0.4114 |
| Map | 4K | 5.65 / 2.15 | 4.80 / 2.13 | 5.65 / 2.13 | 0.0001 / 0.0000 |
| Map | 262K | 11.83 / 8.24 | 11.44 / 8.22 | 11.83 / 8.22 | 0.0037 / 0.0044 |
| Map | 5M | 119.28 / 120.27 | 119.21 / 120.26 | 119.28 / 120.26 | 0.0578 / 0.0597 |
| Map | 50M | 1,136.90 / 1,184.46 | 1,136.74 / 1,184.45 | 1,136.90 / 1,184.45 | 0.5368 / 0.5172 |

**Clustered keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.75 / 2.16 | 4.87 / 2.15 | 5.75 / 2.15 | 0.0002 / 0.0002 |
| Tree | 262K | 13.19 / 11.68 | 13.06 / 11.67 | 13.19 / 11.67 | 0.0159 / 0.0276 |
| Tree | 5M | 132.58 / 187.07 | 132.55 / 187.06 | 132.58 / 187.06 | 0.1889 / 0.4003 |
| Tree | 50M | 1,138.98 / 1,853.28 | 1,138.95 / 1,853.26 | 1,138.98 / 1,853.26 | 1.8260 / 4.1662 |
| Map | 4K | 5.73 / 2.16 | 4.92 / 2.15 | 5.73 / 2.15 | 0.0002 / 0.0002 |
| Map | 262K | 13.09 / 11.68 | 12.96 / 11.67 | 13.09 / 11.67 | 0.0176 / 0.0259 |
| Map | 5M | 141.59 / 187.07 | 141.54 / 187.06 | 141.59 / 187.06 | 0.2145 / 0.3813 |
| Map | 50M | 1,234.19 / 1,853.31 | 1,234.16 / 1,853.29 | 1,234.19 / 1,853.29 | 2.3823 / 4.0440 |

</details>

<details>
<summary>String keys — Go / Rust, ns/op</summary>

Go `string` keys against Rust `String`s, each a mixed key written as `user:` and 16 hex digits, with `uint64` values. Rust's containers move in keys made before the timed build; Go's copy each key's bytes into their leaves. Rust replaces values with `update` and `get_mut`, and its churn reinserts the key its removal returned. In parentheses, Go's time against Rust's: negative is faster.

| Workload | 262K | 5M |
|---|---:|---:|
| Tree · Build, per insert | 115.5 / 95.4 (+21%) | 240.8 / 195.7 (+23%) |
| Tree · Read hit | 36.2 / 39.7 (-9%) | 102.5 / 181.8 (-44%) |
| Tree · Read miss | 33.3 / 33.0 (+1%) | 82.3 / 120.8 (-32%) |
| Tree · Replace value | 46.5 / 41.0 (+13%) | 158.2 / 179.2 (-12%) |
| Tree · Delete + reinsert same key | 143.7 / 146.8 (-2%) | 319.1 / 393.6 (-19%) |
| Map · Build, per insert | 141.8 / 104.9 (+35%) | 313.9 / 212.9 (+47%) |
| Map · Read hit | 54.5 / 44.3 (+23%) | 141.5 / 168.9 (-16%) |
| Map · Read miss | 50.0 / 34.8 (+44%) | 131.6 / 91.8 (+43%) |
| Map · Replace value | 87.7 / 48.5 (+81%) | 249.1 / 176.5 (+41%) |
| Map · Delete + reinsert same key | 193.9 / 151.4 (+28%) | 410.6 / 399.9 (+3%) |
| Map · 4 workers · reads | 13.9 / 12.4 (+13%) | 39.4 / 61.5 (-36%) |

</details>

Small gaps are inconclusive: a macOS system service used one or two cores throughout the run. The [raw report](benchmarks/arm64-2026-10-29.json) holds every run, with Go's built-in `map` and `sync.Map` at 4K to 5M mixed keys; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

How the comparison is kept fair:

- **Identical work.** Both languages get the same keys, queries and operation counts. Checksums are verified, including every bit of replaced values; new-key churn uses disjoint key sets and checks that every delete and insert succeeds.
- **Fresh processes.** Every backend runs in a fresh process, in rotated order. Serial cases are pinned to one OS thread, and parallel timings exclude starting workers and reducing checksums.
- **Public APIs.** Batches go through each library's public batch API, and the Rust code is unmodified upstream.
- **Equal guarantees.** Both concurrent maps omit `len()`, and both read payloads with plain loads validated by node versions.

<details>
<summary>Commands, from source checkpoint `c55ab0c`</summary>

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
