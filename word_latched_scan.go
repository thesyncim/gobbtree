package gobbtree

// walk visits entries in key order, or in reverse, a leaf at a time: at
// least lower and below upper when bounded. Each leaf's entries are copied
// with the leaf latched shared, and visited once it is let go: as upstream's
// range_cloned, visit holds no latch, a writer waits at most for one leaf's
// copy, and the walk is not a snapshot. An entry present throughout is
// visited once. The walk goes on from the leaf's fence, the separator beside
// the edge its descent took at the lowest level that has one.
func (t *latchedTree) walk(lower, upper uint64, reverse, bounded bool, visit func(uint64, uint64) bool) {
	if bounded && lower >= upper || t.root.Load() == 0 {
		return
	}
	var buf [nodeSize]wordEntry
	var path treePath
	if !reverse {
		for from := lower; ; {
			leaf, _, _ := t.descend(from, shared, &path)
			count, done := 0, false
			for i := leaf.lowerEntry(from); i < int(leaf.n); i++ {
				entry := leaf.entries()[i]
				if bounded && entry.key >= upper {
					done = true
					break
				}
				buf[count] = entry
				count++
			}
			leaf.unlockShared()
			for i := 0; i < count; i++ {
				if !visit(buf[i].key, buf[i].value) {
					return
				}
			}
			next, ok := path.highFence()
			if done || !ok || bounded && next >= upper {
				return
			}
			from = next
		}
	}
	// below is the exclusive upper end still to visit; all says there is none.
	below, all := upper, !bounded
	for {
		key := ^uint64(0)
		if !all {
			if below == 0 {
				return
			}
			key = below - 1
		}
		leaf, _, _ := t.descend(key, shared, &path)
		end := int(leaf.n)
		if !all {
			end = leaf.lowerEntry(below)
		}
		count, done := 0, false
		for i := min(end, int(leaf.n)) - 1; i >= 0; i-- {
			entry := leaf.entries()[i]
			if bounded && entry.key < lower {
				done = true
				break
			}
			buf[count] = entry
			count++
		}
		leaf.unlockShared()
		for i := 0; i < count; i++ {
			if !visit(buf[i].key, buf[i].value) {
				return
			}
		}
		next, ok := path.lowFence()
		if done || !ok || bounded && next <= lower {
			return
		}
		below, all = next, false
	}
}

// highFence is the lowest key past the leaf the path leads to, or false for
// the last leaf.
func (p *treePath) highFence() (uint64, bool) {
	for level := p.depth - 1; level >= 0; level-- {
		if l := &p.levels[level]; l.edge+1 < l.count {
			return l.high, true
		}
	}
	return 0, false
}

// lowFence is the lowest key of the leaf the path leads to, or false for the
// first leaf.
func (p *treePath) lowFence() (uint64, bool) {
	for level := p.depth - 1; level >= 0; level-- {
		if l := &p.levels[level]; l.edge > 0 {
			return l.low, true
		}
	}
	return 0, false
}
