package disksource_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mxk/go-vss"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/vhdxtest"
	"github.com/kopia/kopia/snapshot/image/disksource"
)

// writeState writes a state file as a run that then crashed would leave it.
func writeState(t *testing.T, path string, records ...disksource.ShadowCopyRecord) {
	t.Helper()

	b, err := json.Marshal(map[string]any{"shadowCopies": records})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o600))
}

func TestAcquire_removesOnlyItsOwnLeftoverShadowCopies(t *testing.T) {
	mnt := vhdxtest.New(t).FormatAndMount()
	volume := vhdxtest.VolumeGUIDPath(t, mnt)

	id, err := vss.Create(volume)
	require.NoError(t, err)

	t.Cleanup(func() { _ = vss.Remove(id) })

	sc, err := vss.Get(id)
	require.NoError(t, err)

	statePath := filepath.Join(t.TempDir(), "state.json")

	// a state file that names an existing shadow copy but does not describe
	// it, as someone who could write the file but did not create the shadow
	// copy would have to write it
	for name, rec := range map[string]disksource.ShadowCopyRecord{
		"no details":            {ID: id},
		"another creation time": {ID: id, Volume: sc.VolumeName, Created: sc.InstallDate.Add(time.Minute)},
		"another volume":        {ID: id, Volume: `\\?\Volume{00000000-0000-0000-0000-000000000000}\`, Created: sc.InstallDate},
	} {
		writeState(t, statePath, rec)

		guard, err := disksource.Acquire(statePath, t.Logf)
		require.NoError(t, err, name)
		require.Empty(t, guard.State.Records(), "%v: the record is dropped", name)
		require.NoError(t, guard.Close())

		_, err = vss.Get(id)
		require.NoError(t, err, "%v: the shadow copy must survive", name)
	}

	// the record a run writes for its own shadow copy
	writeState(t, statePath, disksource.ShadowCopyRecord{ID: id, Volume: sc.VolumeName, Created: sc.InstallDate})

	guard, err := disksource.Acquire(statePath, t.Logf)
	require.NoError(t, err)
	require.Empty(t, guard.State.Records())
	require.NoError(t, guard.Close())

	_, err = vss.Get(id)
	require.Error(t, err, "the leftover shadow copy is removed")

	// a record of a shadow copy that is already gone is just forgotten
	writeState(t, statePath, disksource.ShadowCopyRecord{ID: id, Volume: sc.VolumeName, Created: sc.InstallDate})

	guard, err = disksource.Acquire(statePath, t.Logf)
	require.NoError(t, err)
	require.Empty(t, guard.State.Records())
	require.NoError(t, guard.Close())
}
