// Package store is upstream bbtree-store's in-memory store
// (bbtree-store/src/, design/16-bbtree-store.md), ported: an ordered map of
// byte strings to byte strings that goroutines share, whose keys and values
// lie in pages a budget counts, so that a cache can stay within one.
//
// It is the tree's annexed map (gobbtree.AnnexedMapOf, upstream's BBTreeMap
// with WithLeafAnnex), whose keys point to their records, key and value, and
// whose leaves keep their records in chains of heap pages of their own, each
// leaf's annex (leaf_heap.go). A budget that refuses pages past a limit
// bounds the pages a store takes; past it, the store refuses inserts
// (ErrAlloc), and holds what it held.
//
//   - A record goes into its leaf's chain: after the last record of the
//     head, else into another page of the chain with room, else, once a
//     compaction made room, else into a new page, in front.
//   - A split moves no record: the new leaf's records lie in the old leaf's
//     chain, strays, which come home two generations on. A merge splices
//     the leaving leaf's chain in behind the head.
//   - A leaf compacts in place, sliding its records together over the holes
//     in the pages where only its records live, when a removal leaves a page
//     of its chain, and the chain, half holes; when a record finds no room
//     and a quarter of the chain is holes; or when the budget refuses a page
//     and the chain has holes at all. Pages it empties go back; pages where
//     only other leaves' records live become orphans, which free themselves
//     as their last record dies.
//   - A value too long for its record goes into overflow pages, written
//     before the insert holds the leaf, the record keeping what is left over
//     once they are full if it fits (SQLite's rule).
//   - Only a goroutine holding a record's leaf exclusively moves, changes or
//     kills it; lookups and scans read it holding the leaf shared, in the
//     map's callbacks, and a stray killed from another leaf touches only its
//     page's atomic live count.
//
// # Differences from upstream
//
// The algorithms are upstream's, with its thresholds; the differences are
// Go's memory and Go's shape.
//
//   - Pages are Go objects, which the garbage collector frees once nothing
//     points into them: Go code can't hand out an allocator's memory for
//     typed pages. So a store's allocator is a Budget, asked before the store
//     takes a page and told when it gives one back; a refused page is
//     upstream's refused allocation. The map's nodes come from the collector,
//     not the budget, which bounds heap and overflow pages only, where
//     upstream's allocator gives nodes too.
//   - No Go pointer lies in a page's bytes, so the collector sees them all. A
//     heap page is a typed object whose header holds the chain's link as a
//     pointer, and whose records follow it in the same object (heap_page.go);
//     an overflow page's link to the next is a pointer too. Each page object
//     is its page size less 8 bytes, the header Go's allocator puts before an
//     object over 512 bytes that holds pointers, so it takes exactly a page
//     of Go's heap: a heap page's room is 1,000 bytes at 1 KiB, upstream's
//     1,008, and an overflow page's 1,016 bytes of value, 1,008 here.
//   - Go doesn't align an object to its size, so a record can't find its
//     page by rounding its address down: its header holds its offset in its
//     page. It holds its key's length in 2 bytes, not 4, as a key is at most
//     a quarter of a page of at most 64 KiB, so the header stays 9 bytes, and
//     a key stays a pointer to its record, 8 bytes, as upstream's (record.go).
//     Pages are 1 KiB to 64 KiB, for 2-byte offsets, where upstream's go to 1
//     GiB; the size is a store's setting (Config), not a node config's.
//   - A record links its overflow pages by a 4-byte id in the store's table
//     of first pages, not by their 8-byte address; an id is used again once
//     its record dies, as overflow pages are only read holding their record's
//     leaf. The table is Go memory, 8 bytes a long value, outside the budget.
//   - The tree's leaves hold 32 keys whatever their size, where upstream's
//     1 KiB leaves hold about 50 store keys: a store has about twice as many
//     leaves, each chaining fewer pages, so more of its records are strays,
//     whose holes their own leaf can't take back, and more of its room lies
//     in leaves' heads. At 1M entries of 20-byte keys and 32-byte values its
//     heap holds 90.8 bytes an entry to upstream's 80.4, and under churn
//     1.81 bytes a live record byte to upstream's 1.52; a spent budget takes
//     fewer holes back in place, 41% of the removed keys at upstream's
//     test's seed to upstream's 62% (store_test.go). The thresholds are
//     upstream's; the leaves are the tree's.
//   - A record's lengths are little-endian, upstream's native-endian: the
//     same bytes on the machines both run on.
//   - The tree's Release takes no allocator, so each leaf's annex keeps its
//     store's heap, 8 bytes a leaf, set as the leaf first takes a page and
//     carried by splits and merges.
//   - Go has no Drop: Clear gives every page back, as dropping upstream's
//     store does, killing every record first while orphans or overflow pages
//     are out, and leaves the store empty. It is the owner's.
//   - A lookup's key points to a struct that holds the caller's slice,
//     marked by a key length no record has, as upstream's QueryKey points to
//     the caller's bytes; the tree never keeps a lookup's key. The tree
//     calls its keys' methods through their dictionary, so the compiler
//     can't keep that struct on the stack: a lookup, an insert and a removal
//     each allocate it, 32 bytes, where upstream's lies on the stack.
//   - The tree hands occupied the stored key but not its index in the leaf:
//     the store finds it among the leaf's keys by its pointer.
//   - GetInto appends to the caller's buffer and returns it, where upstream's
//     get_into returns the value's length. Scan and ScanReverse are
//     iterators whose loop bodies run holding each leaf shared, as upstream's
//     closures do, so the body must not use the store; Range's copies come
//     from Next and NextBack. Bounds are the store's, with the tree's
//     constructors, where upstream takes any RangeBounds; errors are values.
package store
