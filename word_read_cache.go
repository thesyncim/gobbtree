package gobbtree

import (
	"sync"
	"sync/atomic"
)

// Small stable-key maps benefit from immutable SIMD inputs. Large maps keep
// only the shared numeric arena. A key mutation retires the optional cache;
// existing-key replacements update its atomic values under the leaf lock.
const smallReadCacheLimit = 512 * 1024

type wordReadCache struct {
	nodes  []wordNode
	routes wordRoutes
	fill   sync.Mutex
}

func (m *wordMap) disableReadCache() {
	for {
		previous := m.view.Load()
		if previous == nil || previous.cache == nil {
			return
		}
		view := *previous
		view.cache = nil
		// Publish before any key mutation. Another writer must observe retirement
		// before it changes keys, even if the publishing goroutine is preempted.
		if m.view.CompareAndSwap(previous, &view) {
			return
		}
	}
}

// Arena IDs reserve 256 slots per slab; the first eight slabs grow from
// one to 128 nodes. Remove those early holes in the compact cache allocation.
func (c *wordReadCache) node(id uint32) *wordNode {
	index := (id &^ wordLeafID) - 1
	if index >= 8*256 {
		index -= 8*256 - 255
	} else {
		index = (1 << (index >> 8)) - 1 + (index & 255)
	}
	if index >= uint32(len(c.nodes)) {
		return nil
	}
	return &c.nodes[index]
}
func (m *wordMap) storeValue(id uint32, n *wordNode, index int, value uint64) {
	atomic.StoreUint64(&n.entries()[index].value, value)
	view := m.view.Load()
	if view != nil && view.cache != nil {
		image := view.cache.node(id)
		if image != nil && image.sequence.Load() != 0 {
			atomic.StoreUint64(&image.entries()[index].value, value)
		}
	}
}
func (m *wordMap) cacheNode(cache *wordReadCache, id uint32) *wordNode {
	image := cache.node(id)
	if image == nil {
		return nil
	}
	m.structure.RLock()
	defer m.structure.RUnlock()
	cache.fill.Lock()
	defer cache.fill.Unlock()
	view := m.view.Load()
	if view == nil || view.cache != cache {
		return nil
	}
	if image.sequence.Load() != 0 {
		return image
	}
	source := m.tree.node(id)
	if source.leaf {
		source.mu.Lock()
		defer source.mu.Unlock()
	}
	image.n, image.leaf = source.n, source.leaf
	image.level = uint8(source.sequence.Load() >> 7 & 63)
	image.prev, image.next = source.prev, source.next
	image.tags, image.keys, image.payload = source.tags, source.keys, source.payload
	image.sequence.Store(1)
	return image
}
