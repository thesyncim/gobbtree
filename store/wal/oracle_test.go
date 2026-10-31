package wal

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dirStorage is WAL files in a directory, a file each, named by number in
// 16 hex digits, as the oracle's DirStorage (internal/oracle/src/bin/
// wal-oracle.rs): an append writes at the file's end, unsynced.
type dirStorage struct {
	dir        string
	maxAppends uint32
}

func (s *dirStorage) path(file uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%016x", file))
}

func (s *dirStorage) MaxAppends() uint32 { return s.maxAppends }

func (s *dirStorage) Append(_ context.Context, file, offset uint64, b []byte) error {
	flag := os.O_WRONLY | os.O_APPEND
	if offset == 0 {
		flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(s.path(file), flag, 0o644)
	switch {
	case errors.Is(err, fs.ErrExist), offset != 0 && errors.Is(err, fs.ErrNotExist):
		return ErrOffsetMismatch
	case err != nil:
		return &OutcomeUnknownError{Err: err}
	}
	err = s.appendTo(f, offset, b)
	if closeErr := f.Close(); err == nil && closeErr != nil {
		err = &OutcomeUnknownError{Err: closeErr}
	}
	return err
}

func (s *dirStorage) appendTo(f *os.File, offset uint64, b []byte) error {
	if offset != 0 {
		info, err := f.Stat()
		if err != nil {
			return &OutcomeUnknownError{Err: err}
		}
		if uint64(info.Size()) != offset {
			return ErrOffsetMismatch
		}
	}
	if _, err := f.Write(b); err != nil {
		return &OutcomeUnknownError{Err: err}
	}
	return nil
}

func (s *dirStorage) Len(_ context.Context, file uint64) (uint64, bool, error) {
	info, err := os.Stat(s.path(file))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return uint64(info.Size()), true, nil
}

func (s *dirStorage) Read(_ context.Context, file, offset uint64, buf []byte) error {
	f, err := os.Open(s.path(file))
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := f.ReadAt(buf, int64(offset))
	if n == len(buf) {
		return nil
	}
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// rustAddress is an address as the oracle prints it, RecordAddress's Debug.
func rustAddress(a Address) string {
	return fmt.Sprintf("RecordAddress { file: %d, offset: %d, len: %d }", a.file, a.offset, a.length)
}

// dumpLine is a replayed record as the oracle's dump prints it.
func dumpLine(a Address, r Record) string {
	if r.IsRemove() {
		return fmt.Sprintf("R %s key=%x\n", rustAddress(a), r.Key())
	}
	return fmt.Sprintf("I %s key=%x value=%x\n", rustAddress(a), r.Key(), r.Value())
}

// oracleScript is a script for the oracle's write: one to three writers'
// opens, each followed by up to six appends of up to four inserts and
// removals, an append of none at times.
func oracleScript(rng *rand.Rand) string {
	var b strings.Builder
	for range 1 + rng.IntN(3) {
		b.WriteString("open\n")
		for range rng.IntN(7) {
			b.WriteString("append")
			for range rng.IntN(5) {
				key := randomBytes(rng, rng.IntN(12))
				if rng.IntN(4) == 0 {
					fmt.Fprintf(&b, " R:%x", key)
				} else {
					fmt.Fprintf(&b, " I:%x:%x", key, randomBytes(rng, randomLength(rng)))
				}
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// runScript runs script on storage as the oracle's write does, the writers
// it opens claiming the WAL with ids in turn: what the oracle prints for
// it, and what its dump of the WAL after it prints.
func runScript(t *testing.T, storage Storage, script string, ids []uint64) (out, dump string) {
	t.Helper()
	ctx := context.Background()
	var o, d strings.Builder
	var w *Writer
	for line := range strings.Lines(script) {
		tokens := strings.Fields(line)
		switch tokens[0] {
		case "open":
			if len(ids) == 0 {
				t.Fatal("more opens than Rust's files hold claims")
			}
			var err error
			if w, err = open(ctx, storage, func(Address, Record) {}, ids[0]); err != nil {
				t.Fatal(err)
			}
			ids = ids[1:]
			o.WriteString("opened\n")
		case "append":
			var records []Record
			for _, token := range tokens[1:] {
				fields := strings.Split(token, ":")
				key, err := hex.DecodeString(fields[1])
				if err != nil {
					t.Fatal(err)
				}
				if fields[0] == "R" {
					records = append(records, Remove(key))
					continue
				}
				value, err := hex.DecodeString(fields[2])
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, Insert(key, value))
			}
			addresses, err := w.Append(ctx, records)
			if err != nil {
				t.Fatal(err)
			}
			var printed []string
			for i, a := range addresses {
				printed = append(printed, rustAddress(a))
				d.WriteString(dumpLine(a, records[i]))
			}
			o.WriteString(strings.Join(printed, " ") + "\n")
		}
	}
	d.WriteString("claimed\n")
	return o.String(), d.String()
}

// goDump is what the oracle's dump prints of the WAL in dir, from Go's open.
func goDump(t *testing.T, dir string, maxAppends uint32) string {
	t.Helper()
	var d strings.Builder
	_, err := Open(context.Background(), &dirStorage{dir: dir, maxAppends: maxAppends}, func(a Address, r Record) {
		d.WriteString(dumpLine(a, r))
	})
	var corrupted *CorruptedError
	switch {
	case err == nil:
		d.WriteString("claimed\n")
	case errors.As(err, &corrupted):
		fmt.Fprintf(&d, "corrupted %d %d\n", corrupted.File, corrupted.Offset)
	case errors.Is(err, ErrTailMoved):
		d.WriteString("tail moved\n")
	default:
		t.Fatal(err)
	}
	return d.String()
}

func runOracle(t *testing.T, oracle, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(oracle, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Rust %v: %v\n%s", args, err, stderr.Bytes())
	}
	return string(out)
}

// walFiles is the WAL files in dir, by number.
func walFiles(t *testing.T, dir string) [][]byte {
	t.Helper()
	var files [][]byte
	for file := uint64(0); ; file++ {
		b, err := os.ReadFile((&dirStorage{dir: dir}).path(file))
		if errors.Is(err, fs.ErrNotExist) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, b)
	}
}

// claimIDs is the ids of the writers that claimed the WAL in dir, in LSN
// order: their claims' keys.
func claimIDs(t *testing.T, dir string) []uint64 {
	t.Helper()
	var ids []uint64
	var position tail
	for file, b := range walFiles(t, dir) {
		scanned, stop, ok := position.scanBytes(b)
		if !ok {
			t.Fatalf("Rust's file %d fails its checks at %d", file, stop)
		}
		for _, r := range scanned.records {
			if r.kind == kindClaim {
				if len(r.key) != 8 {
					t.Fatalf("a claim's key of %d bytes", len(r.key))
				}
				ids = append(ids, binary.LittleEndian.Uint64(r.key))
			}
		}
		position = scanned.tail.nextFile()
	}
	return ids
}

// TestUpstreamWALOracle requires the wal-oracle binary compiled from the
// exact revision in internal/oracle/Cargo.toml: upstream's WAL, on files in
// a directory. Go and Rust run the same scripts of opens and appends, which
// must give the same addresses and the same files, byte for byte, with Go's
// writers claiming with Rust's random ids; each side replays both sides'
// files alike, and reports bytes after their last append corrupted alike.
func TestUpstreamWALOracle(t *testing.T) {
	oracle := os.Getenv("GOBBTREE_WAL_ORACLE")
	if oracle == "" {
		t.Skip("set GOBBTREE_WAL_ORACLE to the pinned Rust wal-oracle binary")
	}
	rng := rand.New(rand.NewPCG(11, 12))
	for _, maxAppends := range []uint32{1, 2, 3, 10_000} {
		// Claims at opens, filling files and starting the next, then random
		// scripts.
		scripts := []string{"open\nappend I:61:31\nopen\nappend R:61 I:62:\nappend\nopen\nappend I::\nopen\n"}
		for range 8 {
			scripts = append(scripts, oracleScript(rng))
		}
		for i, script := range scripts {
			t.Run(fmt.Sprintf("%d/%d", maxAppends, i), func(t *testing.T) {
				max := fmt.Sprint(maxAppends)
				rustDir, goDir := t.TempDir(), t.TempDir()
				rustOut := runOracle(t, oracle, script, "write", rustDir, max)
				goOut, want := runScript(t, &dirStorage{dir: goDir, maxAppends: maxAppends}, script, claimIDs(t, rustDir))
				if goOut != rustOut {
					t.Fatalf("script:\n%s\nGo appended:\n%s\nRust appended:\n%s", script, goOut, rustOut)
				}
				// Before the opens below claim the WAL again.
				rustFiles, goFiles := walFiles(t, rustDir), walFiles(t, goDir)
				if len(rustFiles) == 0 || !slices.EqualFunc(rustFiles, goFiles, bytes.Equal) {
					t.Fatalf("script:\n%s\nGo's files:\n%x\nRust's files:\n%x", script, goFiles, rustFiles)
				}
				// Claims change the files, not what they replay.
				for _, dir := range []string{rustDir, goDir} {
					if got := goDump(t, dir, maxAppends); got != want {
						t.Fatalf("Go replayed:\n%s\nwant:\n%s", got, want)
					}
					if got := runOracle(t, oracle, "", "dump", dir, max); got != want {
						t.Fatalf("Rust replayed:\n%s\nwant:\n%s", got, want)
					}
				}
				for _, dir := range []string{rustDir, goDir} {
					files := walFiles(t, dir)
					last, end := len(files)-1, len(files[len(files)-1])
					s := &dirStorage{dir: dir, maxAppends: maxAppends}
					if err := s.Append(context.Background(), uint64(last), uint64(end), []byte("not a record")); err != nil {
						t.Fatal(err)
					}
					got, rust := goDump(t, dir, maxAppends), runOracle(t, oracle, "", "dump", dir, max)
					if got != rust || !strings.HasSuffix(got, fmt.Sprintf("corrupted %d %d\n", last, end)) {
						t.Fatalf("Go replayed:\n%s\nRust replayed:\n%s", got, rust)
					}
				}
			})
		}
	}
}
