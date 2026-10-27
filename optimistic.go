package gobbtree

import (
	"math"
	"runtime"
	"sync/atomic"
)

type atomicWords struct {
	sequence atomic.Uint64
	values   [nodeSize]atomic.Uint64
}

func (l *leaf[K, V, O]) sequenceValue() uint64 {
	if l.pairs != nil {
		return l.pairs.sequence.Load()
	}
	if l.words != nil {
		return l.words.sequence.Load()
	}
	return l.sequence.Load()
}
func (n *node[K, V, O]) advanceSequence() {
	l := n.leaf
	var sequence uint64
	if l.pairs != nil {
		sequence = l.pairs.sequence.Add(1)
	} else if l.words != nil {
		sequence = l.words.sequence.Add(1)
	} else {
		sequence = l.sequence.Add(1)
	}
	n.image.Load().valid.Store(sequence&1 == 0)
}

// SIMD reads immutable tags and routing columns. Paired leaf keys and values
// are atomic; image validity and the structural epoch validate each read.
func (n *node[K, V, O]) publish() { n.publishFrom(0) }
func (n *node[K, V, O]) publishFrom(first int) {
	if !n.shared {
		return
	}
	if n.leaf != nil && n.strings && n.leaf.wordKind == 0 {
		image := n.image.Load()
		if image == nil {
			image = &searchImage[K, V, O]{n: -1, strings: true, leaf: n.leaf, node: n}
			image.valid.Store(true)
			n.image.Store(image)
		}
		if n.parentSlot != nil {
			n.parentSlot.Store(image)
		}
		return
	}
	// No retired image may validate data in a recycled atomic block.
	if old := n.image.Load(); old != nil && old.valid.Load() {
		old.valid.Store(false)
	}
	image := &searchImage[K, V, O]{n: n.n, prefix: n.prefix, prefixHead: n.prefixHead, strings: n.strings, leaf: n.leaf, readEdges: n.readEdges}
	if n.leaf == nil || n.leaf.pairs == nil {
		image.codes = new([nodeSize]uint64)
		*image.codes = n.codes
	}
	image.compact = n.compact
	if n.children != nil && n.compact && n.directory == nil {
		image.highCodes = new([nodeSize]uint32)
		*image.highCodes = n.highCodes
	}
	if n.leaf != nil && (n.leaf.words != nil || n.leaf.pairs != nil) {
		image.leaf = nil
	}
	image.tags = n.tags
	if n.directory != nil {
		image.directory = new([256]uint8)
		*image.directory = *n.directory
	}
	if n.strings {
		image.keys = new([nodeSize]K)
		*image.keys = n.keys
	}
	if n.children != nil {
		for i := 0; i < n.n; i++ {
			child := n.children[i]
			child.parentSlot = &n.readEdges[i]
			n.readEdges[i].Store(child.image.Load())
		}
		for i := n.n; i < nodeSize; i++ {
			n.readEdges[i].Store(nil)
		}
	} else {
		sequence := n.leaf.sequenceValue()
		image.imageVersion = sequence + (sequence & 1)
		image.valid.Store(sequence&1 == 0)
		image.wordState = n.leaf.words
		image.pairs = n.leaf.pairs
		image.valueKind = n.leaf.wordKind
		if n.leaf.pairs != nil {
			for i := first; i < n.n; i++ {
				n.leaf.pairs.entries[i].key.Store(n.codes[i])
				n.leaf.pairs.entries[i].value.Store(packWord(n.leaf.values[i]))
			}
		} else if n.leaf.words != nil {
			switch n.leaf.wordKind {
			case 1:
				for i := first; i < n.n; i++ {
					n.leaf.words.values[i].Store(any(n.leaf.values[i]).(uint64))
				}
			case 2:
				for i := first; i < n.n; i++ {
					n.leaf.words.values[i].Store(uint64(any(n.leaf.values[i]).(int)))
				}
			default:
				for i := first; i < n.n; i++ {
					n.leaf.words.values[i].Store(packWord(n.leaf.values[i]))
				}
			}
		}
	}
	n.image.Store(image)
	if n.parentSlot != nil {
		n.parentSlot.Store(image)
	}
}
func (n *node[K, V, O]) setValue(i int, value V) {
	n.leaf.values[i] = value
	if n.leaf.pairs != nil {
		n.leaf.pairs.entries[i].value.Store(packWord(value))
	} else if n.leaf.words != nil {
		n.leaf.words.values[i].Store(packWord(value))
	}
}

