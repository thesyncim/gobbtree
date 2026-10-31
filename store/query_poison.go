//go:build storepoison

package store

import "unsafe"

// pointQuery, under storepoison, holds its query in the heap, where a key
// the map kept would stay valid, and poisons it when released: a read of
// the key then panics, where a frame's query would be read stale.
type pointQuery struct{ q *queryKey }

func (p *pointQuery) key(key []byte) storeKey {
	p.q = &queryKey{marker: queryMarker, key: key}
	return storeKey{unsafe.Pointer(p.q)}
}

func (p *pointQuery) release() { p.q.key = poisonedKey[:] }

const poisonedQueries = true
