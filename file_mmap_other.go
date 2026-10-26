//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package gobbtree

import (
	"fmt"
	"os"
)

func mapFileReadOnly(_ *os.File, _ int64, _ int) ([]byte, func() error, error) {
	return nil, nil, fmt.Errorf("gobbtree: mmap is unavailable on this platform; use ReadFile")
}
