package securedir

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
)

// Security descriptors in SDDL form: owned by Administrators, with a DACL
// that is protected from inherited entries and inherited by everything
// created below the folder.
const (
	adminsOnlySDDL = "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	usersReadSDDL  = adminsOnlySDDL + "(A;OICI;0x1200a9;;;BU)"

	// adminsOnlyFileSDDL is adminsOnlySDDL for a single file.
	adminsOnlyFileSDDL = "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"

	directoryPermissions = 0o700
	filePermissions      = 0o600
)

// Secure creates the folder at path, if needed, owned by Administrators and
// with permissions according to access. It then checks everything already in
// the folder and returns an error wrapping ErrUntrusted if an entry is a link
// or is not owned by SYSTEM or Administrators. Subfolders that are themselves
// owned by an administrator and protected from inherited permissions are
// trusted without looking inside.
//
// A folder that exists and is owned by anyone else is replaced when it is
// empty and rejected otherwise: whoever created it may hold a handle that
// still lets them change its permissions after the owner changes.
//
// Files this process creates afterwards are owned by Administrators, so that
// a later call trusts them; see ownNewFilesAsAdministrators.
//
// The parent folder must already be trusted. The caller needs to be an
// administrator.
func Secure(path string, access Access) error {
	path = filepath.Clean(path)

	if err := ownNewFilesAsAdministrators(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), directoryPermissions); err != nil {
		return errors.Wrapf(err, "unable to create %v", filepath.Dir(path))
	}

	sddl := adminsOnlySDDL
	if access == UsersRead {
		sddl = usersReadSDDL
	}

	created, err := createSecured(path, sddl)
	if err != nil || created {
		return err
	}

	replaced, err := replaceIfUntrusted(path, sddl)
	if err != nil || replaced {
		return err
	}

	if err := applySDDL(path, sddl); err != nil {
		return err
	}

	return verifyContents(path)
}

// ownNewFilesAsAdministrators makes Administrators, rather than the user, the
// owner of every file and folder this process creates from now on. Windows
// does this by itself for most elevated processes, but not, for example, with
// User Account Control turned off; the files a run leaves in a secured folder
// would then fail the next run's check.
func ownNewFilesAsAdministrators() error {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return errors.Wrap(err, "unable to identify the Administrators group")
	}

	var token windows.Token

	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_DEFAULT, &token); err != nil {
		return errors.Wrap(err, "unable to open the process token")
	}

	defer token.Close() //nolint:errcheck

	// TOKEN_OWNER
	owner := struct{ Owner *windows.SID }{admins}

	//nolint:gosec
	if err := windows.SetTokenInformation(token, windows.TokenOwner, (*byte)(unsafe.Pointer(&owner)), uint32(unsafe.Sizeof(owner))); err != nil {
		return errors.Wrap(err, "unable to create files as Administrators; run as administrator")
	}

	return nil
}

// createSecured creates the folder at path with the security descriptor in
// sddl already in place, so that it is never open to other users. It reports
// false if path already exists.
func createSecured(path, sddl string) (bool, error) {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return false, errors.Wrap(err, "invalid security descriptor")
	}

	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, errors.Wrapf(err, "invalid path %v", path)
	}

	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))

	err = windows.CreateDirectory(name, &sa)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return false, nil
	}

	return err == nil, errors.Wrapf(err, "unable to create %v", path)
}

// replaceIfUntrusted checks the existing entry at path. If it is a folder
// that SYSTEM or Administrators own, it reports false and the folder can be
// kept. An empty folder of another owner is replaced with a new one secured
// with sddl. Anything else is an error wrapping ErrUntrusted.
func replaceIfUntrusted(path, sddl string) (bool, error) {
	// a link here would make everything below apply to another folder
	fi, err := os.Lstat(path)
	if err != nil {
		return false, errors.Wrapf(err, "unable to examine %v", path)
	}

	if isLink(fi) || !fi.IsDir() {
		return false, errors.Wrapf(ErrUntrusted, "%v is not a folder", path)
	}

	owner, err := ownerOf(path)
	if err != nil {
		return false, err
	}

	if isTrustedOwner(owner) {
		return false, nil
	}

	// removing fails if the folder holds anything, and creating it again
	// fails for as long as someone keeps the removed folder open
	if err := os.Remove(path); err != nil {
		return false, errors.Wrapf(ErrUntrusted, "%v is owned by %v and cannot be replaced (%v); remove it and try again", path, owner, err)
	}

	created, err := createSecured(path, sddl)
	if err != nil {
		return false, errors.Wrapf(ErrUntrusted, "%v was owned by %v and cannot be replaced (%v); remove it and try again", path, owner, err)
	}

	if !created {
		return false, errors.Wrapf(ErrUntrusted, "%v was created again while replacing it; remove it and try again", path)
	}

	return true, nil
}