func wordKind[V any]() uint8 {
	var zero V
	switch any(zero).(type) {
	case uint64:
		return 1
	case int:
		return 2
	case int64:
		return 3
	case uint:
		return 4
	case uint32:
		return 5
	case int32:
		return 6
	case uint16:
		return 7
	case int16:
		return 8
	case uint8:
		return 9
	case int8:
		return 10
	case float64:
		return 11
	case float32:
		return 12
	case bool:
		return 13
	case uintptr:
		return 14
	default:
		return 0
	}
}

// Keep the common conversion within the inlining budget.
func packWord[V any](value V) uint64 {
	if word, ok := any(value).(uint64); ok {
		return word
	}
	return packWordOther(value)
}
func packWordOther[V any](value V) uint64 {
	switch v := any(value).(type) {
	case uint64:
		return v
	case int:
		return uint64(v)
	case int64:
		return uint64(v)
	case uint:
		return uint64(v)
	case uint32:
		return uint64(v)
	case int32:
		return uint64(v)
	case uint16:
		return uint64(v)
	case int16:
		return uint64(v)
	case uint8:
		return uint64(v)
	case int8:
		return uint64(v)
	case float64:
		return math.Float64bits(v)
	case float32:
		return uint64(math.Float32bits(v))
	case bool:
		if v {
			return 1
		}
		return 0
	case uintptr:
		return uint64(v)
	default:
		panic("gobbtree: non-word value in atomic value path")
	}
}
func unpackWord[V any](word uint64, kind uint8) V {
	// Each assertion is guaranteed by the kind selected from V at construction.
	switch kind {
	case 1:
		return any(word).(V)
	case 2:
		return any(int(word)).(V)
	case 3:
		return any(int64(word)).(V)
	case 4:
		return any(uint(word)).(V)
	case 5:
		return any(uint32(word)).(V)
	case 6:
		return any(int32(word)).(V)
	case 7:
		return any(uint16(word)).(V)
	case 8:
		return any(int16(word)).(V)
	case 9:
		return any(uint8(word)).(V)
	case 10:
		return any(int8(word)).(V)
	case 11:
		return any(math.Float64frombits(word)).(V)
	case 12:
		return any(math.Float32frombits(uint32(word))).(V)
	case 13:
		return any(word != 0).(V)
	case 14:
		return any(uintptr(word)).(V)
	default:
		panic("gobbtree: invalid atomic value kind")
	}
}

// structuralStart/End run under structure.Lock. The epoch prevents readers
// from accepting a descent across a split/merge. Only structural writers
// change it; readers perform loads, not a globally contended reader increment.
// The three adjacent leaves cover every leaf a split or rebalance can change.
func (m *mapCore[K, V, O]) structuralStart(key K) [3]*node[K, V, O] {
	m.version.Add(1)
	m.tree.shared = true
	var held [3]*node[K, V, O]
	if m.tree.root != nil {
		target := m.tree.root.findLeaf(key)
		held = [3]*node[K, V, O]{target.leaf.prev, target, target.leaf.next}
		for _, n := range held {
			if n != nil {
				n.leaf.mu.Lock()
			}
		}
	}
	return held
}
func (m *mapCore[K, V, O]) structuralEnd(held [3]*node[K, V, O]) {
	if m.tree.root == nil {
		m.root.Store(nil)
	} else {
		m.tree.root.parentSlot = &m.root
		m.root.Store(m.tree.root.image.Load())
	}
	if m.tree.routesDirty {
		m.rebuildRoutes()
	}
	m.version.Add(1)
	for _, n := range held {
		if n != nil {
			n.leaf.mu.Unlock()
		}
	}
}

