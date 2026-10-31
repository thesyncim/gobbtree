package wal

import (
	"context"
	"errors"
)

// Storage is the storage under the WAL, upstream's WalStorage
// (wal/storage.rs): numbered files, from 0, that take appends at their end,
// under S3 Express's rules on every backend. The writer appends while
// lookups read, so its methods are called concurrently. They must not keep
// the slices they are given.
type Storage interface {
	// MaxAppends is how many appends a file takes at most (S3 Express:
	// 10,000), upstream's MAX_APPENDS.
	MaxAppends() uint32

	// Append appends b to file file if its length is offset; offset 0
	// creates the file, which must not exist. Whole or nothing, and durable
	// once it returns nil. It fails with ErrOffsetMismatch, wrapped or not,
	// if nothing was written for the offset, and else with an
	// *OutcomeUnknownError.
	Append(ctx context.Context, file, offset uint64, b []byte) error

	// Len is file file's length; exists is false if it doesn't exist.
	Len(ctx context.Context, file uint64) (length uint64, exists bool, err error)

	// Read reads len(buf) bytes of file file from offset; it fails with
	// io.ErrUnexpectedEOF, wrapped or not, if the file holds fewer.
	Read(ctx context.Context, file, offset uint64, buf []byte) error
}

// ErrOffsetMismatch is upstream's AppendError::OffsetMismatch: the offset
// isn't the file's length, or, for offset 0, the file exists. Nothing was
// written.
var ErrOffsetMismatch = errors.New("wal: the append's offset isn't its file's length")

// OutcomeUnknownError is upstream's AppendError::OutcomeUnknown: the bytes
// may or may not have been written. Err is why.
type OutcomeUnknownError struct{ Err error }

func (e *OutcomeUnknownError) Error() string {
	return "wal: the append's outcome is unknown: " + e.Err.Error()
}

// Unwrap is why the outcome is unknown.
func (e *OutcomeUnknownError) Unwrap() error { return e.Err }
