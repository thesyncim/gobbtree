//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package gobbtree

import (
	"fmt"
	"os"
	"syscall"
)

func mapFileReadOnly(file *os.File, offset int64, length int) ([]byte, func() error, error) {
	base := offset &^ int64(os.Getpagesize()-1)
	delta := int(offset - base)
	if length > int(^uint(0)>>1)-delta {
		return nil, nil, fmt.Errorf("%w: mapping length", ErrFileFormat)
	}
	data, err := syscall.Mmap(int(file.Fd()), base, length+delta, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return data[delta:], func() error { return syscall.Munmap(data) }, nil
}
