package disksource

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
)

func TestSingleDisk(t *testing.T) {
	n, err := singleDisk([]blockdev.DiskExtent{{DiskNumber: 3, Offset: 597688320, Length: 998608207872}})
	require.NoError(t, err)
	require.Equal(t, uint32(3), n)

	// Google Drive's virtual drive reports one empty extent on disk 0.
	_, err = singleDisk([]blockdev.DiskExtent{{DiskNumber: 0}})
	require.ErrorIs(t, err, errNotOnPhysicalDisk)

	_, err = singleDisk(nil)
	require.ErrorIs(t, err, errNotOnPhysicalDisk)

	_, err = singleDisk([]blockdev.DiskExtent{
		{DiskNumber: 1, Length: 1 << 30},
		{DiskNumber: 2, Length: 1 << 30},
	})
	require.ErrorIs(t, err, image.ErrUnsupportedLayout)

	// Empty extents do not count toward spanning.
	n, err = singleDisk([]blockdev.DiskExtent{{DiskNumber: 0}, {DiskNumber: 4, Length: 1 << 30}})
	require.NoError(t, err)
	require.Equal(t, uint32(4), n)
}
