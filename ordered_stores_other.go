//go:build !amd64 || race

package gobbtree

// Elsewhere, or under the race detector, exclusive writers keep atomic
// operations. See ordered_stores_amd64.go.
const orderedStores = false
