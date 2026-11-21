package image

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProgressChecking(t *testing.T) {
	p := &Progress{}
	p.startDisk([]VolumeSource{{Size: 100}, {Size: 50}})
	p.startVolume(1, "p3-C.img")
	p.addVolumeBytes(100)

	require.False(t, p.Snapshot().Checking)

	// the volume is read: its upload is saved, then its chunks checked
	p.startChecking()
	require.Equal(t, ProgressSnapshot{Volume: 1, Volumes: 2, VolumeName: "p3-C.img", BytesDone: 100, BytesTotal: 150, Checking: true}, p.Snapshot())

	p.checkedChunks(40, 90)
	s := p.Snapshot()
	require.True(t, s.Checking)
	require.Equal(t, int64(40), s.ChunksChecked)
	require.Equal(t, int64(90), s.ChunksToCheck)

	// done with the volume, and the next one is read again
	p.finishVolume(100)
	require.False(t, p.Snapshot().Checking)

	p.startVolume(2, "p4-recovery.img")
	require.Zero(t, p.Snapshot().ChunksToCheck)

	// a nil Progress ignores updates
	var none *Progress
	none.startChecking()
	none.checkedChunks(1, 2)
}
