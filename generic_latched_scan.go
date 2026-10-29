package gobbtree

// walk visits entries in b in key order, or in reverse, a leaf at a time:
// each leaf's entries are copied with the leaf latched shared and visited
// once it is let go, as latchedTree.walk, keeping range_cloned's contract.
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
		// The leaf's low fence: its first key, its head where it keeps one,
		// none for the leftmost leaf, which no path turns right to. Keys
		// their codes hold whole take their separator's code, which may
		// lie below the first key; the leaf, held, keeps it a fence.
		var low *K
		if level := path.separatorLevel(); level >= 0 && n.count.Load() > 0 {
			k := t.firstKey(n)
			if t.wholeCodes() {
				l := &path.levels[level]
				k = t.decode(l.n.codes[l.edge&(nodeSize-1)].Load())
			}
			low = &k
		}
		n.latch.unlockShared()
		for i := 0; i < count; i++ {
			if !visit(keys[i], values[i]) {
				return
			}
		}
		if done || low == nil {
			return
		}
		seek, below, strict, fenced = seekKey, *low, true, true
	}
}
