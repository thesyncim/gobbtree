package gobbtree

import (
	"cmp"
	"math"
	"runtime"
	"sync/atomic"
)

type atomicWords struct {
	sequence atomic.Uint64
	values   [nodeSize]atomic.Uint64
}

func (l *leaf[K, V]) sequenceValue() uint64 {
	if l.words != nil {
		return l.words.sequence.Load()
	}
	return l.sequence.Load()
}
func (l *leaf[K, V]) advanceSequence() {
	if l.words != nil {
		l.words.sequence.Add(1)
	} else {
		l.sequence.Add(1)
	}
}

// Published search images are immutable. SIMD reads only these images, never
// columns a writer can modify. Go's GC keeps old images alive for readers.
func (n *node[K, V]) publish() {
	if !n.shared {
		return
	}
	image := &searchImage[K, V]{n: n.n, prefix: n.prefix, strings: n.strings, codes: n.codes, leaf: n.leaf, readEdges: n.readEdges}
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
		image.wordState = n.leaf.words
		image.valueKind = n.leaf.wordKind
		if n.leaf.words != nil {
			for i := 0; i < n.n; i++ {
				n.leaf.words.values[i].Store(packWord(n.leaf.values[i]))
			}
		}
	}
	n.image.Store(image)
	if n.parentSlot != nil {
		n.parentSlot.Store(image)
	}
}
func (n *node[K, V]) setValue(i int, value V) {
	n.leaf.values[i] = value
	if n.leaf.words != nil {
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
func packWord[V any](value V) uint64 {
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
func (m *Map[K, V]) structuralStart(key K) [3]*node[K, V] {
	m.version.Add(1)
	m.tree.shared = true
	var held [3]*node[K, V]
	if m.tree.root != nil {
		target := m.tree.root.findLeaf(key)
		held = [3]*node[K, V]{target.leaf.prev, target, target.leaf.next}
		for _, n := range held {
			if n != nil {
				n.leaf.mu.Lock()
			}
		}
	}
	return held
}
func (m *Map[K, V]) structuralEnd(held [3]*node[K, V]) {
	if m.tree.root == nil {
		m.root.Store(nil)
	} else {
		m.tree.root.parentSlot = &m.root
		m.root.Store(m.tree.root.image.Load())
	}
	m.version.Add(1)
	for _, n := range held {
		if n != nil {
			n.leaf.mu.Unlock()
		}
	}
}

func (m *Map[K, V]) optimisticGet(key K) (V, bool) {
	var zero V
	code := numericCode(key)
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
		// Keys are immutable; child links are atomics. A structural epoch
		// validates the entire route, including a concurrent move of an edge.
		for image.readEdges != nil {
			index := 0
			if image.strings {
				index = image.childIndex(key)
			} else {
				index = routeCode(&image.codes, image.n, code)
			}
			image = image.readEdges[index].Load()
			if image == nil {
				continue retry
			}
		}
		var value V
		var found bool
		if state := image.wordState; state != nil {
			sequence := state.sequence.Load()
			if sequence&1 != 0 || sequence != image.imageVersion {
				continue
			}
			i := 0
			if image.strings {
				i = image.lower(key)
			} else {
				i = lowerCode(&image.codes, image.n, code)
			}
			found = i < image.n && ((!image.strings && image.codes[i] == code) || (image.strings && cmp.Compare(image.keys[i], key) == 0))
			var word uint64
			if found {
				word = state.values[i].Load()
			}
			if state.sequence.Load() != sequence || m.version.Load() != version {
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
			i = image.lower(key)
		} else {
			i = lowerCode(&image.codes, image.n, code)
		}
		found = i < image.n && ((!image.strings && image.codes[i] == code) || (image.strings && cmp.Compare(image.keys[i], key) == 0))
		if found {
			value = leaf.values[i]
		}
		leaf.mu.RUnlock()
		if m.version.Load() == version {
			return value, found
		}
	}
}

func initWordValues[K cmp.Ordered, V any](n *node[K, V]) {
	if !n.shared {
		return
	}
	n.leaf.wordKind = wordKind[V]()
	if n.leaf.wordKind != 0 {
		n.leaf.words = new(atomicWords)
	}
}
