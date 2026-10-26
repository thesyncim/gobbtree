//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package gobbtree

import "os"

func mapFileReadOnly(_ *os.File, _ int64, _ int) ([]byte, func() error, error) {
	return nil, nil, ErrMmapUnavailable
}
