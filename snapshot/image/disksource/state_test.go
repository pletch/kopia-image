package disksource

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShadowState_persists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")

	s, err := OpenShadowState(path)
	require.NoError(t, err)
	require.Empty(t, s.IDs())

	require.NoError(t, s.Add("{a}"))
	require.NoError(t, s.Add("{b}"))
	require.NoError(t, s.Remove("{a}"))
	require.NoError(t, s.Close())

	// a crashed run's leftovers are visible to the next run
	s, err = OpenShadowState(path)
	require.NoError(t, err)
	require.Equal(t, []string{"{b}"}, s.IDs())
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
