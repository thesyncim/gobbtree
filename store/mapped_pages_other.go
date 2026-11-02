//go:build !unix

package store

import (
	"sync/atomic"
	"unsafe"
)

// pageArena maps no pages where the system has no mmap: a store whose
// config asks for MappedPages takes its pages from Go's heap.
type pageArena struct{}

var handedAgain atomic.Int64

func newPageArena(int) *pageArena { return nil }

func (*pageArena) take() unsafe.Pointer { return nil }
func (*pageArena) give(unsafe.Pointer)  {}
func (*pageArena) release()             {}
