package manifest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/blobtesting"
	"github.com/kopia/kopia/internal/faketime"
	"github.com/kopia/kopia/internal/testlogging"
)

// TestManifestDeletedInTheSameTick checks that a manifest deleted with the
// same modification time as it was created with, as within one tick of a
// coarse clock, stays deleted after both were flushed separately. The two
// entries are in different contents, which are merged in no particular
// order, so the check is repeated.
func TestManifestDeletedInTheSameTick(t *testing.T) {
	ctx := testlogging.Context(t)
	now := faketime.Frozen(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))

	for i := range 20 {
		data := blobtesting.DataMap{}

		bm := newContentManagerForTesting(ctx, t, data, contentManagerOpts{})

		mgr, err := NewManager(ctx, bm, ManagerOptions{TimeNow: now}, nil)
		require.NoError(t, err)

		id, err := mgr.Put(ctx, map[string]string{"type": "checkpoint"}, "payload")
		require.NoError(t, err)
		require.NoError(t, mgr.Flush(ctx))
		require.NoError(t, bm.Flush(ctx))

		require.NoError(t, mgr.Delete(ctx, id))
		require.NoError(t, mgr.Flush(ctx))
		require.NoError(t, bm.Flush(ctx))

		// a new manager loads both contents from the repository
		mgr2 := newManagerForTesting(ctx, t, data, ManagerOptions{TimeNow: now})

		_, err = mgr2.GetMetadata(ctx, id)
		require.ErrorIs(t, err, ErrNotFound, "attempt %v", i)

		found, err := mgr2.Find(ctx, map[string]string{"type": "checkpoint"})
		require.NoError(t, err)
		require.Empty(t, found, "attempt %v", i)
	}
}

// TestManifestDeletedInTheSameTick_visible checks that the manifest is
// visible to a new manager before it is deleted, so that the test above
// does not pass for want of anything stored.
func TestManifestDeletedInTheSameTick_visible(t *testing.T) {
	ctx := testlogging.Context(t)
	data := blobtesting.DataMap{}

	bm := newContentManagerForTesting(ctx, t, data, contentManagerOpts{})

	mgr, err := NewManager(ctx, bm, ManagerOptions{}, nil)
	require.NoError(t, err)

	id, err := mgr.Put(ctx, map[string]string{"type": "checkpoint"}, "payload")
	require.NoError(t, err)
	require.NoError(t, mgr.Flush(ctx))
	require.NoError(t, bm.Flush(ctx))

	_, err = newManagerForTesting(ctx, t, data, ManagerOptions{}).GetMetadata(ctx, id)
	require.NoError(t, err)
}
