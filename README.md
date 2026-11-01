# gobbtree

[![Tests](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml)
[![amd64 SIMD](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml)

**Fast ordered maps and sets for Go, ported from Rust's [bbtree](https://github.com/XiangpengHao/bbtree).** It is a B+ tree whose nodes keep compact summaries of their keys and search them with SIMD, so most lookups never touch a full key.

1. Faster than the Rust original in 189 of the 192 `uint64` timings of the [benchmarks](#benchmarks): 1.9× its lookup throughput on one thread (`Tree`) and 1.5× on four (`Map`), on 5M keys. Rust still builds faster in 7 of the 24 build cells, and wins 11 of the 22 byte-string cells. Upstream's [store](#store-and-wal) is faster than the port: Go's inserts take 1.1–1.6× as long, and its lookups 1.8–2.1×.
2. `Map` is concurrent, with upstream's optimistic latching: lookups descend without locking, checking node versions, and writers latch only the leaf they change. On four threads it serves 2.2× the lookups of `sync.Map`, with its keys in order.
3. From 5M entries up, peak memory 20–33% below the Rust original's on clustered keys, within 3% of it on mixed keys, and 4–8% above it on sequential keys.
4. Any `cmp.Ordered` key, your own orders through a two-method `Key` interface, and `[]byte` keys.
5. File maps in upstream's format: write a snapshot, then query it in place through `mmap`. Go and Rust read each other's files.
6. Pure Go, with no cgo or dependencies: the SIMD comes from Go 1.27's `simd` package. The one piece of assembly is the store's, an identity function on amd64 and arm64 that keeps a lookup's key on the stack.
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
for k, v := range s.Scan(store.AtLeast([]byte("user:"))) { // holding each leaf: don't use s or panic here
    fmt.Printf("%s=%s\n", k, v)
}
```

`store/wal` is upstream's write-ahead log: CRC-checked records appended to numbered files through a `Storage`, a writer that fences the one before it, and a replay on open. Its files are upstream's byte for byte. As upstream's, it is a library of its own: the store doesn't log to it, and the package brings no storage of its own. Each package's documentation lists how it differs from upstream.

## Concurrency

`Map` ports the protocol of upstream's `BBTreeMap` ([design](https://github.com/XiangpengHao/bbtree/blob/8ef597b5b8d957531d88871bdb566f391206f860/design/09-concurrency.md)). Every node carries a latch word with a version. Lookups descend optimistically, checking each node's version, and write nothing. Writers latch only the leaf they change; a split or merge takes only the nodes it changes, without waiting, and starts over if another goroutine holds one. No lock or counter is shared by all writers, so writes scale with cores. String keys' lookups read their leaf without latching it too, where its summaries leave one candidate, whose bytes never change. Custom keys' lookups descend by their windows and rule out a miss without latching, as upstream's do, and compare a hit's candidates holding the leaf shared, since a key's `Compare` may read what a writer moves. A lookup whose leaf keeps changing, or whose summaries leave several candidates, takes the shared latch, which lets readers progress behind a hot leaf.

Beyond upstream, every node keeps its fences, the bounds of the keys under it, and a table of hints jumps from a key straight to its leaf. A hint is checked against the leaf's version and fences before it is used, so a stale one costs only a descent from the root.

- **Iteration** keeps upstream's `range_cloned` contract, also for `Scan`: it copies a few leaves at a time and visits the copies with nothing held, so the loop body may use the map. An `AnnexedMapOf`'s iterations instead run their bodies holding each leaf shared, as upstream's `scan` runs its closure, since its keys may read what only their leaf keeps in place; there the body must not change the map. Iteration is not a snapshot: an entry present throughout is visited exactly once, and entries inserted or removed meanwhile may or may not be.
- **Batches** are consistent per entry; a batch is not a transaction.
- **Callbacks** of `Read`, `ReadMany`, `Update`, `UpdateOrInsert` and `DeleteIf` run under the entry's leaf latch, as upstream's do, and must not use the same map. A panicking callback releases its latches and stores nothing, as Go programs may recover; upstream instead asks that user code never panic, and leaves what a map holds after one unspecified. A store's `Scan` and `ScanReverse` keep upstream's contract, so that their loop bodies inline: a body must not panic, which would leave its leaf latched.
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
| `LeafAnnex` · `WithLeafAnnex` · `update_or_insert_with_leaf` · `remove_with_leaf` · `AnnexedLeaf::annex` · `keys` | `LeafAnnex` · `AnnexedMapOf` · `UpdateOrInsertWithLeaf` · `DeleteWithLeaf` · `AnnexedLeaf.Annex` · `Keys`, for keys ordered by `Key`; a callback refuses by returning false, and `ReplaceKey` replaces a key with an equal one, as upstream's keys change in place, at the index the occupied callback is handed |
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
- **`AnnexedMapOf`:** `WalkLeaves` walks the leaves a leaf at a time, each held shared until the next, for a container that runs its own loop over a leaf's entries in an iterator the compiler inlines into its caller's range loop, as a store's scans do: Go otherwise calls a scan's loop body through a function value at every entry, where Rust inlines upstream's closure. `Hold` holds one entry's leaf shared, for a container that copies what it needs of the entry, as a store's lookups copy a value, with no callback.
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

gobbtree's `Tree` and `Map` against Rust bbtree's `unsync::BBTreeMap` and `BBTreeMap` on an Apple M4 Max: Go 1.27.3 with `GOEXPERIMENT=simd` at [`8215927`](https://github.com/thesyncim/gobbtree/commit/8215927f0be1487a27b80d8fdecd699ba167c7eb), Rust 1.99.1 with native CPU tuning, full LTO and mimalloc at [`8ef597b`](https://github.com/XiangpengHao/bbtree/tree/8ef597b5b8d957531d88871bdb566f391206f860). Each cell is the median of three fresh-process runs over identical keys, with checked results.

gobbtree is faster in 189 of the 192 `uint64` timings below; Rust wins Map 4 workers · 10% writes at 4K sequential keys; Tree delete + insert new key at 50M sequential keys; Map delete + insert new key at 50M sequential keys. Go builds faster in 17 of 24 build cells, at 0.65× to 2.09× Rust's speed. Go's process peaks 3.3–3.5 MB higher at 4K, 1.7–4.1 MB higher at 262K, from 38.6 MB lower to 9.3 MB higher at 5M, from 613.4 MB lower to 46.1 MB higher at 50M entries, against Rust's.

With byte-string keys, Go `Bytes` against Rust `String`, Go wins 11 of 22 cells.

The tables cover `uint64` keys and values from 4K to 50M entries, with mixed keys (hashed keys spread over the full 64-bit range), sequential keys (consecutive even keys; misses and fresh keys fill the gaps) and clustered keys (runs of 1,024 consecutive even keys at scattered bases). Batch cells are per key, churn cells per delete-and-insert pair, and parallel cells use four workers.

<details>
<summary>Times, mixed keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.76 / 12.76 | 11.00 / 23.89 | 48.73 / 92.82 | 65.08 / 111.84 |
| Read miss | 8.35 / 11.68 | 11.05 / 22.84 | 33.00 / 82.44 | 56.23 / 105.67 |
| Batch hit · 8 keys | 7.54 / 12.21 | 10.80 / 23.19 | 37.58 / 57.13 | 49.52 / 69.28 |
| Batch miss · 8 keys | 8.21 / 11.87 | 10.56 / 20.92 | 30.26 / 46.12 | 44.17 / 57.57 |
| Replace value | 8.04 / 12.61 | 14.93 / 26.39 | 68.02 / 93.52 | 89.52 / 113.13 |
| Delete + reinsert same key | 48.58 / 71.51 | 68.26 / 112.38 | 227.25 / 265.30 | 289.74 / 319.43 |
| Delete + insert new key | 57.49 / 95.23 | 94.18 / 149.28 | 303.75 / 395.64 | 398.25 / 494.68 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.40 / 13.53 | 14.84 / 28.07 | 64.02 / 107.79 | 82.42 / 116.99 |
| Read miss | 9.38 / 12.65 | 14.13 / 25.41 | 46.66 / 79.67 | 72.09 / 103.18 |
| Batch hit · 8 keys | 8.73 / 13.84 | 11.99 / 23.55 | 40.40 / 60.20 | 50.25 / 72.05 |
| Batch miss · 8 keys | 9.33 / 12.88 | 11.72 / 20.75 | 32.80 / 49.17 | 44.40 / 58.75 |
| Replace value | 11.37 / 16.41 | 18.84 / 31.32 | 77.34 / 132.86 | 105.63 / 149.94 |
| Delete + reinsert same key | 53.10 / 72.58 | 74.10 / 110.02 | 232.47 / 266.11 | 294.08 / 320.90 |
| Delete + insert new key | 61.42 / 99.34 | 98.17 / 140.96 | 316.17 / 380.31 | 435.30 / 490.01 |
| 4 workers · reads | 2.28 / 4.42 | 3.99 / 7.56 | 17.82 / 26.61 | 26.86 / 39.43 |
| 4 workers · 10% writes | 6.88 / 7.08 | 6.97 / 9.73 | 19.38 / 28.19 | 25.59 / 43.14 |

</details>

<details>
<summary>Times, sequential keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.59 / 11.95 | 9.85 / 24.73 | 45.16 / 85.79 | 63.63 / 106.54 |
| Read miss | 6.89 / 11.00 | 8.57 / 22.54 | 26.17 / 61.25 | 46.50 / 98.71 |
| Batch hit · 8 keys | 6.71 / 11.88 | 9.71 / 22.88 | 37.08 / 53.20 | 50.96 / 71.06 |
| Batch miss · 8 keys | 6.84 / 11.00 | 8.59 / 20.44 | 25.12 / 39.19 | 39.34 / 59.59 |
| Replace value | 6.87 / 12.49 | 15.09 / 25.51 | 69.13 / 95.18 | 96.42 / 125.25 |
| Delete + reinsert same key | 48.96 / 76.72 | 67.37 / 120.94 | 217.74 / 252.20 | 289.61 / 311.11 |
| Delete + insert new key | 45.24 / 63.80 | 67.54 / 115.30 | 238.57 / 265.87 | 308.32 / 296.65 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.65 / 13.20 | 13.03 / 28.86 | 56.26 / 104.02 | 78.75 / 118.05 |
| Read miss | 7.32 / 12.49 | 10.83 / 26.53 | 39.27 / 67.40 | 60.53 / 97.81 |
| Batch hit · 8 keys | 8.11 / 13.73 | 10.55 / 23.80 | 38.34 / 56.09 | 50.89 / 74.23 |
| Batch miss · 8 keys | 8.04 / 13.03 | 9.40 / 21.19 | 29.02 / 40.83 | 41.84 / 61.62 |
| Replace value | 9.71 / 16.59 | 17.15 / 34.16 | 78.74 / 126.24 | 108.44 / 171.85 |
| Delete + reinsert same key | 51.85 / 77.53 | 71.96 / 120.87 | 226.33 / 256.59 | 299.98 / 306.51 |
| Delete + insert new key | 48.00 / 84.80 | 70.28 / 113.78 | 238.88 / 270.90 | 345.26 / 304.58 |
| 4 workers · reads | 2.09 / 3.39 | 3.66 / 7.37 | 16.64 / 25.26 | 23.44 / 37.37 |
| 4 workers · 10% writes | 6.71 / 6.65 | 6.23 / 10.13 | 19.61 / 27.04 | 26.07 / 41.59 |

</details>

<details>
<summary>Times, clustered keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 9.18 / 25.97 | 15.67 / 49.15 | 69.55 / 193.08 | 131.73 / 362.60 |
| Read miss | 9.65 / 24.97 | 15.24 / 45.73 | 48.54 / 174.60 | 95.37 / 356.08 |
| Batch hit · 8 keys | 7.01 / 24.67 | 14.69 / 44.30 | 60.24 / 150.91 | 100.88 / 219.00 |
| Batch miss · 8 keys | 7.07 / 23.70 | 13.27 / 42.64 | 45.43 / 140.10 | 89.56 / 211.03 |
| Replace value | 8.97 / 27.87 | 20.09 / 52.88 | 87.49 / 255.18 | 137.63 / 414.78 |
| Delete + reinsert same key | 51.58 / 118.27 | 77.40 / 179.10 | 272.51 / 509.38 | 364.58 / 793.64 |
| Delete + insert new key | 49.45 / 103.16 | 82.54 / 174.21 | 295.11 / 548.52 | 398.30 / 793.22 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.20 / 39.15 | 19.83 / 63.09 | 88.63 / 231.42 | 162.32 / 387.49 |
| Read miss | 7.82 / 36.58 | 17.48 / 59.12 | 68.50 / 215.30 | 124.93 / 359.64 |
| Batch hit · 8 keys | 8.42 / 28.42 | 16.45 / 56.19 | 65.37 / 179.49 | 115.46 / 275.10 |
| Batch miss · 8 keys | 8.46 / 27.16 | 14.70 / 56.06 | 53.60 / 168.23 | 99.94 / 266.15 |
| Replace value | 12.68 / 43.41 | 25.33 / 65.85 | 112.83 / 244.29 | 171.43 / 412.40 |
| Delete + reinsert same key | 55.21 / 128.42 | 81.64 / 176.09 | 284.15 / 442.44 | 383.73 / 687.90 |
| Delete + insert new key | 51.81 / 123.62 | 88.95 / 174.04 | 303.94 / 476.69 | 410.43 / 707.95 |
| 4 workers · reads | 2.23 / 9.53 | 5.22 / 14.30 | 24.98 / 60.29 | 39.71 / 94.74 |
| 4 workers · 10% writes | 7.90 / 12.40 | 7.85 / 17.43 | 27.99 / 63.48 | 41.14 / 97.76 |

</details>

<details>
<summary>Memory and build — Go / Rust</summary>

Whole-process RSS, the Go runtime included, under Go's default GC settings: at its peak, after a full read pass, and at the end, after a forced GC in Go. Build is the time to insert every entry, one at a time.

**Mixed keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.57 / 2.18 | 4.80 / 2.16 | 5.57 / 2.16 | 0.0003 / 0.0004 |
| Tree | 262K | 12.75 / 9.08 | 12.68 / 9.06 | 12.75 / 9.06 | 0.0199 / 0.0274 |
| Tree | 5M | 140.44 / 136.50 | 140.00 / 136.48 | 140.44 / 136.48 | 0.7302 / 0.7173 |
| Tree | 50M | 1,318.11 / 1,347.83 | 1,317.98 / 1,347.81 | 1,318.11 / 1,347.81 | 11.9247 / 12.0952 |
| Map | 4K | 5.57 / 2.18 | 4.72 / 2.16 | 5.57 / 2.16 | 0.0005 / 0.0003 |
| Map | 262K | 13.09 / 9.08 | 12.89 / 9.06 | 13.09 / 9.06 | 0.0291 / 0.0273 |
| Map | 5M | 140.77 / 136.53 | 140.12 / 136.51 | 140.77 / 136.51 | 0.7746 / 0.7195 |
| Map | 50M | 1,318.58 / 1,348.22 | 1,318.34 / 1,348.21 | 1,318.58 / 1,348.21 | 12.7517 / 12.4582 |

**Sequential keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.57 / 2.18 | 4.83 / 2.16 | 5.57 / 2.16 | 0.0001 / 0.0001 |
| Tree | 262K | 12.35 / 8.27 | 11.93 / 8.26 | 12.35 / 8.26 | 0.0023 / 0.0040 |
| Tree | 5M | 129.55 / 120.26 | 129.35 / 120.24 | 129.55 / 120.24 | 0.0493 / 0.0556 |
| Tree | 50M | 1,230.13 / 1,184.06 | 1,229.83 / 1,184.04 | 1,230.13 / 1,184.04 | 0.4141 / 0.5274 |
| Map | 4K | 5.55 / 2.18 | 4.70 / 2.16 | 5.55 / 2.16 | 0.0001 / 0.0001 |
| Map | 262K | 12.42 / 8.27 | 12.01 / 8.26 | 12.42 / 8.26 | 0.0048 / 0.0053 |
| Map | 5M | 129.53 / 120.31 | 129.42 / 120.29 | 129.53 / 120.29 | 0.0579 / 0.0733 |
| Map | 50M | 1,230.11 / 1,184.50 | 1,229.83 / 1,184.48 | 1,230.11 / 1,184.48 | 0.5032 / 0.6280 |

**Clustered keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.69 / 2.20 | 4.83 / 2.18 | 5.69 / 2.18 | 0.0002 / 0.0002 |
| Tree | 262K | 13.43 / 11.71 | 13.37 / 11.70 | 13.43 / 11.70 | 0.0150 / 0.0275 |
| Tree | 5M | 148.57 / 187.12 | 148.46 / 187.11 | 148.57 / 187.11 | 0.1944 / 0.4057 |
| Tree | 50M | 1,239.94 / 1,853.31 | 1,239.63 / 1,853.29 | 1,239.94 / 1,853.29 | 2.1504 / 4.4183 |
| Map | 4K | 5.52 / 2.20 | 4.78 / 2.18 | 5.52 / 2.18 | 0.0002 / 0.0002 |
| Map | 262K | 13.45 / 11.71 | 13.34 / 11.70 | 13.45 / 11.70 | 0.0170 / 0.0212 |
| Map | 5M | 148.75 / 187.11 | 148.34 / 187.09 | 148.75 / 187.09 | 0.2112 / 0.3823 |
| Map | 50M | 1,240.01 / 1,853.34 | 1,239.66 / 1,853.33 | 1,240.01 / 1,853.33 | 2.3930 / 4.1704 |

</details>

<details>
<summary>Byte-string keys — Go / Rust, ns/op</summary>

Go `Bytes` keys, byte slices a program can reuse, against Rust `String`s, each a mixed key written as `user:` and 16 hex digits, with `uint64` values. Rust's containers move in keys made before the timed build; Go's copy each key's bytes into their leaves. Rust replaces values with `update` and `get_mut`, and its churn reinserts the key its removal returned. In parentheses, Go's time against Rust's: negative is faster.

| Workload | 262K | 5M |
|---|---:|---:|
| Tree · Build, per insert | 124.4 / 100.8 (+23%) | 292.7 / 224.0 (+31%) |
| Tree · Read hit | 37.6 / 41.3 (-9%) | 123.0 / 192.4 (-36%) |
| Tree · Read miss | 34.8 / 33.7 (+4%) | 96.7 / 136.6 (-29%) |
| Tree · Replace value | 44.6 / 42.0 (+6%) | 184.1 / 193.1 (-5%) |
| Tree · Delete + reinsert same key | 148.4 / 155.3 (-4%) | 409.4 / 429.8 (-5%) |
| Map · Build, per insert | 126.2 / 112.0 (+13%) | 295.4 / 243.2 (+21%) |
| Map · Read hit | 45.6 / 45.3 (+1%) | 144.8 / 177.0 (-18%) |
| Map · Read miss | 39.4 / 35.7 (+11%) | 110.5 / 104.8 (+5%) |
| Map · Replace value | 52.5 / 53.3 (-1%) | 203.0 / 189.0 (+7%) |
| Map · Delete + reinsert same key | 179.5 / 160.4 (+12%) | 445.1 / 447.1 (0%) |
| Map · 4 workers · reads | 11.8 / 15.3 (-23%) | 40.2 / 56.2 (-29%) |

</details>

Upstream's `bbtree-store` is faster than the port in 20 of the 24 timed store cells, at 32K, 1M and 10M entries.

<details>
<summary>Store — Go / Rust</summary>

Upstream's own benchmark, `bbtree-store/examples/bench.rs`, unmodified and built with the same tuning, against `store/cmd/storebench`, its port: 20-byte keys and 32-byte values, a fresh process a run, the two alternating, medians of three. Each time is one pass over every entry, in ns an entry, and churn's four passes, in ns an operation; eight threads' are wall time. Long values are 4,000 bytes, for one entry in 16. Go's node bytes are its heap's, once collected, past the budget's pages, where upstream counts what its allocator gives its nodes.

| Workload | 32K | 1M | 10M |
|---|---:|---:|---:|
| Insert | 166 / 106 | 264 / 193 | 341 / 308 |
| Get | 88 / 42 | 313 / 154 | 432 / 244 |
| Scan | 2.4 / 2.0 | 7.8 / 8.2 | 8.5 / 9.1 |
| Churn · a get, then a removal or a resized insert | 303 / 208 | 573 / 471 | 743 / 643 |
| 8 threads · insert | 130 / 53 | 68 / 35 | 71 / 45 |
| 8 threads · get | 22 / 14 | 40 / 21 | 61 / 36 |
| Long values · insert | 1,710 / 778 | 1,677 / 1,951 | 1,763 / 4,356 |
| Long values · get | 244 / 145 | 692 / 559 | 868 / 755 |
| Bytes an entry, nodes and heap | 127.4 / 110.3 | 123.0 / 103.7 | 131.1 / 107.9 |
| Churn · heap bytes a live record byte, after it | 1.84 / 1.55 | 1.81 / 1.52 | 1.85 / 1.54 |
| Long values · heap bytes a value byte | 1.038 / 1.049 | 1.039 / 1.032 | 1.042 / 1.029 |

</details>

Small gaps are inconclusive: other processes shared the machine, as the report's limitations say. The [raw report](benchmarks/arm64-2026-11-01.json) holds every run, with Go's built-in `map` and `sync.Map` at 4K to 5M mixed keys; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

How the comparison is kept fair:

- **Identical work.** Both languages get the same keys, queries and operation counts. Checksums are verified, including every bit of replaced values; new-key churn uses disjoint key sets and checks that every delete and insert succeeds.
- **Fresh processes.** Every backend runs in a fresh process, in rotated order. Serial cases are pinned to one OS thread, and parallel timings exclude starting workers and reducing checksums.
- **Public APIs.** Batches go through each library's public batch API, and the Rust code is unmodified upstream. The store cells run upstream's own `bbtree-store` benchmark against its line-for-line port.
- **Equal guarantees.** Both concurrent maps omit `len()`, and both read payloads with plain loads validated by node versions.
- **Interference retaken, both sides.** Where one backend's three runs of a timing segment, CPU or store, differ by more than 1.3× in their serial timings summed, as when another process takes the machine's memory bandwidth, the whole segment runs again, both languages (`internal/spread.py`); the report's limitations list each retake. Builds are single timings, reported as measured.

<details>
<summary>Commands, from source checkpoint `8215927`</summary>

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

Time the stores, upstream's benchmark built from its checkout at `8ef597b`, at 32K, 1M and 10M entries:

```sh
GOEXPERIMENT=simd go build -o storebench-go ./store/cmd/storebench
git clone https://github.com/XiangpengHao/bbtree && git -C bbtree checkout 8ef597b
RUSTFLAGS='-C target-cpu=native' CARGO_PROFILE_RELEASE_LTO=fat CARGO_PROFILE_RELEASE_CODEGEN_UNITS=1 \
  cargo build --release --locked --manifest-path bbtree/Cargo.toml -p bbtree-store --example bench
python3 internal/store_compare.py --go ./storebench-go --rust bbtree/target/release/examples/bench \
  --size 1000000 --repeat 3   # and again with --size 32768 and --size 10000000
```

`internal/matrix.sh <dir>` runs every segment above in turn, with the retakes, from the six drivers gathered in `<dir>`; `internal/assemble_report.py` gathers its records into a report, and `internal/report_drift.py` compares two reports cell by cell, Go's change beside Rust's.

`cpu_compare.py` and `rss_compare.py` take `--keys mixed` (the default), `--keys sequential` or `--keys clustered`. `cpu_compare.py --text --mutations` runs the string-key cells, which the report has at 262K and 5M entries. `python3 internal/render_benchmarks.py` redraws the cards and tables above from the saved report.

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
