//go:build storepoison

package store

import "testing"

// TestAReleasedQueryPanics checks the poison storepoison's tests rely on:
// a read of a query's key after its release panics.
func TestAReleasedQueryPanics(t *testing.T) {
	var q pointQuery
	k := q.key([]byte("key"))
	if string(k.bytes()) != "key" {
		t.Fatalf("query key %q", k.bytes())
	}
	q.release()
	defer func() {
		if recover() == nil {
			t.Fatal("a released query's key was read")
		}
	}()
	k.bytes()
}
