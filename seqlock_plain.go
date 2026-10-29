//go:build !race && (amd64 || (arm64 && (darwin || ios || arm64.v8.1)))

package gobbtree

// Writers change a node only while they hold its latch exclusively. Taking
// it is a compare-and-swap, a full barrier on these targets (amd64 LOCK
// CMPXCHG; arm64 CASAL, which every Apple core and every ARMv8.1 core
// provides), and letting go is a release store that moves its version, so
// the stores between need no ordering of their own: a reader that observes
// any of them fails its version check and retries. Aligned word stores
// cannot tear, and torn multiword states are only ever discarded. The race
// detector cannot see this protocol, so race builds keep atomic stores.
const seqlockPlainStores = true
