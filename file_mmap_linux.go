//go:build linux

package gobbtree

import "syscall"

// adviseHugePages asks for 2 MiB pages, as upstream's OsPages::Huge does.
func adviseHugePages(mapping []byte) { _ = syscall.Madvise(mapping, syscall.MADV_HUGEPAGE) }
