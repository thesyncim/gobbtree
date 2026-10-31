package store

import (
	"bytes"
	"iter"
	"slices"
)

// RangeCopies is copies of the entries of a range of a Store, in key order
// from the front (Next), in reverse from the back (NextBack), from
// Store.Range, upstream's RangeCopies (range.rs). It holds no latch between
// calls, and is one goroutine's.
//
// Each end copies up to rangeBatch entries at a time by a scan that starts
// past what it handed out last, and ends before what the other end handed
// out last. It hands a copy out only if the other end hasn't handed it, or
// anything past it, out since. So, as a scan does, it hands out what other
// goroutines insert ahead of it, and nothing twice.
type RangeCopies struct {
	store *Store
	// left is the range left: from past what the front handed out last, or
	// the range's start, to before what the back handed out last, or the
	// range's end.
	left Bounds
	// front and back are the entries each end copied ahead, the next one
	// last.
	front, back []rangeEntry
}

// rangeEntry is a copied entry.
type rangeEntry struct{ key, value []byte }

// rangeBatch is the most entries a copy takes.
const rangeBatch = 16

func newRangeCopies(s *Store, b Bounds) *RangeCopies { return &RangeCopies{store: s, left: b} }

// copyBatch copies up to a batch of entries from the front if ascending,
// else from the back, the next one last.
func (c *RangeCopies) copyBatch(ascending bool) []rangeEntry {
	if c.left.isEmpty() {
		return nil
	}
	entries := c.store.Scan(c.left)
	if !ascending {
		entries = c.store.ScanReverse(c.left)
	}
	var copied []rangeEntry
	for key, value := range entries {
		copied = append(copied, rangeEntry{bytes.Clone(key), bytes.Clone(value)})
		if len(copied) == rangeBatch {
			break
		}
	}
	slices.Reverse(copied)
	return copied
}

// finish stops both ends: they meet.
func (c *RangeCopies) finish() ([]byte, []byte, bool) {
	c.front, c.back = nil, nil
	c.left = Above(nil).Below(nil)
	return nil, nil, false
}

// Next is the next entry from the front, false once the ends meet.
func (c *RangeCopies) Next() (key, value []byte, ok bool) {
	if len(c.front) == 0 {
		c.front = c.copyBatch(true)
	}
	n := len(c.front)
	if n == 0 {
		return c.finish()
	}
	entry := c.front[n-1]
	c.front = c.front[:n-1]
	if !c.left.holds(entry.key) {
		return c.finish()
	}
	c.left.lower, c.left.lowerKind = bytes.Clone(entry.key), excluded
	return entry.key, entry.value, true
}

// NextBack is the next entry from the back, false once the ends meet.
func (c *RangeCopies) NextBack() (key, value []byte, ok bool) {
	if len(c.back) == 0 {
		c.back = c.copyBatch(false)
	}
	n := len(c.back)
	if n == 0 {
		return c.finish()
	}
	entry := c.back[n-1]
	c.back = c.back[:n-1]
	if !c.left.holds(entry.key) {
		return c.finish()
	}
	c.left.upper, c.left.upperKind = bytes.Clone(entry.key), excluded
	return entry.key, entry.value, true
}

// All iterates the copies from the front, as Next hands them out.
func (c *RangeCopies) All() iter.Seq2[[]byte, []byte] {
	return func(yield func(key, value []byte) bool) {
		for {
			key, value, ok := c.Next()
			if !ok || !yield(key, value) {
				return
			}
		}
	}
}

// Backward iterates the copies from the back, as NextBack hands them out.
func (c *RangeCopies) Backward() iter.Seq2[[]byte, []byte] {
	return func(yield func(key, value []byte) bool) {
		for {
			key, value, ok := c.NextBack()
			if !ok || !yield(key, value) {
				return
			}
		}
	}
}
