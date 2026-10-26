package gobbtree

import (
	"cmp"
	"iter"
)

// Entry is a key/value pair used by batch operations and snapshots.
type Entry[K cmp.Ordered, V any] struct {
	Key   K
	Value V
}

// Result is a value and a presence flag. A present zero value has OK=true.
type Result[V any] struct {
	Value V
	OK    bool
}

// Tree is an ordered map for a single owner. Its zero value is ready to use.
// Concurrent reads are safe only when no goroutine modifies the tree.
type Tree[K cmp.Ordered, V any] struct {
	root        *node[K, V]
	words       *wordTree
	arena       *nodeArena
	length      int
	shared      bool
	routesDirty bool
}

// NewTree creates an empty tree without synchronization.
func NewTree[K cmp.Ordered, V any]() *Tree[K, V] { return new(Tree[K, V]) }
func (t *Tree[K, V]) Len() int                   { return t.length }
func (t *Tree[K, V]) IsEmpty() bool              { return t.length == 0 }
func (t *Tree[K, V]) Clear() {
	t.root = nil
	t.words = nil
	t.arena = nil
	t.length = 0
	t.routesDirty = false
}

// Get returns a copy of the value associated with key.
func (t *Tree[K, V]) Get(key K) (V, bool) {
	if words := t.words; words != nil {
		code := numericCode(key)
		n := words.rootNode
		if id := words.routes.lookup(code); id != 0 {
			n = words.node(id)
		}
		for n != nil {
			if n.leaf {
				i := firstTag(&n.tags, int(n.n), code)
				if i < int(n.n) && n.entries()[i].key != code {
					i = n.lowerEntry(code)
				}
				if i < int(n.n) && n.entries()[i].key == code {
					value := n.entries()[i].value
					if words.valueKind == 1 {
						return any(value).(V), true
					}
					return unpackWord[V](value, words.valueKind), true
				}
				break
			}
			index := 0
			if n.directory != 0 {
				index = directoryRoute(&n.keys, &words.directories[n.directory-1], int(n.n), code)
			} else {
				projection := code >> n.shift
				prefix := (*n.prefixWord() >> n.shift) &^ uint64(65535)
				if projection > prefix|65535 {
					index = int(n.n) - 1
				} else if projection >= prefix {
					index = max(0, upperWordInline(&n.inner().inline, int(n.n), uint16(projection))-1)
					if index > 0 && n.inner().inline[index] == uint16(projection) && n.keys[index] > code {
						index = max(0, lowerCodeScalar(&n.keys, index+1, code+1)-1)
					}
				}
			}
			n = words.node(n.inner().children[index])
		}
		var zero V
		return zero, false
	}

	if t.root != nil {
		if !t.root.strings {
			code := numericCode(key)
			n := t.root
			for n.children != nil {
				index := 0
				if n.directory != nil {
					index = directoryRoute(&n.codes, n.directory, n.n, code)
				} else if n.compact {
					index = routeCompact(&n.codes, &n.highCodes, n.n, code)
				} else {
					index = routeCode(&n.codes, n.n, code)
				}
				n = n.children[index]
			}
			i := findCode(&n.codes, &n.tags, n.n, code)
			if i < n.n && n.codes[i] == code {
				return n.leaf.values[i], true
			}
			var zero V
			return zero, false
		}
		n := t.root.findLeaf(key)
		i := n.lower(key)
		if i < n.n && cmp.Compare(n.keys[i], key) == 0 {
			return n.leaf.values[i], true
		}
	}
	var zero V
	return zero, false
}
func (t *Tree[K, V]) Contains(key K) bool { _, ok := t.Get(key); return ok }

// Insert stores value, returning the previous value and whether it replaced
// an entry. Equal keys keep the original stored key, as in upstream bbtree.
func (t *Tree[K, V]) Insert(key K, value V) (V, bool) {
	if words := t.words; words != nil {
		code, packed := numericCode(key), packWord(value)
		if leaf := words.deletedLeaf; leaf != nil {
			words.deletedLeaf = nil
			if code == words.deletedKey {
				words.insertLeaf(leaf, words.deletedIndex, code, packed)
				t.length++
				var zero V
				return zero, false
			}
		}
		n := words.rootNode
		if n == nil {
			words.insert(code, packed)
		} else {
			if id := words.routes.lookup(code); id != 0 {
				n = words.node(id)
			}
			for !n.leaf {
				index := 0
				if n.directory != 0 {
					index = directoryRoute(&n.keys, &words.directories[n.directory-1], int(n.n), code)
				} else {
					projection := code >> n.shift
					prefix := (*n.prefixWord() >> n.shift) &^ uint64(65535)
					if projection > prefix|65535 {
						index = int(n.n) - 1
					} else if projection >= prefix {
						index = max(0, upperWordInline(&n.inner().inline, int(n.n), uint16(projection))-1)
						if index > 0 && n.inner().inline[index] == uint16(projection) && n.keys[index] > code {
							index = max(0, lowerCodeScalar(&n.keys, index+1, code+1)-1)
						}
					}
				}
				n = words.node(n.inner().children[index])
			}
			i := firstTag(&n.tags, int(n.n), code)
			if i < int(n.n) && n.entries()[i].key != code {
				i = n.lowerEntry(code)
			}
			if i < int(n.n) && n.entries()[i].key == code {
				old := n.entries()[i].value
				n.entries()[i].value = packed
				if words.valueKind == 1 {
					return any(old).(V), true
				}
				return unpackWord[V](old, words.valueKind), true
			}
			if count := int(n.n); count < nodeSize {
				if i == count && count > 0 && code < n.entries()[count-1].key {
					i = n.lowerEntry(code)
				}
				if i > 0 {
					n.insertLeafPlain(i, code, packed)
					t.length++
					var zero V
					return zero, false
				}
			}
			words.insertNew(n, i, code, packed)
		}
		t.length++
		var zero V
		return zero, false
	}
	return t.insertGeneric(key, value)
}

