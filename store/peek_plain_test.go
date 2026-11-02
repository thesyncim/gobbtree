//go:build !race && (amd64 || (arm64 && (darwin || ios || arm64.v8.1)))

package store

// peeksSettle reports builds whose lookups read leaves racing, as the map's
// plain-store builds do (seqlock_plain.go).
const peeksSettle = true
