package gobbtree

import "cmp"

const batchLanes = 8

// Interleave independent routing steps so the CPU can overlap child loads.
// The caller owns the Tree and its scratch arrays.
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

func descendPublished[K cmp.Ordered, V any](root *searchImage[K, V], keys []K, nodes *[batchLanes]*searchImage[K, V], codes *[batchLanes]uint64) bool {
	if root == nil {
		return true
	}
	for j, key := range keys {
		nodes[j] = root
		if !root.strings {
			codes[j] = numericCode(key)
		}
	}
	for nodes[0].readEdges != nil {
		for j, key := range keys {
			n := nodes[j]
			if n.readEdges == nil {
				return false
			}
			index := 0
			code := codes[j]
			if n.strings {
				index = n.childIndex(key)
			} else if n.directory != nil {
				index = directoryRoute(&n.codes, n.directory, n.n, code)
			} else if n.compact {
				if code == ^uint64(0) {
					index = n.n - 1
				} else {
					index = max(0, lowerCompact(&n.codes, &n.highCodes, n.n, code+1)-1)
				}
			} else {
				index = routeCode(&n.codes, n.n, code)
			}
			nodes[j] = n.readEdges[index].Load()
			if nodes[j] == nil {
				return false
			}
		}
	}
	return true
}