func (t *Tree[K, V]) insertGeneric(key K, value V) (V, bool) {
	if t.root == nil && t.initWords() {
		return t.Insert(key, value)
	}
	if t.root == nil {
		t.arena = new(nodeArena)
		t.root = newLeaf[K, V](t.shared, t.arena)
		t.routesDirty = true
	}
	// Replacements should not split or allocate, even in a full leaf.
	n := t.root
	full := true
	if n.strings {
		n = n.findLeaf(key)
	} else {
		n, full = n.findLeafForInsert(numericCode(key))
	}
	i := n.exact(key)
	if i < n.n && cmp.Compare(n.keys[i], key) == 0 {
		old := n.leaf.values[i]
		n.setValue(i, value)
		return old, true
	}
	if !full && n.n < nodeSize {
		if i == n.n && n.n > 0 && cmp.Compare(key, n.keys[n.n-1]) < 0 {
			i = n.lower(key)
		}
		if i > 0 {
			n.insertLeaf(i, key, value)
			t.length++
			var zero V
			return zero, false
		}
	}
	if t.root.n == nodeSize {
		right := t.root.split()
		root := newInner[K, V](t.shared, t.arena)
		root.insertChild(0, t.root)
		root.insertChild(1, right)
		t.root = root
		t.routesDirty = true
	}
	t.insertAbsent(t.root, key, value)
	t.length++
	var zero V
	return zero, false
}
func (t *Tree[K, V]) insertAbsent(n *node[K, V], key K, value V) {
	if n.leaf != nil {
		n.insertLeaf(n.lower(key), key, value)
		return
	}
	i := n.childIndex(key)
	if n.children[i].n == nodeSize {
		right := n.children[i].split()
		n.insertChild(i+1, right)
		t.changedRoutes(n)
		if cmp.Compare(key, right.keys[0]) >= 0 {
			i++
		}
	}
	t.insertAbsent(n.children[i], key, value)
	if cmp.Compare(n.keys[i], n.children[i].keys[0]) != 0 {
		n.keys[i] = n.children[i].keys[0]
		n.refit()
		t.changedRoutes(n)
	}
}

// Delete removes an entry, returning its previous value and presence.
func (t *Tree[K, V]) Delete(key K) (V, bool) {
	if t.words != nil {
		old, ok := t.words.deleteOwner(numericCode(key))
		if ok {
			t.length--
			return t.wordValue(old), true
		}
		var zero V
		return zero, false
	}
	if t.root == nil {
		var v V
		return v, false
	}
	old, ok := t.remove(t.root, key)
	if !ok {
		return old, false
	}
	t.length--
	if t.root.n == 0 {
		t.root = nil
		t.arena = nil
		t.routesDirty = true
	} else if t.root.children != nil && t.root.n == 1 {
		oldRoot := t.root
		t.root = t.root.children[0]
		oldRoot.release()
		t.routesDirty = true
	}
	return old, true
}
func (t *Tree[K, V]) remove(n *node[K, V], key K) (V, bool) {
	if n.leaf != nil {
		i := n.exact(key)
		if i == n.n || cmp.Compare(n.keys[i], key) != 0 {
			var v V
			return v, false
		}
		return n.deleteLeaf(i), true
	}
	i := n.childIndex(key)
	child := n.children[i]
	old, ok := t.remove(child, key)
	if !ok {
		return old, false
	}
	if child.n < minNodeSize {
		n.rebalance(i)
		// Rebalancing can change both neighboring minima and child positions.
		for j := 0; j < n.n; j++ {
			n.keys[j] = n.children[j].keys[0]
		}
		n.refit()
		t.changedRoutes(n)
	} else if cmp.Compare(n.keys[i], child.keys[0]) != 0 {
		n.keys[i] = child.keys[0]
		n.refit()
		t.changedRoutes(n)
	}
	return old, true
}

