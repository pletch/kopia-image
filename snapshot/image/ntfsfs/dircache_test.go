package ntfsfs

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/fs"
)

func testListing(n int) *dirListing {
	entries := make([]fs.Entry, n)
	for i := range entries {
		e := &fileEntry{}
		e.name = string(rune('a' + i%26))
		entries[i] = e
	}

	return newDirListing(entries)
}

func TestDirCache_evictsLeastRecentlyUsed(t *testing.T) {
	var c dirCache

	half := maxCachedDirEntries / 2

	c.put(1, testListing(half))
	c.put(2, testListing(half))
	require.NotNil(t, c.get(1)) // 1 is now the most recently used

	c.put(3, testListing(1))

	require.NotNil(t, c.get(1))
	require.Nil(t, c.get(2))
	require.NotNil(t, c.get(3))
}

func TestDirCache_keepsALargeListing(t *testing.T) {
	var c dirCache

	c.put(1, testListing(1))
	c.put(2, testListing(maxCachedDirEntries+1))

	require.Nil(t, c.get(1))
	require.NotNil(t, c.get(2))

	c.purge()
	require.Nil(t, c.get(2))
}

func TestDirListing_firstNameWins(t *testing.T) {
	l := testListing(27) // "a" twice

	require.Same(t, l.entries[0], l.byName["a"])
}
