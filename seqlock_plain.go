//go:build !race && (amd64 || (arm64 && (darwin || ios || arm64.v8.1)))

package gobbtree

// Numeric writers bracket every change that optimistic readers can observe
// with sequence or epoch increments, which readers validate after atomic
// loads. On these targets that increment is a full barrier (amd64 LOCK XADD;
// arm64 LDADDAL, which every Apple core and every ARMv8.1 core provides), so
// the stores between the increments need no ordering of their own: a reader
// that observes any of them also observes the odd epoch and retries. Aligned
// word stores cannot tear, and torn multiword states are only ever discarded.
// The race detector cannot see this protocol, so race builds keep atomic stores.
const seqlockPlainStores = true
