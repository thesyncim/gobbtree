//go:build race || !(amd64 || (arm64 && (darwin || ios || arm64.v8.1)))

package gobbtree

// Elsewhere, or under the race detector, writers store shared numeric fields
// atomically. See seqlock_plain.go.
const seqlockPlainStores = false
