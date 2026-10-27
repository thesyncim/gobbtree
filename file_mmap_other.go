//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package gobbtree

import "os"

func mapFileReadOnly(*os.File, int64, int, OsPages) ([]byte, func() error, error) {
	return nil, nil, ErrMmapUnavailable
}