func (n *node[K, V]) rebalance(i int) {
	child := n.children[i]
	if i > 0 && n.children[i-1].n > minNodeSize {
		left := n.children[i-1]
		copy(child.keys[1:child.n+1], child.keys[:child.n])
		child.keys[0] = left.keys[left.n-1]
		if child.leaf != nil {
			copy(child.leaf.values[1:child.n+1], child.leaf.values[:child.n])
			child.leaf.values[0] = left.leaf.values[left.n-1]
			var v V
			left.leaf.values[left.n-1] = v
		} else {
			copy(child.children[1:child.n+1], child.children[:child.n])
			child.children[0] = left.children[left.n-1]
			left.children[left.n-1] = nil
		}
		var k K
		left.keys[left.n-1] = k
		left.n--
		child.n++
		left.refit()
		child.refit()
		return
	}
	if i+1 < n.n && n.children[i+1].n > minNodeSize {
		right := n.children[i+1]
		child.keys[child.n] = right.keys[0]
		if child.leaf != nil {
			child.leaf.values[child.n] = right.leaf.values[0]
			copy(right.leaf.values[:right.n-1], right.leaf.values[1:right.n])
			var v V
			right.leaf.values[right.n-1] = v
		} else {
			child.children[child.n] = right.children[0]
			copy(right.children[:right.n-1], right.children[1:right.n])
			right.children[right.n-1] = nil
		}
		copy(right.keys[:right.n-1], right.keys[1:right.n])
		var k K
		right.keys[right.n-1] = k
		right.n--
		child.n++
		right.refit()
		child.refit()
		return
	}
	if i > 0 {
		i--
	}
	left, right := n.children[i], n.children[i+1]
	copy(left.keys[left.n:], right.keys[:right.n])
	if left.leaf != nil {
		copy(left.leaf.values[left.n:], right.leaf.values[:right.n])
		left.leaf.next = right.leaf.next
		if left.leaf.next != nil {
			left.leaf.next.leaf.prev = left
		}
	} else {
		copy(left.children[left.n:], right.children[:right.n])
	}
	left.n += right.n
	left.refit()
	n.removeChild(i + 1)
	right.release()
}

func (t *Tree[K, V]) firstLeaf() *node[K, V] {
	n := t.root
	if n == nil {
		return nil
	}
	for n.children != nil {
		n = n.children[0]
	}
	return n
}
func (t *Tree[K, V]) lastLeaf() *node[K, V] {
	n := t.root
	if n == nil {
		return nil
	}
	for n.children != nil {
		n = n.children[n.n-1]
	}
	return n
}
func (t *Tree[K, V]) First() (K, V, bool) {
	if t.words != nil {
		if id := t.words.end(false); id != 0 {
			n := t.words.node(id)
			i := 0

			return decodeWordKey[K](n.entries()[i].key, t.words.keyKind), t.wordValue(n.entries()[i].value), true
		}
		var k K
		var v V
		return k, v, false
	}
	if n := t.firstLeaf(); n != nil {
		return n.keys[0], n.leaf.values[0], true
	}
	var k K
	var v V
	return k, v, false
}
func (t *Tree[K, V]) Last() (K, V, bool) {
	if t.words != nil {
		if id := t.words.end(true); id != 0 {
			n := t.words.node(id)
			i := 0
			i = int(n.n) - 1
			return decodeWordKey[K](n.entries()[i].key, t.words.keyKind), t.wordValue(n.entries()[i].value), true
		}
		var k K
		var v V
		return k, v, false
	}
	if n := t.lastLeaf(); n != nil {
		return n.keys[n.n-1], n.leaf.values[n.n-1], true
	}
	var k K
	var v V
	return k, v, false
}
func (t *Tree[K, V]) PopFirst() (K, V, bool) {
	k, v, ok := t.First()
	if ok {
		t.Delete(k)
	}
	return k, v, ok
}
func (t *Tree[K, V]) PopLast() (K, V, bool) {
	k, v, ok := t.Last()
	if ok {
		t.Delete(k)
	}
	return k, v, ok
}

// Scan visits keys in [lower, upper), stopping when visit returns false.
// Neither visit nor another goroutine may mutate the tree during the scan.
func (t *Tree[K, V]) Scan(lower, upper K, visit func(K, V) bool) {
	if t.words != nil {
		t.words.scan(numericCode(lower), numericCode(upper), false, func(k, v uint64) bool { return visit(decodeWordKey[K](k, t.words.keyKind), t.wordValue(v)) })
		return
	}

	if t.root == nil || cmp.Compare(lower, upper) >= 0 {
		return
	}
	n := t.root.findLeaf(lower)
	i := n.lower(lower)
	for ; n != nil; n = n.leaf.next {
		for ; i < n.n; i++ {
			if cmp.Compare(n.keys[i], upper) >= 0 || !visit(n.keys[i], n.leaf.values[i]) {
				return
			}
		}
		i = 0
	}
}

