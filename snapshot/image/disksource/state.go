package disksource

import (
	"encoding/json"
	stderrors "errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/gofrs/flock"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
)

// ErrAlreadyRunning is returned when another image run holds the state lock.
var ErrAlreadyRunning = errors.New("another kopia image create is running")

// ShadowState records the shadow copies the current run created, in a file
// that outlives the process. Shadow copies created through WMI persist across
// reboots, so a run that crashes or loses power leaves them behind; the next
// run removes the ones listed here. The state also holds an exclusive lock so
// two runs never remove each other's shadow copies.
type ShadowState struct {
	path string
	lock *flock.Flock
	ids  []string
}

type shadowStateFile struct {
	ShadowCopyIDs []string `json:"shadowCopyIds"`
}

// OpenShadowState locks and reads the state file at path, creating its
// directory if needed. It returns ErrAlreadyRunning if another process holds
// the lock.
func OpenShadowState(path string) (*ShadowState, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:mnd
		return nil, errors.Wrap(err, "unable to create state directory")
	}

	lock := flock.New(path + ".lock")

	ok, err := lock.TryLock()
	if err != nil {
		return nil, errors.Wrap(err, "unable to lock image state")
	}

	if !ok {
		return nil, ErrAlreadyRunning
	}

	s := &ShadowState{path: path, lock: lock}

	b, err := os.ReadFile(path) //nolint:gosec
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}

	if err == nil {
		var f shadowStateFile

		err = json.Unmarshal(b, &f)
		s.ids = f.ShadowCopyIDs
	}

	if err != nil {
		return nil, stderrors.Join(errors.Wrapf(err, "unable to read %v", path), lock.Unlock())
	}

	return s, nil
}

// IDs returns the recorded shadow copy IDs.
func (s *ShadowState) IDs() []string {
	return slices.Clone(s.ids)
}

// Add records a shadow copy ID and saves the state.
func (s *ShadowState) Add(id string) error {
	s.ids = append(s.ids, id)
	return s.save()
}

// Remove forgets a shadow copy ID and saves the state.
func (s *ShadowState) Remove(id string) error {
	s.ids = slices.DeleteFunc(s.ids, func(v string) bool { return v == id })
	return s.save()
}

// Close releases the lock.
func (s *ShadowState) Close() error {
	return errors.Wrap(s.lock.Unlock(), "unable to unlock image state")
}

func (s *ShadowState) save() error {
	b, err := json.Marshal(shadowStateFile{ShadowCopyIDs: s.ids})
	if err != nil {
		return errors.Wrap(err, "unable to encode image state")
	}

	tmp := s.path + ".tmp"

	if err := os.WriteFile(tmp, b, 0o600); err != nil { //nolint:mnd
		return errors.Wrap(err, "unable to write image state")
	}

	return errors.Wrap(os.Rename(tmp, s.path), "unable to save image state")
}

// Session holds the resources of imaging one disk: open devices and shadow
// copies. Close releases them.
type Session struct {
	volumes []image.VolumeSource
	closers []func() error
}

// Volumes returns the volume sources to pass to image.Create.
func (s *Session) Volumes() []image.VolumeSource {
	return s.volumes
}

// Close releases the session's resources in reverse order of acquisition.
func (s *Session) Close() error {
	var err error

	for _, c := range slices.Backward(s.closers) {
		err = stderrors.Join(err, c())
	}

	s.closers = nil

	return err
}

// Guard protects a whole run: it holds the shadow state lock and keeps the
// computer awake. Close releases both.
type Guard struct {
	State   *ShadowState
	closers []func() error
}

// Close releases the guard.
func (g *Guard) Close() error {
	var err error

	for _, c := range slices.Backward(g.closers) {
		err = stderrors.Join(err, c())
	}

	g.closers = nil

	return err
}
