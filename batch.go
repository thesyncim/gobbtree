package gobbtree

const batchLanes = 8

// Interleave independent routing steps so the CPU can overlap child loads.
// The caller owns the Tree and its scratch arrays.
func descendBatch[K any, V any, O keyOrder[K]](root *node[K, V, O], keys []K, nodes *[batchLanes]*node[K, V, O], codes *[batchLanes]uint64) {
	if root == nil {
		return
	}
	for j, key := range keys {
		nodes[j] = root
		if !root.strings {
			codes[j] = numericKey[O](key)
		}
	}
	for nodes[0].children != nil {
		for j, key := range keys {
			n := nodes[j]
			if !n.strings {
				index := 0
				code := codes[j]
				if n.directory != nil {
					index = directoryRoute(&n.codes, n.directory, n.n, code)
				} else if n.compact {
					index = routeCompact(&n.codes, &n.highCodes, n.n, code)
				} else {
					index = routeCode(&n.codes, n.n, code)
				}
				nodes[j] = n.children[index]
			} else {
				nodes[j] = n.children[n.childIndex(key)]
			}
		}
	}
}