// ScanReverse visits the same half-open interval as Scan, in descending order.
func (t *Tree[K, V]) ScanReverse(lower, upper K, visit func(K, V) bool) {
	if t.words != nil {
		t.words.scan(numericCode(lower), numericCode(upper), true, func(k, v uint64) bool { return visit(decodeWordKey[K](k, t.words.keyKind), t.wordValue(v)) })
		return
	}

	if t.root == nil || cmp.Compare(lower, upper) >= 0 {
		return
	}
	n := t.root.findLeaf(upper)
	i := n.lower(upper) - 1
	for ; n != nil; n = n.leaf.prev {
		for ; i >= 0; i-- {
			if cmp.Compare(n.keys[i], lower) < 0 || !visit(n.keys[i], n.leaf.values[i]) {
				return
			}
		}
		if n.leaf.prev != nil {
			i = n.leaf.prev.n - 1
		}
	}
}

// All iterates all entries in ascending order. Mutation during iteration is forbidden.
func (t *Tree[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if t.words != nil {
			t.words.all(false, func(k, v uint64) bool { return yield(decodeWordKey[K](k, t.words.keyKind), t.wordValue(v)) })
			return
		}

		for n := t.firstLeaf(); n != nil; n = n.leaf.next {
			for i := 0; i < n.n; i++ {
				if !yield(n.keys[i], n.leaf.values[i]) {
					return
				}
			}
		}
	}
}

// Reverse iterates all entries in descending order.
func (t *Tree[K, V]) Reverse() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if t.words != nil {
			t.words.all(true, func(k, v uint64) bool { return yield(decodeWordKey[K](k, t.words.keyKind), t.wordValue(v)) })
			return
		}

		for n := t.lastLeaf(); n != nil; n = n.leaf.prev {
			for i := n.n - 1; i >= 0; i-- {
				if !yield(n.keys[i], n.leaf.values[i]) {
					return
				}
			}
		}
	}
}

// GetMany fills dst in query order, including repeated keys and misses.
// It panics before writing if dst is shorter than keys. No allocation is needed.
func (t *Tree[K, V]) GetMany(keys []K, dst []Result[V]) {
	if len(dst) < len(keys) {
		panic("gobbtree: GetMany destination too short")
	}
	if t.words != nil {
		if t.words.keyKind == 1 && t.words.valueKind == 1 {
			t.words.getMany(any(keys).([]uint64), any(dst).([]Result[uint64]))
			return
		}
		for start := 0; start < len(keys); start += batchLanes {
			count := min(batchLanes, len(keys)-start)
			var codes [batchLanes]uint64
			var values [batchLanes]Result[uint64]
			for i := 0; i < count; i++ {
				codes[i] = numericCode(keys[start+i])
			}
			t.words.getMany(codes[:count], values[:count])
			for i := 0; i < count; i++ {
				var value V
				if values[i].OK {
					value = t.wordValue(values[i].Value)
				}
				dst[start+i] = Result[V]{value, values[i].OK}
			}
		}
		return
	}

	for start := 0; start < len(keys); start += batchLanes {
		count := min(batchLanes, len(keys)-start)
		var nodes [batchLanes]*node[K, V]
		var codes [batchLanes]uint64
		descendBatch(t.root, keys[start:start+count], &nodes, &codes)
		for j := 0; j < count; j++ {
			var result Result[V]
			if n := nodes[j]; n != nil {
				i := 0
				if !n.strings {
					i = findCode(&n.codes, &n.tags, n.n, codes[j])
				} else {
					i = n.lower(keys[start+j])
				}
				if i < n.n && ((!n.strings && n.codes[i] == codes[j]) || (n.strings && cmp.Compare(n.keys[i], keys[start+j]) == 0)) {
					result = Result[V]{n.leaf.values[i], true}
				}
			}
			dst[start+j] = result
		}
	}
}

// InsertMany inserts in input order and fills dst with previous values.
func (t *Tree[K, V]) InsertMany(entries []Entry[K, V], dst []Result[V]) {
	if len(dst) < len(entries) {
		panic("gobbtree: InsertMany destination too short")
	}
	for i, e := range entries {
		dst[i].Value, dst[i].OK = t.Insert(e.Key, e.Value)
	}
}

// Clone makes an independent tree; values and keys are shallow copies.
func (t *Tree[K, V]) Clone() *Tree[K, V] {
	out := NewTree[K, V]()
	for k, v := range t.All() {
		out.Insert(k, v)
	}
	return out
}
