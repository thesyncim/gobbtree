package gobbtree

// Bounds selects the keys of a range, as a Rust range does upstream: each end
// is included, excluded or open. The zero Bounds is open at both ends (..).
//
//	Between(a, b)          a..b
//	AtLeast(a).AtMost(b)   a..=b
//	AtLeast(a)             a..
//	Above(a).Below(b)      (Excluded(a), Excluded(b))
//	Below(b)               ..b
//	AtMost(b)              ..=b
//
// Methods that take Bounds panic, as upstream does, when the lower end is
// above the upper one, or when both ends are the same excluded key.
type Bounds[K any] struct {
	lower, upper         K
	lowerKind, upperKind boundKind
}

type boundKind uint8

// Any two bounded kinds share a bit, so one AND tells whether both ends are
// bounded.
const (
	unbounded boundKind = 0
	included  boundKind = 1
	excluded  boundKind = 3
)

// Between returns the keys from lower, included, to upper, excluded.
func Between[K any](lower, upper K) Bounds[K] {
	return Bounds[K]{lower: lower, upper: upper, lowerKind: included, upperKind: excluded}
}

// AtLeast returns the keys from lower, included, with no upper end.
func AtLeast[K any](lower K) Bounds[K] { return Bounds[K]{lower: lower, lowerKind: included} }

// Above returns the keys past lower, excluded, with no upper end.
func Above[K any](lower K) Bounds[K] { return Bounds[K]{lower: lower, lowerKind: excluded} }

// Below returns the keys up to upper, excluded, with no lower end.
func Below[K any](upper K) Bounds[K] { return Bounds[K]{upper: upper, upperKind: excluded} }

// AtMost returns the keys up to upper, included, with no lower end.
func AtMost[K any](upper K) Bounds[K] { return Bounds[K]{upper: upper, upperKind: included} }

// Below returns b ending at upper, excluded.
func (b Bounds[K]) Below(upper K) Bounds[K] {
	b.upper, b.upperKind = upper, excluded
	return b
}

// AtMost returns b ending at upper, included.
func (b Bounds[K]) AtMost(upper K) Bounds[K] {
	b.upper, b.upperKind = upper, included
	return b
}

// checkBounds panics on the bounds upstream rejects.
func checkBounds[K any, O keyOrder[K]](b *Bounds[K]) {
	if b.lowerKind&b.upperKind != 0 {
		checkEnds[K, O](b)
	}
}

func checkEnds[K any, O keyOrder[K]](b *Bounds[K]) {
	switch c := compareKeys[O](b.lower, b.upper); {
	case c > 0:
		panic("gobbtree: range start is greater than range end")
	case c == 0 && b.lowerKind == excluded && b.upperKind == excluded:
		panic("gobbtree: range start and end are equal and excluded")
	}
}

// aboveLower reports whether key is inside b's lower end.
func aboveLower[K any, O keyOrder[K]](b *Bounds[K], key K) bool {
	switch b.lowerKind {
	case included:
		return compareKeys[O](key, b.lower) >= 0
	case excluded:
		return compareKeys[O](key, b.lower) > 0
	}
	return true
}

// belowUpper reports whether key is inside b's upper end.
func belowUpper[K any, O keyOrder[K]](b *Bounds[K], key K) bool {
	switch b.upperKind {
	case included:
		return compareKeys[O](key, b.upper) <= 0
	case excluded:
		return compareKeys[O](key, b.upper) < 0
	}
	return true
}

// wordBounds gives b over order-preserving codes as [low, high), or [low, ∞)
// when open; empty reports that no code is inside.
func wordBounds[K any, O keyOrder[K]](b *Bounds[K]) (low, high uint64, open, empty bool) {
	switch b.lowerKind {
	case included:
		low = numericKey[O](b.lower)
	case excluded:
		low = numericKey[O](b.lower)
		if low == ^uint64(0) {
			return 0, 0, false, true
		}
		low++
	}
	switch b.upperKind {
	case unbounded:
		return low, 0, true, false
	case included:
		high = numericKey[O](b.upper)
		if high == ^uint64(0) {
			return low, 0, true, false
		}
		high++
	case excluded:
		high = numericKey[O](b.upper)
	}
	return low, high, false, low >= high
}
