package gobbtree

import (
	"math"
	"strings"
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

type textImage[K any, V any, O keyOrder[K]] struct {
	image searchImage[K, V, O]
	codes [nodeSize]uint64
	keys  [nodeSize]K
}

// SIMD reads immutable tags and routing columns. Paired leaf keys and values
// are atomic; image validity and the structural epoch validate each read.
func (n *node[K, V, O]) publish() { n.publishFrom(0) }
func (n *node[K, V, O]) publishFrom(first int) {
	if !n.shared {
		return
	}
	if n.leaf != nil && n.strings {
		image := n.image.Load()
		if image == nil {
			image = &searchImage[K, V, O]{n: -1, strings: true, leaf: n.leaf, node: n}
			image.valid.Store(true)
			n.image.Store(image)
		}
		if n.parentSlot != nil {
			n.parentSlot.Store(image)
		}
		n.storeWords(first)
		return
	}
	// No retired image may validate data in a recycled atomic block.
	if old := n.image.Load(); old != nil && old.valid.Load() {
		old.valid.Store(false)
	}
	var image *searchImage[K, V, O]
	if n.strings {
		// One allocation carries a textual image and the columns it copies.
		block := &textImage[K, V, O]{codes: n.codes, keys: n.keys}
		image = &block.image
		image.codes, image.keys = &block.codes, &block.keys
	} else {
		image = new(searchImage[K, V, O])
		if n.leaf == nil || n.leaf.pairs == nil {
			image.codes = new([nodeSize]uint64)
			*image.codes = n.codes
		}
	}
	image.n, image.prefix, image.prefixHead, image.strings = n.n, n.prefix, n.prefixHead, n.strings
	image.leaf, image.readEdges = n.leaf, n.readEdges
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
		} else {
			n.storeWords(first)
		}
	}
	n.image.Store(image)
	if n.parentSlot != nil {
		n.parentSlot.Store(image)
	}
}

// storeWords mirrors values from first on into a leaf's atomic words.
func (n *node[K, V, O]) storeWords(first int) {
	words := n.leaf.words
	if words == nil {
		return
	}
	switch n.leaf.wordKind {
	case 1:
		for i := first; i < n.n; i++ {
			words.values[i].Store(any(n.leaf.values[i]).(uint64))
		}
	case 2:
		for i := first; i < n.n; i++ {
			words.values[i].Store(uint64(any(n.leaf.values[i]).(int)))
		}
	default:
		for i := first; i < n.n; i++ {
			words.values[i].Store(packWord(n.leaf.values[i]))
		}
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

// optimisticAttempts is how many failed validations a lookup tolerates before
// it reads under the leaf latch instead. Like upstream's, the latched read
// takes no structure-wide lock, so a reader never holds up writers.
const optimisticAttempts = 4

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

// optimisticWord reads a direct textual leaf with word values without its
// latch. Such leaves hold interned keys, so the reader loads only a key's
// data word, which cannot tear, and takes the length stored before its
// immutable bytes. The leaf sequence and structural epoch validate every
// plain load; loadFence keeps those loads ahead of the validation.
func (n *node[K, V, O]) optimisticWord(s *keySearch[K, O], words *atomicWords, epoch *atomic.Uint64, version uint64) (word uint64, found, ok bool) {
	sequence := words.sequence.Load()
	if sequence&1 != 0 {
		return 0, false, false
	}
	count, prefix := min(n.n, nodeSize), n.prefix
	key := textView(&s.key)
	match := count > 0
	if match && prefix > 0 {
		first := internedText(&n.keys[0])
		if prefix > len(first) {
			return 0, false, false
		}
		match = strings.Compare(key[:min(len(key), prefix)], first[:prefix]) == 0
	}
	if match {
		code := stringCode(key, prefix)
		for i := lowerCode(&n.codes, count, code); i < count && n.codes[i] == code; i++ {
			c := strings.Compare(internedText(&n.keys[i]), key)
			if c == 0 {
				word, found = words.values[i].Load(), true
			}
			if c >= 0 {
				break
			}
		}
	}
	loadFence()
	if words.sequence.Load() != sequence || epoch.Load() != version {
		return 0, false, false
	}
	return word, found, true
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
