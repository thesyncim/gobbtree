//go:build unix

package gobbtree

import (
	"errors"
	"os"
	"syscall"
)

// syncDir makes a rename in dir durable. A file system that can't sync
// directories (EINVAL) is taken as done.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if errors.Is(err, syscall.EINVAL) {
		err = nil
	}
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	return err
}
