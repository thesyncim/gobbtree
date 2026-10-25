package gobbtree

import "cmp"

const batchLanes = 8

// Interleave independent routing steps so the CPU can overlap child loads.
// The caller owns the scratch, and for Map holds the structural read latch.
// No leaf contents are read here: Map must latch each returned leaf first.
func descendBatch[K cmp.Ordered, V any](root *node[K, V], keys []K, nodes *[batchLanes]*node[K, V], codes *[batchLanes]uint64) {
	if root == nil {
		return
	}
	for j, key := range keys {
		nodes[j] = root
		if !root.strings {
			codes[j] = numericCode(key)
		}
	}
	for nodes[0].children != nil {
		for j, key := range keys {
			n := nodes[j]
			if !n.strings {
				nodes[j] = n.children[routeCode(&n.codes, n.n, codes[j])]
			} else {
				nodes[j] = n.children[n.childIndex(key)]
			}
		}
	}
}
