//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package gobbtree

// adviseHugePages does nothing: as upstream, huge pages are advised on Linux
// only.
func adviseHugePages([]byte) {}
