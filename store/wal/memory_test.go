package wal_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"

	"github.com/thesyncim/gobbtree/store/wal"
)

// WAL files in memory (memoryStorage), for the tests, upstream's
// tests/wal/memory.rs: they keep S3 Express's rules, and fail appends on
// demand.

// memoryStorage is WAL files in memory, which take maxAppends appends each.
// Its pointer is a handle to one storage, as upstream's clones share their
// files.
type memoryStorage struct {
	maxAppends uint32
	mu         sync.Mutex
	// files is upstream's BBTreeMap of files by number; nothing walks it in
	// order.
	files map[uint64]*memoryFile
	// faults is the faults of the next appends, in order.
	faults []appendFault
}

// appendFault is how an append fails, set by failAppend. Either way, it
// reports an *wal.OutcomeUnknownError.
type appendFault int

const (
	// requestLost is a lost request: the append doesn't happen.
	requestLost appendFault = iota
	// responseLost is a lost response: the append happens, written or not.
	responseLost
)

func (f appendFault) String() string {
	if f == requestLost {
		return "RequestLost"
	}
	return "ResponseLost"
}

type memoryFile struct {
	bytes   []byte
	appends uint32
}

func newMemoryStorage(maxAppends uint32) *memoryStorage {
	return &memoryStorage{maxAppends: maxAppends, files: map[uint64]*memoryFile{}}
}

// failAppend fails the next append that has no fault set yet with fault.
func (s *memoryStorage) failAppend(fault appendFault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, fault)
}

func (s *memoryStorage) append(file, offset uint64, b []byte) error {
	stored, exists := s.files[file]
	mismatch := offset != 0
	if exists {
		mismatch = offset == 0 || uint64(len(stored.bytes)) != offset
	}
	if mismatch {
		return wal.ErrOffsetMismatch
	}
	if !exists {
		stored = &memoryFile{}
		s.files[file] = stored
	}
	if stored.appends == s.maxAppends {
		return &wal.OutcomeUnknownError{Err: errors.New("the file takes no more appends")}
	}
	stored.bytes = append(stored.bytes, b...)
	stored.appends++
	return nil
}

func (s *memoryStorage) MaxAppends() uint32 { return s.maxAppends }

func (s *memoryStorage) Append(_ context.Context, file, offset uint64, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lost := &wal.OutcomeUnknownError{Err: errors.New("lost")}
	if len(s.faults) == 0 {
		return s.append(file, offset, b)
	}
	fault := s.faults[0]
	s.faults = s.faults[1:]
	if fault == responseLost {
		_ = s.append(file, offset, b)
	}
	return lost
}

func (s *memoryStorage) Len(_ context.Context, file uint64) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, exists := s.files[file]
	if !exists {
		return 0, false, nil
	}
	return uint64(len(stored.bytes)), true, nil
}

func (s *memoryStorage) Read(_ context.Context, file, offset uint64, buf []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, exists := s.files[file]
	if !exists {
		return fs.ErrNotExist
	}
	if offset > uint64(len(stored.bytes)) || uint64(len(buf)) > uint64(len(stored.bytes))-offset {
		return io.ErrUnexpectedEOF
	}
	copy(buf, stored.bytes[offset:])
	return nil
}