func (m *mapCore[K, V, O]) optimisticGet(key K) (V, bool) {
	var zero V
	search := keySearch[K, O]{key: key}
	code := search.numeric()
retry:
	for {
		version := m.version.Load()
		if version&1 != 0 {
			runtime.Gosched()
			continue
		}
		image := m.root.Load()
		if image == nil {
			if m.version.Load() == version {
				return zero, false
			}
			continue
		}
		if routes := m.routes.Load(); routes != nil {
			index := (code - routes.base) >> routes.shift
			if index < uint64(len(routes.slots)) {
				if slot := routes.slots[index]; slot != nil {
					image = slot.Load()
					if image == nil {
						continue retry
					}
				}
			}
		}
		// Keys are immutable; child links are atomics. A structural epoch
		// validates the entire route, including a concurrent move of an edge.
		for image.readEdges != nil {
			index := 0
			if image.strings {
				index = image.routeSearch(&search)
			} else if image.directory != nil {
				index = directoryRoute(image.codes, image.directory, image.n, code)
			} else if image.compact {
				if code == ^uint64(0) {
					index = image.n - 1
				} else {
					index = max(0, lowerCompact(image.codes, image.highCodes, image.n, code+1)-1)
				}
			} else {
				index = routeCode(image.codes, image.n, code)
			}
			image = image.readEdges[index].Load()
			if image == nil {
				continue retry
			}
		}
		var value V
		var found bool
		if n := image.node; n != nil {
			value, found, ok := n.latchedGet(&search, &m.version, version)
			if !ok {
				continue
			}
			return value, found
		}
		if state := image.pairs; state != nil {
			i := firstTag(&image.tags, image.n, code)
			var word, candidate uint64
			if i < image.n {
				candidate = state.entries[i].key.Load()
				if candidate != code {
					i = state.lower(image.n, code)
					if i < image.n {
						candidate = state.entries[i].key.Load()
					}
				}
			}
			ok := i < image.n && candidate == code
			if ok {
				word = state.entries[i].value.Load()
			}
			if !image.valid.Load() || m.version.Load() != version {
				continue
			}
			if ok {
				if image.valueKind == 1 {
					value = any(word).(V)
				} else {
					value = unpackWord[V](word, image.valueKind)
				}
			}
			return value, ok
		}
		if state := image.wordState; state != nil {
			i := 0
			if image.strings {
				i, found = image.lowerSearch(&search)
			} else {
				i = findCode(image.codes, &image.tags, image.n, code)
				found = i < image.n && image.codes[i] == code
			}
			var word uint64
			if found {
				word = state.values[i].Load()
			}
			if !image.valid.Load() || m.version.Load() != version {
				continue
			}
			if found {
				if image.valueKind == 1 {
					value = any(word).(V)
				} else {
					value = unpackWord[V](word, image.valueKind)
				}
			}
			return value, found
		}
		leaf := image.leaf
		leaf.mu.RLock()
		if leaf.sequence.Load() != image.imageVersion {
			leaf.mu.RUnlock()
			continue
		}
		i := 0
		if image.strings {
			i, found = image.lowerSearch(&search)
		} else {
			i = findCode(image.codes, &image.tags, image.n, code)
			found = i < image.n && image.codes[i] == code
		}
		if found {
			value = leaf.values[i]
		}
		leaf.mu.RUnlock()
		if m.version.Load() == version {
			return value, found
		}
	}
}

// latchedGet reads a textual leaf under its read latch. The epoch check
// comes first: a leaf released by a split or merge may share recycled
// columns, and every such change holds the leaf latch and moves the epoch.
func (n *node[K, V, O]) latchedGet(s *keySearch[K, O], epoch *atomic.Uint64, version uint64) (value V, found, valid bool) {
	n.leaf.mu.RLock()
	if epoch.Load() != version {
		n.leaf.mu.RUnlock()
		return value, false, false
	}
	i, found := n.lowerSearch(s)
	if found {
		value = n.leaf.values[i]
	}
	n.leaf.mu.RUnlock()
	return value, found, true
}

func initWordValues[K any, V any, O keyOrder[K]](n *node[K, V, O]) {
	if !n.shared {
		return
	}
	n.leaf.wordKind = wordKind[V]()
	if n.leaf.wordKind != 0 {
		if !n.strings {
			n.leaf.pairs, n.pairsID = n.arena.pairs.alloc()
			n.leaf.pairs.sequence.Store(0)
		} else {
			n.leaf.words = new(atomicWords)
		}
	}
}
