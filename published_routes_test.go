package gobbtree

import (
	"sync"
	"sync/atomic"
	"testing"
)

// nodeRoutedKey is a defined uint64. The numeric arena accepts only built-in
// word types, so these tests exercise the node tree and its published routes.
type nodeRoutedKey uint64

func TestPublishedRoutesTrackStructure(t *testing.T) {
	var m Map[nodeRoutedKey, uint64]
	const size = 20000
	keys := make([]nodeRoutedKey, size)
	for i := range keys {
		keys[i] = nodeRoutedKey(mixedKey(uint64(i)))
		m.Insert(keys[i], uint64(i))
	}
	check := func() {
		routes := m.routes.Load()
		if routes == nil {
			t.Fatal("missing published routing table")
		}
		depth := int(min(2, m.tree.root.level-1))
		for bucket, slot := range routes.slots {
			if slot == nil {
				continue
			}
			low := routes.base | uint64(bucket)<<routes.shift
			high := low | ((uint64(1) << routes.shift) - 1)
			for _, key := range []uint64{low, high} {
				n := m.tree.root
				for step := 0; step < depth; step++ {
					i := 0
					for i+1 < n.n && n.codes[i+1] <= key {
						i++
					}
					n = n.children[i]
				}
				if slot != n.parentSlot || slot.Load() != n.image.Load() {
					t.Fatal("stale published route")
				}
			}
		}
	}
	check()
	for i, key := range keys {
		if i%2 == 0 {
			m.Delete(key)
		} else {
			m.Insert(key, uint64(i)+1)
		}
		if i%257 == 0 {
			check()
		}
	}
	for i, key := range keys {
		value, ok := m.Get(key)
		if ok != (i%2 != 0) || (ok && value != uint64(i)+1) {
			t.Fatalf("cached lookup %d", i)
		}
	}
	m.Clear()
	if m.routes.Load() != nil {
		t.Fatal("clear retained routes")
	}
}

func TestPublishedRoutesConcurrentRebuild(t *testing.T) {
	var m Map[uint64, uint64]
	const size = 12000
	for i := 0; i < size; i++ {
		k := mixedKey(uint64(i))
		m.Insert(k, k^12345)
	}
	var done atomic.Bool
	var readers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		readers.Go(func() {
			for i := 0; !done.Load(); i++ {
				k := mixedKey(uint64(i % size))
				if v, ok := m.Get(k); ok && v != k^12345 {
					t.Error("cached reader observed a key/value mismatch")
					return
				}
			}
		})
	}
	for round := 0; round < 2; round++ {
		for i := 0; i < size; i++ {
			k := mixedKey(uint64(i))
			m.Delete(k)
			m.Insert(k, k^12345)
		}
		m.Clear()
		for i := size - 1; i >= 0; i-- {
			k := mixedKey(uint64(i))
			m.Insert(k, k^12345)
		}
	}
	done.Store(true)
	readers.Wait()
}

func TestRetiredNumericImagesRemainInvalidAfterArenaReuse(t *testing.T) {
	var m Map[nodeRoutedKey, uint64]
	const size = 4096
	const mask = uint64(0x9e3779b97f4a7c15)
	for key := nodeRoutedKey(0); key < size; key++ {
		m.Insert(key, uint64(key)^mask)
	}
	type retired struct {
		image *searchImage[nodeRoutedKey, uint64, natural[nodeRoutedKey]]
		key   nodeRoutedKey
	}
	var images []retired
	for n := m.tree.firstLeaf(); n != nil; n = n.leaf.next {
		images = append(images, retired{n.image.Load(), n.keys[0]})
	}
	var stop atomic.Bool
	var readers sync.WaitGroup
	readers.Go(func() {
		for !stop.Load() {
			for _, old := range images {
				value, found, valid := old.image.readValue(old.key, uint64(old.key), &m.version, m.version.Load())
				if valid && found && value != uint64(old.key)^mask {
					t.Error("retired image validated reused key/value storage")
					return
				}
			}
		}
	})
	for key := nodeRoutedKey(1); key < size; key++ {
		m.Delete(key)
	}
	arena := m.tree.arena
	if len(arena.pairs.free) == 0 {
		t.Fatal("deletions did not reclaim numeric blocks")
	}
	reused := make(map[*atomicPairs]bool)
	for _, old := range images {
		reused[old.image.pairs] = true
	}
	for key := nodeRoutedKey(size); key < 2*size; key++ {
		m.Insert(key, uint64(key)^mask)
	}
	stop.Store(true)
	readers.Wait()
	count := 0
	for n := m.tree.firstLeaf(); n != nil; n = n.leaf.next {
		if reused[n.leaf.pairs] {
			count++
		}
	}
	if count < 2 {
		t.Fatal("test did not exercise arena reuse")
	}
	for _, old := range images {
		if old.image.valid.Load() {
			t.Fatal("retired image became valid again")
		}
	}
	for key := nodeRoutedKey(size); key < 2*size; key++ {
		if value, ok := m.Get(key); !ok || value != uint64(key)^mask {
			t.Fatalf("reused leaf lookup %d", key)
		}
	}
}
