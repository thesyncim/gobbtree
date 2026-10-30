package gobbtree

// walk visits entries in b in key order, or in reverse, a leaf at a time:
// each leaf's entries are copied with the leaf latched shared and visited
// once it is let go, keeping upstream's range_cloned contract.
// The walk goes on from the leaf's fence, read while the leaf is held, which
// keeps it: going back, its own head; going on, the next leaf's, through
// the path, or, where a racing writer changed the node it reads it through,
// from the leaf's last key.
func (t *genericTree[K, V, O]) walk(b *Bounds[K], reverse bool, visit func(K, V) bool) {
	if t.root.Load() == nil {
		return
	}
	var keys [nodeSize]K
	var values [nodeSize]V
	var path gpath[K]
	if !reverse {
		// fenced skips keys below from, past also from itself.
		seek, from, fenced, past := seekFirst, b.lower, false, false
		if b.lowerKind != unbounded {
			seek = seekKey
		}
		for {
			var code uint64
			if seek == seekKey {
				code = t.code(from)
			}
			n, _ := t.descend(from, code, seek, false, shared, &path)
			count, done, held := 0, false, int(n.count.Load())
			for i := 0; i < held; i++ {
				// A walk resumed from a leaf's last key may meet keys
				// below the bounds: every key is checked against them.
				k := t.key(n, i)
				if !aboveLower[K, O](b, k) {
					continue
				}
				if fenced {
					if order := t.compare(k, from); order < 0 || past && order == 0 {
						continue
					}
				}
				if !belowUpper[K, O](b, k) {
					done = true
					break
				}
				keys[count], values[count] = k, n.values[i]
				count++
			}
			var next, last K
			has, ok, kept := false, true, false
			if !done {
				if next, has, ok = t.nextFence(&path); !ok && held > 0 {
					last, kept = t.key(n, held-1), true
				}
			}
			n.latch.unlockShared()
			for i := 0; i < count; i++ {
				if !visit(keys[i], values[i]) {
					return
				}
			}
			switch {
			case done || ok && !has || !ok && !kept:
				return
			case !ok:
				seek, from, fenced, past = seekKey, last, true, true
			default:
				seek, from, fenced, past = seekKey, next, true, false
			}
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
			if !belowUpper[K, O](b, k) || fenced && t.compare(k, below) >= 0 {
				continue
			}
			if !aboveLower[K, O](b, k) {
				done = true
				break
			}
			keys[count], values[count] = k, n.values[i]
			count++
		}
		low, more := t.walkLow(n, &path)
		n.latch.unlockShared()
		for i := 0; i < count; i++ {
			if !visit(keys[i], values[i]) {
				return
			}
		}
		if done || !more {
			return
		}
		seek, below, strict, fenced = seekKey, low, true, true
	}
}

// walkLow is the low fence of leaf n, held, which path leads to: its first
// key, its head where it keeps one, and none for the leftmost leaf, which
// no path turns right to. Keys their codes hold whole take their
// separator's code, which may lie below the first key. The leaf, held,
// keeps the separator's code, but not where the path found it: a writer
// splitting or merging a node above moves it, and the node's slot then
// holds another's. Where a node of the path from the separator's down
// changed since the descent, the walk goes on from the first key, as a
// forward walk does from its last.
func (t *genericTree[K, V, O]) walkLow(n *gleaf[K, V], path *gpath[K]) (K, bool) {
	var k K
	level := path.separatorLevel()
	if level < 0 {
		return k, false
	}
	if t.wholeCodes() {
		l := &path.levels[level]
		k = t.decode(l.n.codes[l.edge&(nodeSize-1)].Load())
		if t.owner || path.unchanged(level) {
			return k, true
		}
	}
	if n.count.Load() == 0 {
		return k, false
	}
	return t.firstKey(n), true
}
