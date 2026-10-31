package wal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/rand/v2"
)

// The writer, upstream's wal/writer.rs: it opens the WAL, scanning it and
// claiming it, then appends. This is the WAL's shell, which upstream leaves
// unproven: it reads and appends through the storage, and leaves what the
// bytes are to the tail (tail.go).
//
//   - A scan reads each WAL file from the tail on, has the tail scan the
//     bytes, and replays the inserts and removals they hold; then it moves
//     to the next file, if there is one, once it read the rest of this one,
//     full by then (upstream's issue 23, which it fixes).
//   - An append goes where the tail says, in the bytes it encodes. An
//     attempt whose outcome is unknown is made again, same bytes at the same
//     offset; if the offset then mismatches, the writer reads the bytes back
//     to learn whether an earlier attempt wrote them.
//   - Opening scans, then appends a claim, scanning on and claiming again
//     for as long as another writer appended first.

// appendAttempts is the attempts at an append, while its outcome is
// unknown, before it fails.
const appendAttempts = 3

// Writer is the WAL's one writer, upstream's WalWriter. It appends one
// append at a time: its methods must not be called concurrently, as
// upstream's &mut self, while lookups read through its Storage.
type Writer struct {
	storage Storage
	tail    tail
}

// Open scans the WAL in storage, giving replay each record but claims, with
// its address, in LSN order; then claims the WAL, as upstream's
// WalWriter::open. A record's key and value alias the bytes the scan read:
// copy them to keep them past the call.
func Open(ctx context.Context, storage Storage, replay func(Address, Record)) (*Writer, error) {
	return open(ctx, storage, replay, rand.Uint64())
}

// open is Open, claiming the WAL as the writer of id.
func open(ctx context.Context, storage Storage, replay func(Address, Record), id uint64) (*Writer, error) {
	writerID := binary.LittleEndian.AppendUint64(nil, id)
	claim := []part{{kind: kindClaim, key: writerID}}
	var t tail
	for {
		if err := t.scan(ctx, storage, replay); err != nil {
			return nil, err
		}
		a := newEncodedAppend(t.appendPosition(storage.MaxAppends()), claim)
		err := a.write(ctx, storage)
		if err == nil {
			return &Writer{storage: storage, tail: a.tail}, nil
		}
		// Another writer appended since the scan: scan on.
		if !errors.Is(err, ErrTailMoved) {
			return nil, err
		}
	}
}

// Append appends records as one append, and gives their addresses. It
// appends nothing for no records. After an error, the next append goes
// where this one would have, failing with ErrTailMoved if this one was
// written after all.
//
// It panics if a key or a value is longer than math.MaxUint32 bytes, as
// upstream's WalWriter::append.
func (w *Writer) Append(ctx context.Context, records []Record) ([]Address, error) {
	if len(records) == 0 {
		return nil, nil
	}
	parts := make([]part, len(records))
	for i, r := range records {
		parts[i] = r.part()
	}
	// newEncodedAppend's precondition: a record's lengths are uint32s.
	for _, p := range parts {
		if uint64(len(p.key)) > math.MaxUint32 || uint64(len(p.value)) > math.MaxUint32 {
			panic("wal: at most math.MaxUint32 bytes")
		}
	}
	a := newEncodedAppend(w.tail.appendPosition(w.storage.MaxAppends()), parts)
	if err := a.write(ctx, w.storage); err != nil {
		return nil, err
	}
	w.tail = a.tail
	return a.addresses, nil
}

// scan reads the WAL from t to its end, giving replay each record but
// claims, and moves t there.
//
// Once the next file exists, it reads the rest of the file before moving
// on: another writer may have appended to the file after the scan read it.
// A writer starts the next file only once the file is full, so its length
// is final by then. Upstream's moves on at once, and fails such an open
// with Corrupted at the next file's first record, whose LSN is past the one
// it expects (XiangpengHao/bbtree#23).
func (t *tail) scan(ctx context.Context, storage Storage, replay func(Address, Record)) error {
	fileLen, exists, err := storage.Len(ctx, t.file)
	for err == nil && exists {
		if err := t.scanFile(ctx, storage, fileLen, replay); err != nil {
			return err
		}
		var nextLen uint64
		if nextLen, exists, err = storage.Len(ctx, t.file+1); err != nil || !exists {
			break
		}
		if fileLen, _, err = storage.Len(ctx, t.file); err != nil {
			break
		}
		if fileLen != t.fileLen {
			if err := t.scanFile(ctx, storage, fileLen, replay); err != nil {
				return err
			}
		}
		*t, fileLen = t.nextFile(), nextLen
	}
	if err != nil {
		return &StorageError{Err: err}
	}
	return nil
}

// scanFile reads the last file from t to its end, fileLen, and moves t
// there. It reads it whole, and replays its records only once it scanned
// them all.
func (t *tail) scanFile(ctx context.Context, storage Storage, fileLen uint64, replay func(Address, Record)) error {
	file, start := t.file, t.fileLen
	corrupted := func(pos int) error { return &CorruptedError{File: file, Offset: start + uint64(pos)} }
	if fileLen < start {
		return corrupted(0)
	}
	// make panics for a file longer than memory, as upstream's expect.
	b := make([]byte, fileLen-start)
	if err := storage.Read(ctx, file, start, b); err != nil {
		return &StorageError{Err: err}
	}
	scanned, stop, ok := t.scanBytes(b)
	if !ok {
		return corrupted(stop)
	}
	for i, r := range scanned.records {
		switch r.kind {
		case kindInsert:
			replay(scanned.addresses[i], Insert(r.key, r.value))
		case kindRemove:
			replay(scanned.addresses[i], Remove(r.key))
		}
	}
	*t = scanned.tail
	return nil
}

// write writes the bytes to the storage, attempting again while the outcome
// is unknown. It fails with ErrTailMoved if the offset isn't the file's
// length, unless an earlier attempt wrote these bytes there.
func (a *encodedAppend) write(ctx context.Context, storage Storage) error {
	for attempt := 1; ; attempt++ {
		err := storage.Append(ctx, a.file, a.offset, a.bytes)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrOffsetMismatch):
			if attempt > 1 {
				stored, err := a.isStored(ctx, storage)
				if err != nil {
					return err
				}
				if stored {
					return nil
				}
			}
			return ErrTailMoved
		case attempt == appendAttempts:
			if unknown, ok := errors.AsType[*OutcomeUnknownError](err); ok {
				err = unknown.Err
			}
			return &StorageError{Err: err}
		}
	}
}

// isStored reports whether the storage holds these bytes at this offset.
func (a *encodedAppend) isStored(ctx context.Context, storage Storage) (bool, error) {
	stored := make([]byte, len(a.bytes))
	err := storage.Read(ctx, a.file, a.offset, stored)
	switch {
	case err == nil:
		return bytes.Equal(stored, a.bytes), nil
	case errors.Is(err, io.ErrUnexpectedEOF):
		return false, nil
	}
	return false, &StorageError{Err: err}
}
