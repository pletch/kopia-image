package ntfsfs

import "strings"

// protectedPaths are the credential stores of a Windows volume, as paths in
// the volume in lower case. On the original disk only administrators and
// SYSTEM can read them, but a volume image shows its files without their
// permissions, so they are hidden unless the process runs as an
// administrator: anyone who can read them can recover password hashes, LSA
// secrets and the machine's DPAPI keys.
//
//nolint:gochecknoglobals
var protectedPaths = map[string]bool{
	// registry hives: SAM (password hashes), SECURITY (LSA secrets,
	// cached domain logons), SYSTEM (the boot key that decrypts them), and
	// the profile of SYSTEM
	"windows/system32/config": true,

	// the machine's DPAPI master keys
	"windows/system32/microsoft/protect": true,

	// profiles of the LocalService and NetworkService accounts
	"windows/serviceprofiles": true,

	// backups of the registry hives on old versions of Windows
	"windows/repair": true,

	// the Active Directory database of a domain controller
	"windows/ntds": true,

	// memory written to disk, which holds secrets of running programs
	"hiberfil.sys": true,
	"pagefile.sys": true,
	"swapfile.sys": true,
}

// childPath returns the path of an entry named name in the directory at
// parent: names joined by "/" in lower case, as NTFS compares them.
func childPath(parent, name string) string {
	name = strings.ToLower(name)

	if parent == "" {
		return name
	}

	return parent + "/" + name
}

// isProtectedPath tells whether an entry is a protected credential store.
// Entries inside a protected directory are never listed, so only the
// directory itself needs checking.
func isProtectedPath(path string) bool {
	return protectedPaths[path]
}
