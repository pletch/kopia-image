package disksource

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	volumeA = `\\?\Volume{aaaaaaaa-0000-0000-0000-000000000000}\`
	volumeB = `\\?\Volume{bbbbbbbb-0000-0000-0000-000000000000}\`
)

func TestShadowState_persists(t *testing.T) {
	created := time.Date(2026, 11, 16, 2, 0, 0, 123000000, time.Local)
	path := filepath.Join(t.TempDir(), "sub", "state.json")

	s, err := OpenShadowState(path)
	require.NoError(t, err)
	require.Empty(t, s.Records())

	require.NoError(t, s.Add(ShadowCopyRecord{ID: "{a}", Volume: volumeA, Created: created}))
	require.NoError(t, s.Add(ShadowCopyRecord{ID: "{b}", Volume: volumeB, Created: created}))
	require.NoError(t, s.Remove("{a}"))
	require.NoError(t, s.Close())

	// a crashed run's leftovers are visible to the next run
	s, err = OpenShadowState(path)
	require.NoError(t, err)

	records := s.Records()
	require.Len(t, records, 1)
	require.Equal(t, "{b}", records[0].ID)
	require.True(t, records[0].Matches(strings.ToUpper(volumeB), created.UTC()), "the volume and creation time survive a reload")
	require.NoError(t, s.Close())
}

func TestShadowState_exclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	s, err := OpenShadowState(path)
	require.NoError(t, err)

	_, err = OpenShadowState(path)
	require.ErrorIs(t, err, ErrAlreadyRunning)

	require.NoError(t, s.Close())

	s, err = OpenShadowState(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func TestShadowState_corruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := OpenShadowState(path)
	require.Error(t, err)

	// the lock is released on failure
	require.NoError(t, os.Remove(path))

	s, err := OpenShadowState(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

var errFirst = errors.New("first")

func TestSessionAndGuard_closeInReverseOrder(t *testing.T) {
	var order []int

	s := &Session{closers: []func() error{
		func() error { order = append(order, 1); return errFirst },
		func() error { order = append(order, 2); return nil },
	}}

	require.ErrorIs(t, s.Close(), errFirst)
	require.Equal(t, []int{2, 1}, order)
	require.NoError(t, s.Close(), "second close is a no-op")

	order = nil
	g := &Guard{closers: []func() error{
		func() error { order = append(order, 1); return nil },
		func() error { order = append(order, 2); return nil },
	}}

	require.NoError(t, g.Close())
	require.Equal(t, []int{2, 1}, order)
}

func TestShadowCopyRecord_Matches(t *testing.T) {
	created := time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC)
	rec := ShadowCopyRecord{ID: "{a}", Volume: volumeA, Created: created}

	require.True(t, rec.Matches(volumeA, created))
	require.False(t, rec.Matches(volumeB, created), "another volume")
	require.False(t, rec.Matches(volumeA, created.Add(time.Second)), "another creation time")
	require.False(t, ShadowCopyRecord{ID: "{a}"}.Matches("", time.Time{}), "a record without details matches nothing")
}
