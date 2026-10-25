// Package gobbtree implements ordered maps using a B+ tree and dense inline
// key summaries, adapted from XiangpengHao/bbtree. Map is safe for concurrent
// use; Tree avoids synchronization for a single owner. Enable GOEXPERIMENT=simd
// with Go 1.27 to search summaries with Go's portable SIMD intrinsics.
//
// Keys use cmp.Compare order, including its total order for floating point
// NaNs and signed zero. Values are copied, not deep-cloned. References within
// keys and values remain the caller's responsibility. A map or tree must not
// be copied after first use. Iteration callbacks must not mutate the tree;
// callbacks on Map must not call back into the same map.
package gobbtree
