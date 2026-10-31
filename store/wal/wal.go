// Package wal is upstream bbtree-store's WAL (bbtree-store/src/wal.rs,
// design/bbtree-store/01-wal.md), ported: records, each with its LSN and a
// CRC, appended to numbered WAL files in a Storage.
//
//   - The writer (Writer) opens the WAL: it scans every record, giving each
//     to the caller, then claims the WAL with a record of its own, which
//     fences the writer before it. Then it appends, one append at a time,
//     each written whole or not at all.
//   - Lookups read a record back by its address (Address.ReadValue), while
//     the writer appends.
//
// Its files are upstream's byte for byte, and each opens the other's. A WAL
// file is a header, the magic "bbtreeWL" then the format's version, 1, as a
// little-endian uint32, and then records:
//
//	| crc: u32 | LSN: u64 | key length: u32 | value length: u32 | kind: u8 | key … | value … |
//
// little-endian and unaligned, the CRC32C of every byte after it. The kind
// is 1 for an insert, 2 for a removal and 3 for a claim, plus 0x80 on an
// append's last record. LSNs count from 0 across the files, one a record.
// The WAL holds a whole number of appends: bytes that fail a check, a torn
// append's among them, are corruption, never the WAL's end.
//
// # Differences from upstream
//
//   - An open reads the rest of a WAL file, once the next one exists, before
//     it moves on to it: another writer may have filled the file after the
//     open read it, and started the next. Upstream's open moves on at once,
//     and fails with Corrupted on a healthy WAL, at the next file's first
//     record (XiangpengHao/bbtree#23). This is the one difference in what
//     the WAL does; the others are Go's shape.
//   - Calls block where upstream's are async, and Open, Writer.Append and
//     Address.ReadValue take a context.Context first, which they pass to
//     every Storage call and check nowhere else. Cancelling is the storage's
//     to honour by failing its call, as dropping a future at an await is in
//     Rust: an append failed so has an unknown outcome, and is attempted
//     again up to 3 times like any other.
//   - Errors are values to match with errors.Is and errors.As: upstream's
//     WalError is *StorageError, ErrTailMoved and *CorruptedError, and its
//     AppendError is ErrOffsetMismatch and *OutcomeUnknownError. The writer
//     takes any other error from Storage.Append as an unknown outcome too,
//     as the design's "Any error but OffsetMismatch leaves the outcome
//     unknown". io::ErrorKind::UnexpectedEof is io.ErrUnexpectedEOF, and
//     Storage.Len's Option is its exists result.
//   - Storage.MaxAppends is a method, where upstream's is an associated
//     constant; it must not change while a writer appends, as a constant
//     can't.
//   - Record is a struct that Insert and Remove make, where upstream's is an
//     enum: a removal still has no value. The records Open replays alias
//     the bytes it read of their file, as upstream's borrow them for the
//     call; copy a key or a value to keep it, or it keeps the file's bytes
//     in memory.
//   - Writer.Append gives nil for no records, where upstream gives an empty
//     Vec. Neither writes anything.
//   - The writer's id, its claim's key, comes from math/rand/v2, where
//     upstream hashes with a fresh std RandomState: random either way.
//   - Upstream proves the records' encoding and the scan with Verus
//     (record.rs, tail.rs, lemmas.rs). Go has no prover: their specs are
//     property tests and fuzz targets here, which check the code against
//     them.
package wal

import (
	"errors"
	"fmt"
)

// ErrTailMoved is upstream's WalError::TailMoved: the WAL's tail moved under
// the writer. Another writer claimed the WAL, or an append that failed was
// written after all. Open the WAL again.
var ErrTailMoved = errors.New("wal: the WAL's tail moved under its writer")

// StorageError is upstream's WalError::Storage: the storage failed, and an
// append's records may or may not have been written. Err is the storage's
// error; for an append whose outcome stayed unknown, its last attempt's,
// taken out of its *OutcomeUnknownError as upstream takes the io::Error out
// of its AppendError.
type StorageError struct{ Err error }

func (e *StorageError) Error() string { return "wal: the WAL's storage failed: " + e.Err.Error() }

// Unwrap is the storage's error.
func (e *StorageError) Unwrap() error { return e.Err }

// CorruptedError is upstream's WalError::Corrupted: the bytes at Offset of
// WAL file File fail their checks.
type CorruptedError struct{ File, Offset uint64 }

func (e *CorruptedError) Error() string {
	return fmt.Sprintf("wal: WAL file %d is corrupted at offset %d", e.File, e.Offset)
}
