//go:build !unix

package gobbtree

// syncDir does nothing where a directory can't be opened to sync.
func syncDir(string) error { return nil }
