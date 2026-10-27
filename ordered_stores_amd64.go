//go:build amd64 && !race

package gobbtree

// amd64 keeps stores in program order and loads in program order, so a
// plain store already releases, and a seqlock writer's opening store already
// precedes its data stores. Go's atomic stores and read-modify-writes are
// locked instructions there, a full barrier each; exclusive writers use
// plain stores instead. The race detector cannot see this protocol, so race
// builds keep atomic operations.
const orderedStores = true
