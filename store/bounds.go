package store

import "bytes"

// Bounds selects the keys of a range, as a Rust range does upstream, with
// the tree's constructors (gobbtree.Bounds): each end is included,
// excluded or open. The zero Bounds is open at both ends (..). A store
// keeps no bounds' bytes past the call that takes them.
//
//	Between(a, b)          a..b
//	AtLeast(a).AtMost(b)   a..=b
//	AtLeast(a)             a..
//	Above(a).Below(b)      (Excluded(a), Excluded(b))
//	Below(b)               ..b
//	AtMost(b)              ..=b
type Bounds struct {
	lower, upper         []byte
	lowerKind, upperKind boundKind
}

type boundKind uint8

const (
	unbounded boundKind = iota
	included
	excluded
)

// Between is the keys from lower, included, to upper, excluded.
func Between(lower, upper []byte) Bounds {
	return Bounds{lower: lower, upper: upper, lowerKind: included, upperKind: excluded}
}

// AtLeast is the keys from lower, included, with no upper end.
func AtLeast(lower []byte) Bounds { return Bounds{lower: lower, lowerKind: included} }

// Above is the keys past lower, excluded, with no upper end.
func Above(lower []byte) Bounds { return Bounds{lower: lower, lowerKind: excluded} }

// Below is the keys up to upper, excluded, with no lower end.
func Below(upper []byte) Bounds { return Bounds{upper: upper, upperKind: excluded} }

// AtMost is the keys up to upper, included, with no lower end.
func AtMost(upper []byte) Bounds { return Bounds{upper: upper, upperKind: included} }

// Below is b ending at upper, excluded.
func (b Bounds) Below(upper []byte) Bounds {
	b.upper, b.upperKind = upper, excluded
	return b
}

// AtMost is b ending at upper, included.
func (b Bounds) AtMost(upper []byte) Bounds {
	b.upper, b.upperKind = upper, included
	return b
}

// owned is b with copies of its keys.
func (b Bounds) owned() Bounds {
	if b.lowerKind != unbounded {
		b.lower = bytes.Clone(b.lower)
	}
	if b.upperKind != unbounded {
		b.upper = bytes.Clone(b.upper)
	}
	return b
}

// isEmpty reports whether no key lies between b's ends.
func (b *Bounds) isEmpty() bool {
	if b.lowerKind == unbounded || b.upperKind == unbounded {
		return false
	}
	c := bytes.Compare(b.lower, b.upper)
	if b.lowerKind == included && b.upperKind == included {
		return c > 0
	}
	return c >= 0
}

// holds reports whether key lies between b's ends.
func (b *Bounds) holds(key []byte) bool {
	switch b.lowerKind {
	case included:
		if bytes.Compare(key, b.lower) < 0 {
			return false
		}
	case excluded:
		if bytes.Compare(key, b.lower) <= 0 {
			return false
		}
	}
	switch b.upperKind {
	case included:
		return bytes.Compare(key, b.upper) <= 0
	case excluded:
		return bytes.Compare(key, b.upper) < 0
	}
	return true
}
