//go:build unix

package gobbtree

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// WriteFile replaces a file as os.WriteFile would keep it: its permissions,
// and, through a symbolic link, its target, the link left in place. A new
// file takes the umask's permissions.
func TestWriteFileKeepsWhatItReplaces(t *testing.T) {
	dir := t.TempDir()
	w, err := NewFileWriter(fileEntries(1000))
	if err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "fresh.bbt")
	if err := w.WriteFile(fresh); err != nil {
		t.Fatal(err)
	}
	umask := syscall.Umask(0)
	syscall.Umask(umask)
	if info, _ := os.Stat(fresh); info.Mode().Perm() != 0o666&^os.FileMode(umask) {
		t.Fatalf("new file mode %v, umask %o", info.Mode().Perm(), umask)
	}
	kept := filepath.Join(dir, "kept.bbt")
	if err := os.WriteFile(kept, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(kept, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile(kept); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(kept); info.Mode().Perm() != 0o640 {
		t.Fatalf("replaced file mode %v, want 0640", info.Mode().Perm())
	}
	link := filepath.Join(dir, "link.bbt")
	if err := os.Symlink("kept.bbt", link); err != nil {
		t.Fatal(err)
	}
	if err := must(NewFileWriter(fileEntries(5))).WriteFile(link); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced", err)
	}
	m, err := ReadFile[uint64, uint64](kept)
	if err != nil || m.Len() != 5 {
		t.Fatal("the link's target was not replaced", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name()[0] == '.' {
			t.Fatal("left a temporary file:", e.Name())
		}
	}
}

// WriteAt writes into a file opened to append, which a write at an offset
// can't, as upstream's seek and write do.
func TestWriteAtAppendingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "append.bin")
	if err := os.WriteFile(path, []byte("prefix"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := must(NewFileWriter(fileEntries(300)))
	r, err := w.WriteAt(f, 6)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.HasPrefix(data, []byte("prefix")) || int64(len(data)) != r.Offset+r.Length {
		t.Fatalf("file of %d bytes for range %+v", len(data), r)
	}
	m, err := ReadFileRange[uint64, uint64](f, r)
	if err != nil || m.Len() != 300 {
		t.Fatal(m, err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
