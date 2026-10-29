package gobbtree

// walk visits entries in b in key order, or in reverse, a leaf at a time:
// each leaf's entries are copied with the leaf latched shared and visited
// once it is let go, as latchedTree.walk, keeping range_cloned's contract.
// The walk goes on from the leaf's fence, the separator beside the edge its
// descent took at the lowest level that has one.
func (t *genericTree[K, V, O]) walk(b *Bounds[K], reverse bool, visit func(K, V) bool) {
	if t.root.Load() == nil {
		return
	}
	var keys [nodeSize]K
	var values [nodeSize]V
	var path gpath[K]
	if !reverse {
		seek, from, fenced := seekFirst, b.lower, false
		if b.lowerKind != unbounded {
			seek = seekKey
		}
		for {
			var code uint64
			if seek == seekKey {
				code = t.code(from)
			}
			n, _ := t.descend(from, code, seek, false, shared, &path)
			count, done := 0, false
			for i := 0; i < int(n.count.Load()); i++ {
				k := t.key(n, i)
				if fenced && t.compare(k, from) < 0 || !fenced && !aboveLower[K, O](b, k) {
					continue
				}
				if !belowUpper[K, O](b, k) {
					done = true
					break
				}
				keys[count], values[count] = k, n.values[i]
				count++
			}
			n.latch.unlockShared()
			for i := 0; i < count; i++ {
				if !visit(keys[i], values[i]) {
					return
				}
			}
			next := path.highFence()
			if done || next == nil {
				return
			}
			seek, from, fenced = seekKey, *next, true
		}
	}
	seek, below, strict, fenced := seekLast, b.upper, b.upperKind == excluded, false
	if b.upperKind != unbounded {
		seek = seekKey
	}
	for {
		var code uint64
		if seek == seekKey {
			code = t.code(below)
		}
		n, _ := t.descend(below, code, seek, strict, shared, &path)
		count, done := 0, false
		for i := int(n.count.Load()) - 1; i >= 0; i-- {
			k := t.key(n, i)
			if fenced && t.compare(k, below) >= 0 || !fenced && !belowUpper[K, O](b, k) {
				continue
			}
			if !aboveLower[K, O](b, k) {
				done = true
				break
			}
			keys[count], values[count] = k, n.values[i]
			count++
		}
		n.latch.unlockShared()
		for i := 0; i < count; i++ {
			if !visit(keys[i], values[i]) {
				return
			}
		}
		next := path.lowFence()
		if done || next == nil {
			return
		}
		seek, below, strict, fenced = seekKey, *next, true, true
	}
}
