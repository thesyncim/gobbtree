# gobbtree

[![Tests](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/test.yml)
[![amd64 SIMD](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml/badge.svg?branch=main)](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml)

**Fast ordered maps and sets for Go, ported from Rust's [bbtree](https://github.com/XiangpengHao/bbtree).** It is a B+ tree whose nodes keep compact summaries of their keys and search them with SIMD, so most lookups never touch a full key.

1. Faster than the Rust original in 189 of the 192 `uint64` timings of the [benchmarks](#benchmarks): 2.1× its lookup throughput on one thread (`Tree`) and 1.7× on four (`Map`), on 5M keys. Rust still builds faster in 10 of the 24 build cells, and wins 12 of the 22 byte-string cells. Upstream's [store](#store-and-wal) is faster than the port: Go's inserts take 1.2–1.5× as long, and its lookups 1.4–1.5×.
2. `Map` is concurrent, with upstream's optimistic latching: lookups descend without locking, checking node versions, and writers latch only the leaf they change. On four threads it serves 2.3× the lookups of `sync.Map`, with its keys in order.
3. From 5M entries up, peak memory 21–33% below the Rust original's on clustered keys, within 3% of it on mixed keys, and 4–8% above it on sequential keys.
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

With `Config.MappedPages`, on unix, a store maps its pages itself, outside Go's heap. The collector neither scans them nor counts them toward its next collection, so it collects less often: eight goroutines filling fresh 32K-entry stores insert 16% faster, one goroutine inserts 4–9% faster from 32K to 10M entries, and 1M entries under churn peak at 28% less memory. Building a large store from empty with eight goroutines is 11–16% slower instead, as the system faults its new pages in, and lookups and churn run within 4% of a store of Go's pages. Pages a store frees stay mapped for its next ones, until `Clear`, or until a collection finds the store unreachable, which its pages don't bring forward; Go's own garbage, a cleared store's nodes among it, waits longer for a collection too. A scan's slices must not outlive the loop body, as their page may be unmapped by then.

`store/wal` is upstream's write-ahead log: CRC-checked records appended to numbered files through a `Storage`, a writer that fences the one before it, and a replay on open. Its files are upstream's byte for byte. As upstream's, it is a library of its own: the store doesn't log to it, and the package brings no storage of its own. Each package's documentation lists how it differs from upstream.

## Concurrency

`Map` ports the protocol of upstream's `BBTreeMap` ([design](https://github.com/XiangpengHao/bbtree/blob/8ef597b5b8d957531d88871bdb566f391206f860/design/09-concurrency.md)). Every node carries a latch word with a version. Lookups descend optimistically, checking each node's version, and write nothing. Writers latch only the leaf they change; a split or merge takes only the nodes it changes, without waiting, and starts over if another goroutine holds one. No lock or counter is shared by all writers, so writes scale with cores. String keys' lookups read their leaf without latching it too, where its summaries leave one candidate, whose bytes never change. Custom keys' lookups descend by their windows and rule out a miss without latching, as upstream's do, and compare a hit's candidates holding the leaf shared, since a key's `Compare` may read what a writer moves. A lookup whose leaf keeps changing, or whose summaries leave several candidates, takes the shared latch, which lets readers progress behind a hot leaf. Beyond upstream, whose `get_into` holds the leaf shared, a store's `GetInto` copies a value from its record without latching, then checks the leaf's version, as upstream's `get_copied` copies a value the leaf keeps: only a writer holding the leaf exclusively changes or moves a record, and a record a stale read reaches is still memory: the collector's, or a page the store keeps mapped until it clears or goes. So `GetInto` writes nothing either, and falls back to the shared latch where the leaf changed, or the value has overflow pages; `Contains` keeps upstream's latched read, which costs less where no value is copied.

Beyond upstream, every node keeps its fences, the bounds of the keys under it, and a table of hints jumps from a key straight to its leaf. A hint is checked against the leaf's version and fences before it is used, so a stale one costs only a descent from the root. Custom keys' tables index leaves by their codes' top bits past the shared prefix, or, for text, whose bytes take few values, by those bytes ranked as the digits of a number in their base; a table whose hints still miss turns itself off, and its lookups cost what they did without one.

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

- **Custom keys' nodes:** prefixes stop at 56 bytes; and narrower widths keep leaves' windows in 8-byte slots and internal nodes' in 4-byte ones, as Go's nodes hold 32 entries whatever their windows' width.

Beyond upstream:

- **`Map`:** `First`, `Last`, `PopFirst`, `PopLast`, `LoadOrStoreFunc`, `UpdateAll` and `UpdateRange`.
- **`Tree`:** `LowerBound`, `UpperBound`, `LoadOrStore`, `LoadOrStoreFunc` and `UpdateOrInsert`.
- **`FileMap`:** `ByteLen`, `WriteTo` and `Close`.
- **`AnnexedMapOf`:** `WalkLeaves` walks the leaves a leaf at a time, each held shared until the next, for a container that runs its own loop over a leaf's entries in an iterator the compiler inlines into its caller's range loop, as a store's scans do: Go otherwise calls a scan's loop body through a function value at every entry, where Rust inlines upstream's closure. `Hold` holds one entry's leaf shared, for a container that copies what it needs of the entry; `Peek` finds a key's candidates in its leaf without latching it, for one that checks what it copies by the leaf's version, as a store's lookups copy a value; and `HoldWrite` holds a key's leaf exclusively, for one that writes its entry itself, as a store's inserts write a record, with no callbacks.
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

gobbtree's `Tree` and `Map` against Rust bbtree's `unsync::BBTreeMap` and `BBTreeMap` on an Apple M4 Max: Go 1.27.3 with `GOEXPERIMENT=simd` at [`24c8dd2`](https://github.com/thesyncim/gobbtree/commit/24c8dd21725b8c8308f91e53c608435417143e2d), Rust 1.99.1 with native CPU tuning, full LTO and mimalloc at [`8ef597b`](https://github.com/XiangpengHao/bbtree/tree/8ef597b5b8d957531d88871bdb566f391206f860). Each cell is the median of three fresh-process runs over identical keys, with checked results.

gobbtree is faster in 189 of the 192 `uint64` timings below; Rust wins Map 4 workers · 10% writes at 4K sequential keys; Tree delete + insert new key at 50M sequential keys; Map delete + insert new key at 50M sequential keys. Go builds faster in 14 of 24 build cells, at 0.73× to 2.21× Rust's speed. Go's process peaks 3.3–3.4 MB higher at 4K, 1.7–4.1 MB higher at 262K, from 38.6 MB lower to 9.3 MB higher at 5M, from 613.2 MB lower to 46.0 MB higher at 50M entries, against Rust's.

With byte-string keys, Go `Bytes` against Rust `String`, Go wins 10 of 22 cells.

The tables cover `uint64` keys and values from 4K to 50M entries, with mixed keys (hashed keys spread over the full 64-bit range), sequential keys (consecutive even keys; misses and fresh keys fill the gaps) and clustered keys (runs of 1,024 consecutive even keys at scattered bases). Batch cells are per key, churn cells per delete-and-insert pair, and parallel cells use four workers.

<details>
<summary>Times, mixed keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.66 / 12.26 | 10.64 / 22.74 | 38.15 / 81.22 | 64.98 / 107.65 |
| Read miss | 8.14 / 11.44 | 10.03 / 21.62 | 27.53 / 62.25 | 53.95 / 100.97 |
| Batch hit · 8 keys | 7.24 / 11.96 | 10.63 / 22.69 | 31.44 / 49.52 | 48.59 / 66.33 |
| Batch miss · 8 keys | 8.28 / 11.67 | 10.41 / 20.42 | 24.99 / 38.51 | 43.23 / 55.55 |
| Replace value | 7.75 / 12.44 | 14.56 / 25.15 | 51.98 / 81.12 | 87.75 / 106.52 |
| Delete + reinsert same key | 47.72 / 69.94 | 67.09 / 105.18 | 176.34 / 225.46 | 283.27 / 297.12 |
| Delete + insert new key | 57.94 / 92.04 | 89.20 / 136.80 | 240.48 / 328.13 | 387.59 / 477.01 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.25 / 13.18 | 14.32 / 26.89 | 50.34 / 92.22 | 79.83 / 108.64 |
| Read miss | 9.12 / 12.21 | 13.76 / 24.65 | 35.82 / 62.23 | 69.38 / 99.61 |
| Batch hit · 8 keys | 8.48 / 13.59 | 11.73 / 22.98 | 33.55 / 51.84 | 49.37 / 68.68 |
| Batch miss · 8 keys | 9.17 / 12.54 | 11.47 / 20.36 | 27.48 / 40.41 | 42.98 / 57.12 |
| Replace value | 11.05 / 16.08 | 18.48 / 30.33 | 61.19 / 111.22 | 101.53 / 140.88 |
| Delete + reinsert same key | 51.68 / 70.85 | 71.49 / 105.17 | 184.70 / 227.30 | 290.52 / 303.10 |
| Delete + insert new key | 61.19 / 97.67 | 93.70 / 135.12 | 255.17 / 321.48 | 424.51 / 469.19 |
| 4 workers · reads | 2.27 / 3.82 | 4.34 / 7.35 | 16.43 / 27.48 | 25.94 / 37.18 |
| 4 workers · 10% writes | 5.20 / 6.54 | 4.67 / 7.18 | 18.61 / 28.73 | 25.57 / 41.47 |

</details>

<details>
<summary>Times, sequential keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 6.36 / 11.57 | 9.61 / 23.49 | 37.22 / 73.00 | 63.46 / 94.51 |
| Read miss | 6.68 / 10.74 | 8.27 / 21.76 | 19.68 / 48.81 | 45.53 / 91.52 |
| Batch hit · 8 keys | 6.53 / 11.64 | 9.70 / 22.23 | 31.35 / 47.30 | 49.92 / 69.72 |
| Batch miss · 8 keys | 7.35 / 10.77 | 8.56 / 19.98 | 18.95 / 31.75 | 38.38 / 58.49 |
| Replace value | 6.77 / 12.22 | 14.25 / 24.77 | 52.37 / 78.39 | 94.75 / 116.08 |
| Delete + reinsert same key | 47.18 / 74.41 | 64.76 / 114.23 | 172.34 / 214.18 | 283.26 / 291.03 |
| Delete + insert new key | 43.72 / 63.16 | 65.51 / 108.34 | 185.66 / 222.68 | 302.45 / 287.11 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.60 / 12.97 | 12.44 / 27.71 | 47.62 / 89.02 | 75.08 / 110.93 |
| Read miss | 7.45 / 12.17 | 10.65 / 25.66 | 28.28 / 51.95 | 59.43 / 91.34 |
| Batch hit · 8 keys | 7.92 / 13.48 | 10.35 / 23.26 | 32.39 / 50.00 | 49.41 / 73.65 |
| Batch miss · 8 keys | 7.86 / 12.66 | 9.21 / 20.89 | 21.73 / 34.14 | 39.96 / 59.68 |
| Replace value | 9.35 / 16.19 | 16.52 / 32.36 | 59.67 / 107.75 | 106.30 / 159.46 |
| Delete + reinsert same key | 50.13 / 74.78 | 68.92 / 115.84 | 179.87 / 221.86 | 291.44 / 293.49 |
| Delete + insert new key | 47.02 / 82.15 | 67.65 / 107.83 | 193.12 / 232.60 | 312.63 / 294.64 |
| 4 workers · reads | 2.08 / 3.33 | 3.71 / 7.36 | 15.55 / 24.13 | 23.23 / 35.37 |
| 4 workers · 10% writes | 5.02 / 4.74 | 4.27 / 7.38 | 18.75 / 27.07 | 25.75 / 40.09 |

</details>

<details>
<summary>Times, clustered keys — Go / Rust, ns/op</summary>

**Tree**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 8.80 / 26.05 | 15.44 / 45.75 | 59.04 / 158.95 | 121.13 / 336.70 |
| Read miss | 8.70 / 23.53 | 15.00 / 43.53 | 36.49 / 140.47 | 85.79 / 330.95 |
| Batch hit · 8 keys | 6.94 / 22.57 | 14.26 / 43.04 | 48.70 / 127.66 | 91.88 / 204.69 |
| Batch miss · 8 keys | 7.11 / 23.23 | 13.01 / 41.29 | 33.70 / 114.63 | 79.19 / 194.16 |
| Replace value | 8.08 / 27.88 | 19.30 / 50.73 | 70.60 / 201.25 | 126.83 / 393.47 |
| Delete + reinsert same key | 50.16 / 112.01 | 73.73 / 163.70 | 231.67 / 429.89 | 343.94 / 746.72 |
| Delete + insert new key | 48.00 / 99.18 | 79.08 / 161.11 | 250.60 / 475.80 | 371.56 / 755.24 |

**Map**

| Workload | 4K | 262K | 5M | 50M |
|---|---:|---:|---:|---:|
| Read hit | 7.91 / 35.57 | 19.14 / 58.66 | 78.96 / 197.63 | 147.74 / 355.07 |
| Read miss | 7.62 / 34.68 | 16.90 / 55.82 | 50.27 / 181.49 | 113.45 / 330.68 |
| Batch hit · 8 keys | 8.22 / 32.40 | 16.05 / 54.25 | 53.98 / 156.23 | 106.00 / 254.93 |
| Batch miss · 8 keys | 8.16 / 26.91 | 14.43 / 49.70 | 40.60 / 143.59 | 89.33 / 244.49 |
| Replace value | 12.41 / 45.52 | 24.41 / 62.67 | 92.16 / 207.85 | 159.99 / 376.79 |
| Delete + reinsert same key | 53.46 / 123.12 | 77.69 / 167.74 | 239.08 / 369.36 | 352.80 / 620.72 |
| Delete + insert new key | 50.72 / 118.89 | 84.20 / 162.26 | 258.62 / 389.50 | 384.63 / 622.97 |
| 4 workers · reads | 2.21 / 11.48 | 5.37 / 15.87 | 23.09 / 52.77 | 41.16 / 92.84 |
| 4 workers · 10% writes | 5.15 / 9.07 | 5.79 / 15.40 | 26.51 / 54.25 | 42.32 / 94.03 |

</details>

<details>
<summary>Memory and build — Go / Rust</summary>

Whole-process RSS, the Go runtime included, under Go's default GC settings: at its peak, after a full read pass, and at the end, after a forced GC in Go. Build is the time to insert every entry, one at a time.

**Mixed keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.52 / 2.18 | 4.80 / 2.16 | 5.52 / 2.16 | 0.0005 / 0.0004 |
| Tree | 262K | 13.01 / 9.08 | 12.89 / 9.06 | 13.01 / 9.06 | 0.0248 / 0.0279 |
| Tree | 5M | 140.62 / 136.50 | 140.25 / 136.48 | 140.62 / 136.48 | 0.6049 / 0.6339 |
| Tree | 50M | 1,318.63 / 1,347.83 | 1,318.22 / 1,347.81 | 1,318.63 / 1,347.81 | 10.6962 / 10.3163 |
| Map | 4K | 5.55 / 2.18 | 4.70 / 2.16 | 5.55 / 2.16 | 0.0005 / 0.0004 |
| Map | 262K | 12.98 / 9.08 | 12.81 / 9.06 | 12.98 / 9.06 | 0.0283 / 0.0281 |
| Map | 5M | 140.51 / 136.53 | 140.30 / 136.51 | 140.51 / 136.51 | 0.6489 / 0.6415 |
| Map | 50M | 1,318.72 / 1,348.22 | 1,318.44 / 1,348.21 | 1,318.72 / 1,348.21 | 11.3447 / 10.3712 |

**Sequential keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.62 / 2.18 | 4.78 / 2.16 | 5.62 / 2.16 | 0.0001 / 0.0001 |
| Tree | 262K | 12.40 / 8.27 | 12.04 / 8.26 | 12.40 / 8.26 | 0.0026 / 0.0043 |
| Tree | 5M | 129.60 / 120.26 | 129.38 / 120.24 | 129.60 / 120.24 | 0.0493 / 0.0567 |
| Tree | 50M | 1,230.05 / 1,184.06 | 1,229.67 / 1,184.04 | 1,230.05 / 1,184.04 | 0.4002 / 0.5104 |
| Map | 4K | 5.54 / 2.18 | 4.77 / 2.16 | 5.54 / 2.16 | 0.0001 / 0.0001 |
| Map | 262K | 12.40 / 8.27 | 12.08 / 8.26 | 12.40 / 8.26 | 0.0044 / 0.0055 |
| Map | 5M | 129.38 / 120.31 | 129.22 / 120.29 | 129.38 / 120.29 | 0.0578 / 0.0696 |
| Map | 50M | 1,229.90 / 1,184.50 | 1,229.68 / 1,184.48 | 1,229.90 / 1,184.48 | 0.4860 / 0.6164 |

**Clustered keys**

| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |
|---|---:|---:|---:|---:|---:|
| Tree | 4K | 5.60 / 2.20 | 4.82 / 2.18 | 5.60 / 2.18 | 0.0002 / 0.0002 |
| Tree | 262K | 13.63 / 11.71 | 13.43 / 11.70 | 13.63 / 11.70 | 0.0143 / 0.0278 |
| Tree | 5M | 148.54 / 187.11 | 148.34 / 187.09 | 148.54 / 187.09 | 0.1790 / 0.3954 |
| Tree | 50M | 1,240.06 / 1,853.29 | 1,239.66 / 1,853.28 | 1,240.06 / 1,853.28 | 2.0462 / 4.0996 |
| Map | 4K | 5.62 / 2.20 | 4.75 / 2.18 | 5.62 / 2.18 | 0.0002 / 0.0002 |
| Map | 262K | 13.42 / 11.71 | 13.34 / 11.70 | 13.42 / 11.70 | 0.0168 / 0.0266 |
| Map | 5M | 148.64 / 187.12 | 148.28 / 187.11 | 148.64 / 187.11 | 0.2024 / 0.3695 |
| Map | 50M | 1,240.12 / 1,853.34 | 1,239.78 / 1,853.33 | 1,240.12 / 1,853.33 | 2.3062 / 3.9043 |

</details>

<details>
<summary>Byte-string keys — Go / Rust, ns/op</summary>

Go `Bytes` keys, byte slices a program can reuse, against Rust `String`s, each a mixed key written as `user:` and 16 hex digits, with `uint64` values. Rust's containers move in keys made before the timed build; Go's copy each key's bytes into their leaves. Rust replaces values with `update` and `get_mut`, and its churn reinserts the key its removal returned. In parentheses, Go's time against Rust's: negative is faster.

| Workload | 262K | 5M |
|---|---:|---:|
| Tree · Build, per insert | 110.9 / 93.7 (+18%) | 249.6 / 195.3 (+28%) |
| Tree · Read hit | 36.1 / 39.0 (-7%) | 102.3 / 184.0 (-44%) |
| Tree · Read miss | 33.6 / 32.4 (+4%) | 80.8 / 124.1 (-35%) |
| Tree · Replace value | 42.7 / 40.0 (+7%) | 155.3 / 182.2 (-15%) |
| Tree · Delete + reinsert same key | 141.6 / 142.9 (-1%) | 343.9 / 381.6 (-10%) |
| Map · Build, per insert | 116.9 / 105.6 (+11%) | 256.5 / 212.3 (+21%) |
| Map · Read hit | 44.6 / 43.6 (+2%) | 125.8 / 168.8 (-25%) |
| Map · Read miss | 38.2 / 33.9 (+13%) | 94.1 / 93.3 (+1%) |
| Map · Replace value | 51.7 / 48.2 (+7%) | 175.4 / 176.9 (-1%) |
| Map · Delete + reinsert same key | 165.9 / 148.0 (+12%) | 387.1 / 391.8 (-1%) |
| Map · 4 workers · reads | 12.2 / 12.0 (+1%) | 35.8 / 63.8 (-44%) |

</details>

Upstream's `bbtree-store` is faster than the port in 18 of the 24 timed store cells, at 32K, 1M and 10M entries.

<details>
<summary>Store — Go / Rust</summary>

Upstream's own benchmark, `bbtree-store/examples/bench.rs`, unmodified and built with the same tuning, against `store/cmd/storebench`, its port: 20-byte keys and 32-byte values, a fresh process a run, the two alternating, medians of three. Each time is one pass over every entry, in ns an entry, and churn's four passes, in ns an operation; eight threads' are wall time. Long values are 4,000 bytes, for one entry in 16. Go's node bytes are its heap's, once collected, past the budget's pages, where upstream counts what its allocator gives its nodes.

| Workload | 32K | 1M | 10M |
|---|---:|---:|---:|
| Insert | 146 / 97 | 212 / 152 | 338 / 279 |
| Get | 54 / 39 | 178 / 117 | 304 / 212 |
| Scan | 2.0 / 1.6 | 6.9 / 6.5 | 8.0 / 8.5 |
| Churn · a get, then a removal or a resized insert | 193 / 196 | 366 / 389 | 539 / 577 |
| 8 threads · insert | 121 / 55 | 66 / 34 | 71 / 41 |
| 8 threads · get | 12 / 14 | 29 / 19 | 38 / 35 |
| Long values · insert | 1,663 / 775 | 1,576 / 1,539 | 1,735 / 3,835 |
| Long values · get | 150 / 131 | 570 / 515 | 768 / 632 |
| Bytes an entry, nodes and heap | 129.2 / 110.3 | 125.1 / 103.7 | 132.8 / 107.9 |
| Churn · heap bytes a live record byte, after it | 1.84 / 1.55 | 1.81 / 1.52 | 1.85 / 1.54 |
| Long values · heap bytes a value byte | 1.038 / 1.049 | 1.039 / 1.032 | 1.042 / 1.029 |

</details>

Small gaps are inconclusive: other processes shared the machine, as the report's limitations say. The [raw report](benchmarks/arm64-2026-11-02.json) holds every run, with Go's built-in `map` and `sync.Map` at 4K to 5M mixed keys; amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).

## Reproducing the benchmarks

How the comparison is kept fair:

- **Identical work.** Both languages get the same keys, queries and operation counts. Checksums are verified, including every bit of replaced values; new-key churn uses disjoint key sets and checks that every delete and insert succeeds.
- **Fresh processes.** Every backend runs in a fresh process, in rotated order. Serial cases are pinned to one OS thread, and parallel timings exclude starting workers and reducing checksums.
- **Public APIs.** Batches go through each library's public batch API, and the Rust code is unmodified upstream. The store cells run upstream's own `bbtree-store` benchmark against its line-for-line port.
- **Equal guarantees.** Both concurrent maps omit `len()`, and both read payloads with plain loads validated by node versions.
- **Interference retaken, both sides.** Where one backend's three runs of a timing segment, CPU or store, differ by more than 1.3× in their serial timings summed, as when another process takes the machine's memory bandwidth, the whole segment runs again, both languages (`internal/spread.py`); the report's limitations list each retake. Builds are single timings, reported as measured.

<details>
<summary>Commands, from source checkpoint `24c8dd2`</summary>

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
