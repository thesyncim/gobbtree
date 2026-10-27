package gobbtree_test

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/thesyncim/gobbtree"
)

// Version orders by major, then minor: two big-endian bytes, then one.
type Version struct{ Major, Minor uint8 }

func (v Version) Compare(o Version) int {
	return cmp.Or(cmp.Compare(v.Major, o.Major), cmp.Compare(v.Minor, o.Minor))
}

func (v Version) Window(offset int) uint64 {
	return gobbtree.Uint64Window(uint64(v.Major)<<56|uint64(v.Minor)<<48, offset)
}

func ExampleTreeOf() {
	releases := gobbtree.NewTreeOf[Version, string]()
	releases.Insert(Version{1, 10}, "stable")
	releases.Insert(Version{1, 2}, "old")
	releases.Insert(Version{2, 0}, "next")
	for v, name := range releases.All() {
		fmt.Printf("%d.%d %s\n", v.Major, v.Minor, name)
	}
	// Output:
	// 1.2 old
	// 1.10 stable
	// 2.0 next
}

// Tag orders case-insensitively.
type Tag string

func (t Tag) Compare(o Tag) int {
	return strings.Compare(strings.ToLower(string(t)), strings.ToLower(string(o)))
}

func (t Tag) Window(offset int) uint64 {
	return gobbtree.StringWindow(strings.ToLower(string(t)), offset)
}

func ExampleMapOf() {
	tags := gobbtree.NewMapOf[Tag, int]()
	tags.Insert("Go", 1)
	tags.Insert("GO", 2) // the same key: replaces the value, keeps "Go"
	tags.Insert("arm64", 3)
	for tag, n := range tags.All() {
		fmt.Println(tag, n)
	}
	// Output:
	// arm64 3
	// Go 2
}
