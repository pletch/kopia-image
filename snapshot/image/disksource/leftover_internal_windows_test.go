package disksource

import (
	"errors"
	"testing"
	"time"

	"github.com/mxk/go-vss"
	"github.com/stretchr/testify/require"
)

var errServiceStarting = errors.New("the service is starting")

func TestRemoveLeftover_keepsRecordWhenLookupFails(t *testing.T) {
	rec := ShadowCopyRecord{ID: "{11111111-2222-3333-4444-555555555555}", Volume: volumeA, Created: time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC)}

	failing := func(string) (*vss.ShadowCopy, error) { return nil, errServiceStarting }
	require.False(t, removeLeftover(rec, failing, t.Logf), "a failed lookup says nothing about the shadow copy")

	gone := func(string) (*vss.ShadowCopy, error) { return nil, nil } //nolint:nilnil
	require.True(t, removeLeftover(rec, gone, t.Logf), "a shadow copy that is gone is forgotten")

	// one that exists but is not the recorded one is forgotten and not removed:
	// removing it would fail the test, since there is no such shadow copy
	other := func(id string) (*vss.ShadowCopy, error) {
		return &vss.ShadowCopy{ID: id, VolumeName: volumeB, InstallDate: rec.Created}, nil
	}
	require.True(t, removeLeftover(rec, other, t.Logf))
}
