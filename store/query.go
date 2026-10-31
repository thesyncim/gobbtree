//go:build !storepoison

package store

import "unsafe"

// pointQuery holds a point lookup's query key in its caller's frame, as
// upstream's QueryKey lies on its caller's stack. Escape analysis can't
// follow a key through the map's generic calls, so it would move every
// query to the heap, which cost a lookup at 32K entries a third of its
// cycles, the collector's included; key hides the query's address from
// it.
//
// That is sound only because the map keeps no key it is given to find: it
// compares it and lets it go, and stores only the keys vacant returns,
// which point to their records. The storepoison build tag checks it: its
// queries are the heap's, poisoned once released, and a read of one
// panics.
type pointQuery struct{ q queryKey }

// key is the store key of the query for key, valid until release.
func (p *pointQuery) key(key []byte) storeKey {
	p.q = queryKey{marker: queryMarker, key: key}
	return storeKey{noescape(unsafe.Pointer(&p.q))}
}

// release ends the query: the map has let its key go.
func (p *pointQuery) release() {}

// poisonedQueries is whether released queries are poisoned (storepoison).
const poisonedQueries = false
