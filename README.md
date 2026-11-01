# gobbtree

[![Tests](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml)
[![amd64 SIMD](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml)

**Fast ordered maps and sets for Go, ported from Rust's [bbtree](https://github.com/XiangpengHao/bbtree).** It is a B+ tree whose nodes keep compact summaries of their keys and search them with SIMD, so most lookups never touch a full key.

1. Faster than the Rust original in 190 of the 192 `uint64` timings of the [benchmarks](#benchmarks): 1.8× its lookup throughput on one thread and 1.5× on four, on 5M keys. Rust still builds faster in 17 of the 24 build cells, ascending keys 1.2–2.9× as fast, and wins 10 of the 22 byte-string cells.
2. `Map` is concurrent, with upstream's optimistic latching: lookups descend without locking, checking node versions, and writers latch only the leaf they change. On four threads it serves 2.3× the lookups of `sync.Map`, with its keys in order.
3. From 5M entries up, peak memory 21–33% below the Rust original's on clustered keys, within 3% of it on mixed keys, and 4–8% above it on sequential ones.
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

Nodes skip what their keys share, as upstream's do: a node's windows are its keys' past the bytes its bounds share, and the map's past what all its keys share. A key may also add upstream's optional `InlineKey` items as methods, which its maps find once and call directly:

- `CompareFrom(other K, equalLen int) int` compares two keys whose first `equalLen` bytes are known equal, from there; internal nodes then keep 4 bytes of each window, as for strings.
- `HashTag() uint8` is a byte equal keys share; leaves keep each key's, and a lookup compares only the tied keys with its tag.
- `ComparePrefix(prefix []byte) int` orders a key against what all the map's keys share, checked once per search.
- `InlineWidth() InlineWidth` declares how many bytes of each window nodes keep: 2, 4 or 8.

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

A file map searches the image in place, with no rebuild, and supports `Get`, `GetMany` and iteration in both directions over any bounds. Files use upstream's version 1 format, byte for byte: the Rust oracle checks that Go writes the bytes upstream writes for the same entries, and that each side reads the other's files.

- **Types:** integer and string keys; number, plain-array, empty-struct, string and byte-slice values.
- **Checking:** `ReadFile`, `ReadFileRange` and `OpenFileMap` check every page, as upstream's do. `Mmap(...).Open()` and `OpenFileMapUnchecked` check the header alone: a corrupt file may give wrong answers or panic, but is never read out of bounds. A mapped file must not be changed or truncated while mapped, as upstream's unsafe `mmap` requires: a read past a truncated end ends the program.
- **Reading:** reads take no lock and run in parallel. `Close` is safe beside them: reads in progress finish on the snapshot, and a mapping is unmapped once none holds it.
- **Writing:** `WriteFile` replaces the file atomically, syncing it and its directory, keeps the permissions of a file it replaces, and writes through a symbolic link. `CreateFile` writes one from an ascending `iter.Seq2`. `NewFileWriter`, `WriteAt`, `ReadFileRange` and `Mmap(...).Range` handle snapshots inside larger files; `Mmap(...).Pages(OsPagesHuge)` advises huge pages on Linux.
- **Immutable:** to change a file map, write a new snapshot.

### Store and WAL

`store` is upstream's `bbtree-store`: a concurrent map from byte strings to byte strings whose every byte lies in heap pages a `Budget` grants, so that a cache can stay within a memory budget. Values of any length go to overflow pages, and freed room is compacted in place. It runs on `AnnexedMapOf`, the tree's leaf annexes, as upstream's runs on its `LeafAnnex`.

```go
s := store.New() // or store.NewIn(budget)
replaced, err := s.Insert([]byte("user:42"), []byte("Ada"))
v, ok := s.Get([]byte("user:42"))
for k, v := range s.Scan(store.AtLeast([]byte("user:"))) { // holding each leaf: don't use s here
    fmt.Printf("%s=%s\n", k, v)
}
```

`store/wal` is upstream's write-ahead log: CRC-checked records appended to numbered files through a `Storage`, a writer that fences the one before it, and a replay on open. Its files are upstream's byte for byte. As upstream's, it is a library of its own: the store doesn't log to it, and the package brings no storage of its own. Each package's documentation lists how it differs from upstream.

## Concurrency

`Map` ports the protocol of upstream's `BBTreeMap` ([design](https://github.com/XiangpengHao/bbtree/blob/8ef597b5b8d957531d88871bdb566f391206f860/design/09-concurrency.md)). Every node carries a latch word with a version. Lookups descend optimistically, checking each node's version, and write nothing. Writers latch only the leaf they change; a split or merge takes only the nodes it changes, without waiting, and starts over if another goroutine holds one. No lock or counter is shared by all writers, so writes scale with cores. Lookups of custom keys, and of string keys a leaf's summaries don't settle, read the leaf under its shared latch, as upstream's do for keys it can't read inline. A lookup whose leaf keeps changing takes that latch too, which lets readers progress behind a hot leaf.

Beyond upstream, every node keeps its fences, the bounds of the keys under it, and a table of hints jumps from a key straight to its leaf. A hint is checked against the leaf's version and fences before it is used, so a stale one costs only a descent from the root.

- **Iteration** keeps upstream's `range_cloned` contract, also for `Scan`: it copies a few leaves at a time and visits the copies with nothing held, so the loop body may use the map. An `AnnexedMapOf`'s iterations instead run their bodies holding each leaf shared, as upstream's `scan` runs its closure, since its keys may read what only their leaf keeps in place; there the body must not change the map. Iteration is not a snapshot: an entry present throughout is visited exactly once, and entries inserted or removed meanwhile may or may not be.
- **Batches** are consistent per entry; a batch is not a transaction.
- **Callbacks** of `Read`, `ReadMany`, `Update`, `UpdateOrInsert` and `DeleteIf` run under the entry's leaf latch, as upstream's do, and must not use the same map. A panicking callback releases its latches and stores nothing, as Go programs may recover; upstream instead asks that user code never panic, and leaves what a map holds after one unspecified.
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
| `InlineKey` (`inline_key_at`) · `WIDTH` · `hash_tag` · `cmp_from` · `cmp_prefix` · byte-string keys, which compare from where they differ | `Key` (`Window`) · `InlineWidth` · `HashTag` · `CompareFrom` · `ComparePrefix`, methods a key may add · `Bytes`; string and `Bytes` keys compare from where they differ |
| `LeafAnnex` · `WithLeafAnnex` · `update_or_insert_with_leaf` · `remove_with_leaf` · `AnnexedLeaf::annex` · `keys` | `LeafAnnex` · `AnnexedMapOf` · `UpdateOrInsertWithLeaf` · `DeleteWithLeaf` · `AnnexedLeaf.Annex` · `Keys`, for keys ordered by `Key`; a callback refuses by returning false, and `ReplaceKey` replaces a key with an equal one, as upstream's keys change in place |
| `FileWriter::from_ascending` · `byte_len` · `write` | `NewFileWriter` · `ByteLen` · `WriteTo`; a container's `WriteFile` writes one directly |
| `FileMap::open` · `open_unchecked` · `NotAscending::index` | `OpenFileMap` · `OpenFileMapUnchecked` · `NotAscendingError.Index` |
| bbtree-mmap `create` · `write_at` · `read` · `read_range` · `mmap().range().pages().open()` | `CreateFile` · `WriteAt` · `ReadFile` · `ReadFileRange` · `Mmap().Range().Pages().Open()` |
| bbtree-store `BBTreeStore` · `get` · `get_into` · `contains_key` · `insert` · `remove` · `scan` · `scan_rev` · `range` | `store.Store` · `Get` · `GetInto` · `Contains` · `Insert` · `Remove` · `Scan` · `ScanReverse` · `Range`; Go has no `Drop`, so `Clear` gives the pages back |
| bbtree-store `wal::WalWriter::open` · `append` · `RecordAddress::read_value` · `WalStorage` | `wal.Open` · `Writer.Append` · `Address.ReadValue` · `wal.Storage`, blocking with a `context.Context` where upstream's are async |

Different from upstream:

- **Custom keys' nodes:** prefixes stop at 56 bytes; below 8 bytes of prefix a node's windows are its keys' first, past it, so a lookup takes no other; and narrower widths keep leaves' windows in 8-byte slots and internal nodes' in 4-byte ones, as Go's nodes hold 32 entries whatever their windows' width.

Beyond upstream:

- **`Map`:** `First`, `Last`, `PopFirst`, `PopLast`, `LoadOrStoreFunc`, `UpdateAll` and `UpdateRange`.
- **`Tree`:** `LowerBound`, `UpperBound`, `LoadOrStore`, `LoadOrStoreFunc` and `UpdateOrInsert`.
- **`FileMap`:** `ByteLen`, `WriteTo` and `Close`.
- **Everywhere:** `Range(lo, hi)` and `RangeReverse` as shorthands for scanning `Between(lo, hi)`, and `Ascend(from)` and `Descend(from)` for scanning from a key.

Not carried over, because Go has no counterpart:

- **Rust-only traits:** no operator overloading for `Index` (use `Get`), and no `Hash`.
- **Allocators and `InsertError`:** Go's allocation failure is fatal, so `Insert` has no error path.
- **`NodeConfig` page sizes, not ported for now:** nodes hold a fixed 32 entries. One copy of the node code could serve several sizes, but measured with 64-entry nodes, every `Tree` and `Map` cell was slower (4–26% at 262K entries, churn up to 38%): the searches compare 8-byte codes, so a wider node costs a level's work twice over and shifts twice the entries. A store ran its lookups no faster and its inserts 5–20% slower, in 15% less memory.
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

gobbtree's `Tree` and `Map` against Rust bbtree's `unsync::BBTreeMap` and `BBTreeMap` on an Apple M4 Max: Go 1.27.3 with `GOEXPERIMENT=simd` at [`50a7b3e`](https://github.com/thesyncim/gobbtree/commit/50a7b3ec486f17216f732d3081f0480fc3e60917), Rust 1.99.1 with native CPU tuning, full LTO and mimalloc at [`8ef597b`](https://github.com/XiangpengHao/bbtree/tree/8ef597b5b8d957531d88871bdb566f391206f860). Each cell is the median of three fresh-process runs over identical keys, with checked results.

gobbtree is faster in 190 of the 192 `uint64` timings below; Rust wins Map 4 workers · 10% writes at 4K sequential keys; Map delete + insert new key at 50M sequential keys. Go builds faster in 15 of 24 build cells, at 0.59× to 2.86× Rust's speed. Go's process peaks 3.3–3.4 MB higher at 4K, 1.8–4.1 MB higher at 262K, from 38.6 MB lower to 9.4 MB higher at 5M, from 613.4 MB lower to 46.0 MB higher at 50M entries, against Rust's.

With byte-string keys, Go `Bytes` against Rust `String`, Go wins 9 of 22 cells.

The tables cover `uint64` keys and values from 4K to 50M entries, with mixed keys (hashed keys spread over the full 64-bit range), sequential keys (consecutive even keys; misses and fresh keys fill the gaps) and clustered keys (runs of 1,024 consecutive even keys at scattered bases). Batch cells are per key, churn cells per delete-and-insert pair, and parallel cells use four workers.

<details>
<summary>Times, mixed keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.63 / 12.41 | 10.93 / 23.26 | 46.70 / 90.17 | 69.81 / 121.00 |
| Read miss | 8.39 / 11.36 | 10.56 / 22.20 | 31.75 / 75.34 | 57.80 / 115.44 |
| Batch hit · 8 keys | 7.34 / 11.97 | 10.87 / 23.12 | 35.77 / 55.48 | 55.41 / 75.49 |
| Batch miss · 8 keys | 7.96 / 11.41 | 10.53 / 20.66 | 28.76 / 45.48 | 46.16 / 63.98 |
| Replace value | 7.94 / 12.38 | 14.78 / 25.60 | 63.06 / 92.57 | 92.24 / 128.01 |
| Delete + reinsert same key | 47.86 / 69.81 | 67.50 / 109.95 | 212.38 / 253.38 | 304.13 / 364.07 |
| Delete + insert new key | 56.18 / 91.87 | 91.58 / 144.33 | 287.86 / 368.23 | 408.53 / 569.34 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.35 / 13.14 | 14.61 / 27.80 | 59.07 / 104.10 | 85.21 / 128.72 |
| Read miss | 9.23 / 12.05 | 14.07 / 24.72 | 42.98 / 75.23 | 73.82 / 117.55 |
| Batch hit · 8 keys | 8.63 / 13.41 | 11.94 / 23.31 | 38.10 / 57.87 | 54.97 / 78.67 |
| Batch miss · 8 keys | 9.23 / 12.46 | 11.80 / 20.71 | 30.99 / 46.91 | 47.05 / 67.06 |
| Replace value | 12.09 / 15.66 | 18.80 / 31.10 | 73.74 / 127.49 | 107.59 / 160.26 |
| Delete + reinsert same key | 52.30 / 71.08 | 73.43 / 107.14 | 220.36 / 251.05 | 320.87 / 369.67 |
| Delete + insert new key | 60.53 / 95.98 | 95.72 / 137.28 | 301.12 / 360.63 | 479.29 / 563.74 |
| 4 workers · reads | 2.29 / 4.22 | 3.98 / 7.58 | 17.78 / 26.07 | 22.54 / 37.36 |
| 4 workers · 10% writes | 7.08 / 7.50 | 6.66 / 9.90 | 19.20 / 27.10 | 23.89 / 39.41 |

</details>

<details>
<summary>Times, sequential keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.28 / 11.64 | 9.62 / 24.00 | 43.51 / 84.11 | 102.32 / 233.47 |
| Read miss | 6.50 / 10.85 | 8.37 / 21.93 | 26.01 / 59.71 | 57.18 / 189.58 |
| Batch hit · 8 keys | 6.71 / 11.68 | 9.70 / 22.45 | 35.93 / 52.25 | 62.89 / 111.12 |
| Batch miss · 8 keys | 6.82 / 10.95 | 8.64 / 20.32 | 24.66 / 38.45 | 47.94 / 83.56 |
| Replace value | 6.76 / 12.26 | 14.77 / 25.45 | 67.67 / 92.32 | 120.84 / 183.05 |
| Delete + reinsert same key | 47.22 / 74.58 | 65.86 / 117.09 | 208.39 / 244.69 | 503.33 / 662.65 |
| Delete + insert new key | 43.60 / 61.96 | 65.51 / 111.24 | 232.16 / 255.84 | 320.35 / 601.73 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.75 / 13.01 | 13.00 / 28.04 | 55.12 / 101.73 | 81.33 / 137.14 |
| Read miss | 7.62 / 12.29 | 10.85 / 26.18 | 37.18 / 63.91 | 62.89 / 114.34 |
| Batch hit · 8 keys | 8.16 / 13.50 | 10.62 / 23.63 | 37.04 / 55.20 | 52.27 / 84.06 |
| Batch miss · 8 keys | 8.10 / 12.75 | 9.52 / 21.25 | 26.10 / 40.85 | 41.03 / 67.07 |
| Replace value | 9.87 / 16.41 | 16.88 / 32.83 | 74.35 / 124.04 | 109.77 / 165.47 |
| Delete + reinsert same key | 50.99 / 75.77 | 70.65 / 117.65 | 217.09 / 250.54 | 325.10 / 351.84 |
| Delete + insert new key | 46.92 / 82.64 | 69.30 / 110.72 | 233.54 / 260.00 | 343.31 / 334.57 |
| 4 workers · reads | 2.10 / 3.35 | 3.75 / 7.32 | 16.52 / 24.72 | 20.97 / 36.31 |
| 4 workers · 10% writes | 7.58 / 6.42 | 5.64 / 9.46 | 19.36 / 26.99 | 24.50 / 38.47 |

</details>

<details>
<summary>Times, clustered keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.89 / 25.70 | 15.80 / 47.33 | 68.94 / 191.13 | 129.76 / 362.55 |
| Read miss | 8.86 / 25.11 | 15.52 / 44.44 | 48.77 / 168.06 | 94.78 / 349.76 |
| Batch hit · 8 keys | 7.04 / 24.17 | 14.53 / 44.01 | 58.95 / 148.61 | 100.06 / 216.35 |
| Batch miss · 8 keys | 7.01 / 22.63 | 13.12 / 42.39 | 44.08 / 140.44 | 88.08 / 206.44 |
| Replace value | 8.80 / 27.51 | 19.87 / 51.18 | 85.84 / 251.07 | 136.68 / 416.54 |
| Delete + reinsert same key | 50.03 / 112.41 | 74.74 / 169.47 | 266.11 / 498.23 | 363.75 / 802.17 |
| Delete + insert new key | 47.79 / 98.61 | 79.82 / 167.94 | 290.44 / 537.64 | 392.11 / 804.85 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.09 / 35.74 | 19.77 / 61.12 | 87.79 / 231.22 | 163.81 / 381.14 |
| Read miss | 7.73 / 35.01 | 17.25 / 59.09 | 68.04 / 211.75 | 133.57 / 356.77 |
| Batch hit · 8 keys | 8.32 / 29.86 | 16.61 / 53.66 | 66.00 / 177.45 | 114.74 / 273.54 |
| Batch miss · 8 keys | 8.28 / 28.11 | 14.89 / 51.37 | 51.19 / 168.38 | 98.75 / 262.45 |
| Replace value | 12.53 / 40.53 | 24.95 / 64.76 | 110.52 / 243.09 | 171.23 / 405.86 |
| Delete + reinsert same key | 53.53 / 125.49 | 79.30 / 172.07 | 279.46 / 433.08 | 373.58 / 684.26 |
| Delete + insert new key | 50.48 / 119.91 | 85.53 / 171.74 | 300.19 / 465.18 | 406.87 / 691.81 |
| 4 workers · reads | 2.22 / 9.12 | 5.26 / 14.32 | 25.01 / 59.91 | 39.84 / 93.58 |
| 4 workers · 10% writes | 7.53 / 12.66 | 8.21 / 17.17 | 27.74 / 62.53 | 41.44 / 96.87 |

</details>

<details>
<summary>Memory and build — Go / Rust</summary>

Whole-process RSS, the Go runtime included, under Go's default GC settings: at its peak, after a full read pass, and at the end, after a forced GC in Go. Build is the time to insert every entry, one at a time.

**Mixed keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.47 / 2.18 | 4.78 / 2.16 | 5.47 / 2.16 | 0.0005 / 0.0004 |
| Tree | 262K | 13.01 / 9.08 | 12.88 / 9.06 | 13.01 / 9.06 | 0.0270 / 0.0280 |
| Tree | 5M | 140.38 / 136.50 | 140.13 / 136.48 | 140.38 / 136.48 | 0.6505 / 0.6743 |
| Tree | 50M | 1,318.55 / 1,347.83 | 1,318.32 / 1,347.81 | 1,318.55 / 1,347.81 | 11.0649 / 10.6504 |
| Map | 4K | 5.59 / 2.18 | 4.80 / 2.16 | 5.59 / 2.16 | 0.0005 / 0.0004 |
| Map | 262K | 12.96 / 9.08 | 12.88 / 9.06 | 12.96 / 9.06 | 0.0283 / 0.0285 |
| Map | 5M | 140.49 / 136.53 | 140.21 / 136.51 | 140.49 / 136.51 | 0.7169 / 0.6946 |
| Map | 50M | 1,334.74 / 1,348.22 | 1,334.48 / 1,348.21 | 1,334.74 / 1,348.21 | 11.6082 / 10.7004 |

**Sequential keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.54 / 2.18 | 4.73 / 2.16 | 5.54 / 2.16 | 0.0001 / 0.0001 |
| Tree | 262K | 12.39 / 8.27 | 12.08 / 8.26 | 12.39 / 8.26 | 0.0039 / 0.0023 |
| Tree | 5M | 129.63 / 120.26 | 129.34 / 120.24 | 129.63 / 120.24 | 0.0495 / 0.0546 |
| Tree | 50M | 1,230.03 / 1,184.06 | 1,229.70 / 1,184.04 | 1,230.03 / 1,184.04 | 0.3958 / 0.5212 |
| Map | 4K | 5.57 / 2.18 | 4.77 / 2.16 | 5.57 / 2.16 | 0.0001 / 0.0001 |
| Map | 262K | 12.22 / 8.27 | 11.91 / 8.26 | 12.22 / 8.26 | 0.0045 / 0.0056 |
| Map | 5M | 129.48 / 120.31 | 129.20 / 120.29 | 129.48 / 120.29 | 0.0556 / 0.0704 |
| Map | 50M | 1,230.05 / 1,184.50 | 1,229.68 / 1,184.48 | 1,230.05 / 1,184.48 | 0.4793 / 0.6196 |

**Clustered keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.60 / 2.20 | 4.82 / 2.18 | 5.60 / 2.18 | 0.0002 / 0.0002 |
| Tree | 262K | 13.48 / 11.71 | 13.37 / 11.70 | 13.48 / 11.70 | 0.0098 / 0.0282 |
| Tree | 5M | 148.59 / 187.11 | 148.31 / 187.09 | 148.59 / 187.09 | 0.1799 / 0.3948 |
| Tree | 50M | 1,239.92 / 1,853.29 | 1,239.61 / 1,853.28 | 1,239.92 / 1,853.28 | 2.0665 / 4.1574 |
| Map | 4K | 5.55 / 2.20 | 4.78 / 2.18 | 5.55 / 2.18 | 0.0002 / 0.0002 |
| Map | 262K | 13.50 / 11.71 | 13.42 / 11.70 | 13.50 / 11.70 | 0.0167 / 0.0260 |
| Map | 5M | 148.54 / 187.12 | 148.34 / 187.11 | 148.54 / 187.11 | 0.2041 / 0.3699 |
| Map | 50M | 1,240.06 / 1,853.34 | 1,239.70 / 1,853.33 | 1,240.06 / 1,853.33 | 2.3158 / 3.9527 |

</details>

<details>
<summary>Byte-string keys — Go / Rust, ns/op</summary>

Go `Bytes` keys, byte slices a program can reuse, against Rust `String`s, each a mixed key written as `user:` and 16 hex digits, with `uint64` values. Rust's containers move in keys made before the timed build; Go's copy each key's bytes into their leaves. Rust replaces values with `update` and `get_mut`, and its churn reinserts the key its removal returned. In parentheses, Go's time against Rust's: negative is faster.

| Workload | 262K | 5M |
|---|---:|---:|
| Tree · Build, per insert | 116.5 / 97.8 (+19%) | 287.7 / 217.8 (+32%) |
| Tree · Read hit | 37.1 / 40.0 (-7%) | 114.7 / 190.6 (-40%) |
| Tree · Read miss | 34.1 / 32.9 (+3%) | 92.7 / 134.5 (-31%) |
| Tree · Replace value | 43.8 / 40.4 (+8%) | 175.3 / 189.9 (-8%) |
| Tree · Delete + reinsert same key | 146.6 / 148.5 (-1%) | 390.8 / 421.6 (-7%) |
| Map · Build, per insert | 123.6 / 107.6 (+15%) | 291.9 / 241.8 (+21%) |
| Map · Read hit | 45.0 / 44.5 (+1%) | 142.4 / 176.1 (-19%) |
| Map · Read miss | 38.7 / 35.4 (+9%) | 109.9 / 103.5 (+6%) |
| Map · Replace value | 51.7 / 50.7 (+2%) | 200.0 / 187.3 (+7%) |
| Map · Delete + reinsert same key | 173.6 / 152.9 (+14%) | 440.9 / 435.3 (+1%) |
| Map · 4 workers · reads | 11.8 / 14.8 (-21%) | 39.7 / 55.7 (-29%) |

</details>

Small gaps are inconclusive: other processes shared the machine, as the report's limitations say. The [raw report](benchmarks/arm64-2026-11-01.json) holds every run, with Go's built-in `map` and `sync.Map` at 4K to 5M mixed keys; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

How the comparison is kept fair:

- **Identical work.** Both languages get the same keys, queries and operation counts. Checksums are verified, including every bit of replaced values; new-key churn uses disjoint key sets and checks that every delete and insert succeeds.
- **Fresh processes.** Every backend runs in a fresh process, in rotated order. Serial cases are pinned to one OS thread, and parallel timings exclude starting workers and reducing checksums.
- **Public APIs.** Batches go through each library's public batch API, and the Rust code is unmodified upstream.
- **Equal guarantees.** Both concurrent maps omit `len()`, and both read payloads with plain loads validated by node versions.

<details>
<summary>Commands, from source checkpoint `50a7b3e`</summary>

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

The tests check every container against independent references: randomized operation sequences with tree invariants, concurrent mutation and panicking callbacks, SIMD boundaries, steady-state allocations and custom key orders. CI also fuzzes file maps and the WAL, runs the amd64 kernels at each vector width, and checks containers, files (byte for byte) and WAL files against a live Rust oracle of upstream at [`8ef597b`](https://github.com/XiangpengHao/bbtree/tree/8ef597b5b8d957531d88871bdb566f391206f860) (v0.1.7):

```sh
cargo build --release --locked --manifest-path internal/oracle/Cargo.toml
GOBBTREE_ORACLE="$PWD/internal/oracle/target/release/gobbtree-oracle" \
GOBBTREE_FILE_ORACLE="$PWD/internal/oracle/target/release/file-oracle" \
GOEXPERIMENT=simd go test -run 'TestUpstream.*Oracle' -v
GOBBTREE_WAL_ORACLE="$PWD/internal/oracle/target/release/wal-oracle" \
go test ./store/wal -run TestUpstreamWALOracle -v
```

## License

Licensed under either of the MIT License or the Apache License 2.0, at your option, like upstream; [LICENSE](LICENSE) holds both texts.
