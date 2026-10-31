//go:build (darwin || linux) && (amd64 || arm64)

package wal_test

import (
	"context"
	"math"
	"syscall"
	"testing"

	"github.com/thesyncim/gobbtree/store/wal"
)

// Append panics on a key or a value longer than math.MaxUint32 bytes, as
// upstream's, before it writes any of its records. The long bytes are
// mapped and never touched, so they take no memory.
func TestAppendPanicsPastUint32(t *testing.T) {
	long, err := syscall.Mmap(-1, 0, math.MaxUint32+1, syscall.PROT_READ, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Skip(err)
	}
	defer syscall.Munmap(long)
	ctx := context.Background()
	storage := newMemoryStorage(10_000)
	w := mustOpen(t, storage)
	length, _, _ := storage.Len(ctx, 0)
	for _, r := range []wal.Record{wal.Insert(long, nil), wal.Insert(nil, long), wal.Remove(long)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			w.writer.Append(ctx, []wal.Record{wal.Insert([]byte("a"), []byte("1")), r})
		}()
	}
	if after, _, _ := storage.Len(ctx, 0); after != length {
		t.Fatalf("file 0 grew from %d to %d bytes", length, after)
	}
	unwrap(w.insert("b", "2"))(t)
	if got := mustOpen(t, storage).replayed; len(got) != 1 || got[0] != inserted("b", "2") {
		t.Fatal(got)
	}
}
