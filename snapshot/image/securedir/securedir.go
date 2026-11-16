// Package securedir creates folders that programs running as SYSTEM can
// trust: owned by Administrators, writable only by SYSTEM and Administrators,
// and holding nothing an unprivileged user put there.
//
// Folders under ProgramData need this. By default every user can create files
// and folders there, the creator of a folder stays its owner and can change
// its permissions back, and a file a user created earlier stays theirs.
//
// Securing folders is Windows-only and returns ErrNotSupported elsewhere.
package securedir

import "github.com/pkg/errors"

// ErrNotSupported is returned on platforms other than Windows.
var ErrNotSupported = errors.New("securing folders is supported only on Windows")

// ErrUntrusted is returned when a folder holds an entry that an unprivileged
// user could have put there: one not owned by SYSTEM or Administrators, or a
// link to another location.
var ErrUntrusted = errors.New("not created by an administrator")

// Access selects who can read a secured folder. SYSTEM and Administrators
// always have full control and nobody else can write.
type Access int

const (
	// AdminsOnly lets nobody else open the folder.
	AdminsOnly Access = iota

	// UsersRead lets other users list the folder and read its files.
	UsersRead
)