func ownerOf(path string) (*windows.SID, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read the owner of %v", path)
	}

	owner, _, err := sd.Owner()

	return owner, errors.Wrapf(err, "unable to read the owner of %v", path)
}

// SecureFile creates the file at path, if needed, in a folder that Secure
// already secured, and lets only SYSTEM and Administrators open it, whatever
// the folder allows. A lock file needs this: a user who can merely read a
// file can lock it and so keep everyone else from taking the lock.
func SecureFile(path string) error {
	path = filepath.Clean(path)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, filePermissions)
	if err != nil {
		return errors.Wrapf(err, "unable to create %v", path)
	}

	if err := f.Close(); err != nil {
		return errors.Wrapf(err, "unable to create %v", path)
	}

	fi, err := os.Lstat(path)
	if err != nil {
		return errors.Wrapf(err, "unable to examine %v", path)
	}

	if isLink(fi) || !fi.Mode().IsRegular() {
		return errors.Wrapf(ErrUntrusted, "%v is not a file", path)
	}

	return applySDDL(path, adminsOnlyFileSDDL)
}

// applySDDL sets the owner and the protected DACL of a security descriptor
// in SDDL form on path.
func applySDDL(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return errors.Wrap(err, "invalid security descriptor")
	}

	owner, _, err := sd.Owner()
	if err != nil {
		return errors.Wrap(err, "invalid security descriptor")
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		return errors.Wrap(err, "invalid security descriptor")
	}

	// the owner goes first: an owner can always change the permissions back
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, owner, nil, nil, nil); err != nil {
		return errors.Wrapf(err, "unable to take ownership of %v", path)
	}

	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return errors.Wrapf(err, "unable to restrict access to %v", path)
	}

	return nil
}

// verifyContents checks that nothing in a secured folder predates securing it
// in a way an unprivileged user controls.
func verifyContents(root string) error {
	//nolint:wrapcheck
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return errors.Wrapf(err, "unable to examine %v", path)
		}

		if path == root {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return errors.Wrapf(err, "unable to examine %v", path)
		}

		// Nobody but SYSTEM and Administrators can add entries once the folder
		// is secured, so a link of theirs, such as the latest.log link that
		// kopia's logging keeps next to the scheduled runs' logs, is kopia's
		// own. Links are never followed.
		if isLink(fi) {
			owner, err := linkOwner(path)
			if err != nil {
				return errors.Wrapf(err, "unable to read the owner of %v", path)
			}

			if !isTrustedOwner(owner) {
				return errors.Wrapf(ErrUntrusted, "%v is a link owned by %v; remove it and try again", path, owner)
			}

			return nil
		}

		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return errors.Wrapf(err, "unable to read the owner of %v", path)
		}

		owner, _, err := sd.Owner()
		if err != nil {
			return errors.Wrapf(err, "unable to read the owner of %v", path)
		}

		if !isTrustedOwner(owner) {
			return errors.Wrapf(ErrUntrusted, "%v is owned by %v; remove it and try again", path, owner)
		}

		control, _, err := sd.Control()
		if err != nil {
			return errors.Wrapf(err, "unable to read the permissions of %v", path)
		}

		if d.IsDir() && control&windows.SE_DACL_PROTECTED != 0 {
			return filepath.SkipDir
		}

		return nil
	})
}

func isLink(fi os.FileInfo) bool {
	attrs, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if ok && attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return true
	}

	return fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0
}

// linkOwner returns the owner of a link itself, not of what it points to.
func linkOwner(path string) (*windows.SID, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	h, err := windows.CreateFile(p, windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	owner, _, err := sd.Owner()

	return owner, err //nolint:wrapcheck
}

func isTrustedOwner(sid *windows.SID) bool {
	return sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) || sid.IsWellKnown(windows.WinLocalSystemSid)
}

// Describe returns the owner and permissions of a file or folder in SDDL form.
func Describe(path string) (string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", errors.Wrapf(err, "unable to read the permissions of %v", path)
	}

	return sd.String(), nil
}
