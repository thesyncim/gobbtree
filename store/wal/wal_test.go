package wal_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/thesyncim/gobbtree/store/wal"
)

// The WAL on storage in memory, upstream's tests/wal/main.rs: what a writer
// appends, an open replays, across files, lost outcomes and a second writer;
// corruption is an error.

// replayed is a record an open replayed, owning its bytes.
type replayed struct {
	remove     bool
	key, value string
}

func inserted(key, value string) replayed { return replayed{key: key, value: value} }
func removed(key string) replayed         { return replayed{remove: true, key: key} }

func replayedOf(r wal.Record) replayed {
	return replayed{remove: r.IsRemove(), key: string(r.Key()), value: string(r.Value())}
}

// opened is an opened WAL: its writer, and what the open replayed.
type opened struct {
	writer    *wal.Writer
	replayed  []replayed
	addresses []wal.Address
}

func open(storage wal.Storage) (*opened, error) {
	o := &opened{}
	writer, err := wal.Open(context.Background(), storage, func(address wal.Address, r wal.Record) {
		o.replayed = append(o.replayed, replayedOf(r))
		o.addresses = append(o.addresses, address)
	})
	if err != nil {
		return nil, err
	}
	o.writer = writer
	return o, nil
}

func mustOpen(t *testing.T, storage wal.Storage) *opened {
	t.Helper()
	o, err := open(storage)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// insert appends an insert of key, alone.
func (o *opened) insert(key, value string) (wal.Address, error) {
	return o.append(wal.Insert([]byte(key), []byte(value)))
}

// remove appends a removal of key, alone.
func (o *opened) remove(key string) (wal.Address, error) {
	return o.append(wal.Remove([]byte(key)))
}

func (o *opened) append(r wal.Record) (wal.Address, error) {
	addresses, err := o.writer.Append(context.Background(), []wal.Record{r})
	if err != nil {
		return wal.Address{}, err
	}
	return addresses[0], nil
}

// unwrap is v, failing t for err, as upstream's tests unwrap their results.
func unwrap[T any](v T, err error) func(t *testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func exists(t *testing.T, storage wal.Storage, file uint64) bool {
	t.Helper()
	_, exists, err := storage.Len(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

func TestOpenReplaysWhatWasAppended(t *testing.T) {
	ctx := context.Background()
	storage := newMemoryStorage(10_000)
	w := mustOpen(t, storage)
	if len(w.replayed) != 0 {
		t.Fatal(w.replayed)
	}
	records := []wal.Record{wal.Insert([]byte("a"), []byte("1")), wal.Insert([]byte("b"), []byte(""))}
	addresses, err := w.writer.Append(ctx, records)
	if err != nil {
		t.Fatal(err)
	}
	addresses = append(addresses, unwrap(w.remove("a"))(t))
	if none, err := w.writer.Append(ctx, nil); err != nil || len(none) != 0 {
		t.Fatal(none, err)
	}

	reopened := mustOpen(t, storage)
	want := []replayed{inserted("a", "1"), inserted("b", ""), removed("a")}
	if !slices.Equal(reopened.replayed, want) {
		t.Fatalf("replayed %v, want %v", reopened.replayed, want)
	}
	if !slices.Equal(reopened.addresses, addresses) {
		t.Fatalf("replayed at %v, appended at %v", reopened.addresses, addresses)
	}

	if value := unwrap(addresses[0].ReadValue(ctx, storage))(t); string(value) != "1" {
		t.Fatalf("%q", value)
	}
	if value := unwrap(addresses[1].ReadValue(ctx, storage))(t); string(value) != "" {
		t.Fatalf("%q", value)
	}
	// A removal has no value.
	if _, err := addresses[2].ReadValue(ctx, storage); !errors.As(err, new(*wal.CorruptedError)) {
		t.Fatal(err)
	}
}

func TestFullFileStartsTheNext(t *testing.T) {
	// Two appends a file: the open's claim and one insert fill file 0.
	storage := newMemoryStorage(2)
	w := mustOpen(t, storage)
	var want []replayed
	for i := range byte(5) {
		unwrap(w.insert(string([]byte{i}), string([]byte{i, i})))(t)
		want = append(want, inserted(string([]byte{i}), string([]byte{i, i})))
	}
	if !exists(t, storage, 2) || exists(t, storage, 3) {
		t.Fatal("five inserts after a claim, two appends a file, end in file 2")
	}

	// File 2 is full: this open's claim starts file 3.
	w = mustOpen(t, storage)
	if !slices.Equal(w.replayed, want) {
		t.Fatalf("replayed %v, want %v", w.replayed, want)
	}
	if !exists(t, storage, 3) {
		t.Fatal("the claim didn't start file 3")
	}
	address := unwrap(w.remove("\x00"))(t)
	want = append(want, removed("\x00"))

	reopened := mustOpen(t, storage)
	if !slices.Equal(reopened.replayed, want) {
		t.Fatalf("replayed %v, want %v", reopened.replayed, want)
	}
	if last := reopened.addresses[len(reopened.addresses)-1]; last != address {
		t.Fatalf("replayed at %v, appended at %v", last, address)
	}
}

func TestLostOutcomeIsWrittenOnce(t *testing.T) {
	for _, fault := range []appendFault{requestLost, responseLost} {
		storage := newMemoryStorage(10_000)
		w := mustOpen(t, storage)
		storage.failAppend(fault)
		unwrap(w.insert("a", "1"))(t)
		unwrap(w.insert("b", "2"))(t)

		want := []replayed{inserted("a", "1"), inserted("b", "2")}
		if got := mustOpen(t, storage).replayed; !slices.Equal(got, want) {
			t.Fatalf("%v: replayed %v, want %v", fault, got, want)
		}
	}
}

func TestUnknownOutcomesFailTheAppend(t *testing.T) {
	storage := newMemoryStorage(10_000)
	w := mustOpen(t, storage)
	for range 3 {
		storage.failAppend(requestLost)
	}
	_, failed := w.insert("a", "1")
	// The last attempt's error, out of its OutcomeUnknownError, as upstream.
	if storageError, ok := errors.AsType[*wal.StorageError](failed); !ok || storageError.Err.Error() != "lost" {
		t.Fatal(failed)
	}
	// It wasn't written: the next append goes where it would have.
	unwrap(w.insert("b", "2"))(t)

	// A response lost three times: the first attempt was written, and the
	// next append's offset mismatches.
	for range 3 {
		storage.failAppend(responseLost)
	}
	if _, failed := w.insert("c", "3"); !errors.As(failed, new(*wal.StorageError)) {
		t.Fatal(failed)
	}
	if _, moved := w.insert("d", "4"); !errors.Is(moved, wal.ErrTailMoved) {
		t.Fatal(moved)
	}

	want := []replayed{inserted("b", "2"), inserted("c", "3")}
	if got := mustOpen(t, storage).replayed; !slices.Equal(got, want) {
		t.Fatalf("replayed %v, want %v", got, want)
	}
}

func TestNewWriterFencesTheOneBefore(t *testing.T) {
	storage := newMemoryStorage(10_000)
	old := mustOpen(t, storage)
	unwrap(old.insert("a", "1"))(t)

	current := mustOpen(t, storage)
	if want := []replayed{inserted("a", "1")}; !slices.Equal(current.replayed, want) {
		t.Fatalf("replayed %v, want %v", current.replayed, want)
	}
	if _, moved := old.insert("b", "2"); !errors.Is(moved, wal.ErrTailMoved) {
		t.Fatal(moved)
	}
	unwrap(current.insert("c", "3"))(t)

	want := []replayed{inserted("a", "1"), inserted("c", "3")}
	if got := mustOpen(t, storage).replayed; !slices.Equal(got, want) {
		t.Fatalf("replayed %v, want %v", got, want)
	}
}

func TestCorruptionFailsTheOpen(t *testing.T) {
	ctx := context.Background()
	storage := newMemoryStorage(10_000)
	w := mustOpen(t, storage)
	unwrap(w.insert("a", "1"))(t)
	end, _, err := storage.Len(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Append(ctx, 0, end, []byte("not a record")); err != nil {
		t.Fatal(err)
	}
	_, corrupted := open(storage)
	if c, ok := errors.AsType[*wal.CorruptedError](corrupted); !ok || *c != (wal.CorruptedError{File: 0, Offset: end}) {
		t.Fatal(corrupted)
	}

	// A file of another format version.
	storage = newMemoryStorage(10_000)
	if err := storage.Append(ctx, 0, 0, []byte("bbtreeWL\x02\x00\x00\x00")); err != nil {
		t.Fatal(err)
	}
	_, corrupted = open(storage)
	if c, ok := errors.AsType[*wal.CorruptedError](corrupted); !ok || *c != (wal.CorruptedError{File: 0, Offset: 0}) {
		t.Fatal(corrupted)
	}
}

// rivalStorage is a handle to a memoryStorage that runs rival once, before
// its own storage call number before, counting Append, Len and Read from 0:
// another writer on the same files, at a point of an open its calls fix.
type rivalStorage struct {
	*memoryStorage
	calls, before int
	rival         func()
}

func (s *rivalStorage) tick() {
	if s.calls == s.before {
		s.rival()
	}
	s.calls++
}

func (s *rivalStorage) Append(ctx context.Context, file, offset uint64, b []byte) error {
	s.tick()
	return s.memoryStorage.Append(ctx, file, offset, b)
}

func (s *rivalStorage) Len(ctx context.Context, file uint64) (uint64, bool, error) {
	s.tick()
	return s.memoryStorage.Len(ctx, file)
}

func (s *rivalStorage) Read(ctx context.Context, file, offset uint64, buf []byte) error {
	s.tick()
	return s.memoryStorage.Read(ctx, file, offset, buf)
}

// An open whose claim finds that another writer appended since its scan
// scans on from where it stopped, into the file the other writer started,
// and reads what a fresh open does.
func TestOpenScansOnWhenAnotherWriterAppends(t *testing.T) {
	// Two appends a file: the first writer's claim and insert fill file 0.
	storage := newMemoryStorage(2)
	first := mustOpen(t, storage)
	unwrap(first.insert("a", "1"))(t)
	// The open's calls: Len(0), Read(0), Len(1), then its claim's Append,
	// which starts file 1, before which the other writer starts it first.
	var other *opened
	racing := &rivalStorage{memoryStorage: storage, before: 3, rival: func() {
		other = mustOpen(t, storage)
		unwrap(other.insert("b", "2"))(t)
	}}
	w := mustOpen(t, racing)
	want := []replayed{inserted("a", "1"), inserted("b", "2")}
	if !slices.Equal(w.replayed, want) {
		t.Fatalf("replayed %v, want %v", w.replayed, want)
	}
	if fresh := mustOpen(t, storage); !slices.Equal(fresh.addresses[:2], w.addresses) {
		t.Fatalf("a fresh open replays at %v, the one that scanned on at %v", fresh.addresses, w.addresses)
	}
	if _, moved := other.insert("c", "3"); !errors.Is(moved, wal.ErrTailMoved) {
		t.Fatal(moved)
	}
}

// An open during which another writer fills the file it read, and starts
// the next, reads the rest of the file before the next: upstream's reports
// a healthy WAL corrupted here, at the next file's first record, whose LSN
// is past the one it expects (XiangpengHao/bbtree#23, whose reproducer this
// is).
func TestOpenReadsTheRestOfAFileFilledDuringIt(t *testing.T) {
	// Writer A creates the WAL: file 0 holds its claim, 1 of 2 appends.
	storage := newMemoryStorage(2)
	mustOpen(t, storage)

	// Writer B opens: Len(0), Read(0), Len(1), … Between its read of file 0
	// and its Len(1), writer C opens, its claim filling file 0, and appends
	// a removal, which starts file 1.
	b := &rivalStorage{memoryStorage: storage, before: 2, rival: func() {
		c := mustOpen(t, storage)
		unwrap(c.remove("b"))(t)
	}}
	opened := mustOpen(t, b)
	want := []replayed{removed("b")}
	if !slices.Equal(opened.replayed, want) {
		t.Fatalf("replayed %v, want %v", opened.replayed, want)
	}

	// The same bytes, opened again with no other writer.
	if fresh := mustOpen(t, storage); !slices.Equal(fresh.replayed, want) {
		t.Fatalf("a fresh open replays %v, want %v", fresh.replayed, want)
	}
}

// Lookups read values back by their addresses while the writer appends, on
// one storage.
func TestLookupsReadWhileTheWriterAppends(t *testing.T) {
	ctx := context.Background()
	// Few appends a file, for the writer to start files under the readers.
	storage := newMemoryStorage(16)
	w := mustOpen(t, storage)
	type written struct {
		address wal.Address
		value   string
	}
	var (
		mu        sync.Mutex
		published []written
		done      atomic.Bool
		readers   sync.WaitGroup
	)
	for reader := range 4 {
		readers.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(reader), 1))
			for reads := 0; !done.Load() || reads < 200; {
				mu.Lock()
				n := len(published)
				var x written
				if n > 0 {
					x = published[rng.IntN(n)]
				}
				mu.Unlock()
				if n == 0 {
					runtime.Gosched()
					continue
				}
				value, err := x.address.ReadValue(ctx, storage)
				if err != nil || string(value) != x.value {
					t.Errorf("read %q, %v at %v, want %q", value, err, x.address, x.value)
					return
				}
				reads++
			}
		})
	}
	for i := range 300 {
		var records []wal.Record
		var values []string
		for j := range 1 + i%3 {
			value := fmt.Sprintf("value %d/%d", i, j)
			records = append(records, wal.Insert(fmt.Appendf(nil, "key %d/%d", i, j), []byte(value)))
			values = append(values, value)
		}
		addresses, err := w.writer.Append(ctx, records)
		if err != nil {
			t.Error(err)
			break
		}
		mu.Lock()
		for j, address := range addresses {
			published = append(published, written{address, values[j]})
		}
		mu.Unlock()
	}
	done.Store(true)
	readers.Wait()
}
