//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package gobbtree

import (
	"fmt"
	"os"
	"syscall"
)

func mapFileReadOnly(file *os.File, offset int64, length int, pages OsPages) ([]byte, func() error, error) {
	base := offset &^ int64(os.Getpagesize()-1)
	delta := int(offset - base)
	if length > int(^uint(0)>>1)-delta {
		return nil, nil, fmt.Errorf("%w: mapping length", ErrFileFormat)
	}
	data, err := syscall.Mmap(int(file.Fd()), base, length+delta, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	if pages == OsPagesHuge {
		// Advice on the page-aligned mapping; when not taken, pages stay small.
		adviseHugePages(data)
	}
	return data[delta:], func() error { return syscall.Munmap(data) }, nil
}
