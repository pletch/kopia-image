package securedir_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/snapshot/image/securedir"
)

// requireAdministrator skips tests that change owners, which needs elevation.
func requireAdministrator(t *testing.T) {
	t.Helper()

	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("needs an elevated process")
	}
}

// giveToCurrentUser makes the current user, rather than Administrators, the
// owner of path, as if an unelevated process had created it.
func giveToCurrentUser(t *testing.T, path string) {
	t.Helper()

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)

	require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil))
}

func TestSecure_setsOwnerAndPermissions(t *testing.T) {
	requireAdministrator(t)

	dir := filepath.Join(t.TempDir(), "a", "b")

	require.NoError(t, securedir.Secure(dir, securedir.AdminsOnly))

	sddl, err := securedir.Describe(dir)
	require.NoError(t, err)
	require.Contains(t, sddl, "O:BA")
	require.Contains(t, sddl, "D:P")
	require.Contains(t, sddl, "(A;OICI;FA;;;SY)")
	require.Contains(t, sddl, "(A;OICI;FA;;;BA)")
	require.NotContains(t, sddl, ";;;BU)")

	require.NoError(t, securedir.Secure(dir, securedir.UsersRead))

	sddl, err = securedir.Describe(dir)
	require.NoError(t, err)
	require.Contains(t, sddl, "(A;OICI;0x1200a9;;;BU)")

	// files created in it afterwards are trusted
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "cache", "deep"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cache", "deep", "f"), []byte("x"), 0o600))
	require.NoError(t, securedir.Secure(dir, securedir.UsersRead))
}

// ownerOf returns the owner of path in SDDL form, e.g. "O:BA".
func ownerOf(t *testing.T, path string) string {
	t.Helper()

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	require.NoError(t, err)

	return sd.String()
}

func TestSecure_filesCreatedAfterwardsBelongToAdministrators(t *testing.T) {
	requireAdministrator(t)

	dir := filepath.Join(t.TempDir(), "secured")
	require.NoError(t, securedir.Secure(dir, securedir.UsersRead))

	// created the way the image commands create their state, with no owner given
	sub := filepath.Join(dir, "bin")
	file := filepath.Join(sub, "state.json")

	require.NoError(t, os.MkdirAll(sub, 0o700))
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	require.Equal(t, "O:BA", ownerOf(t, sub))
	require.Equal(t, "O:BA", ownerOf(t, file))

	// so the next run trusts them
	require.NoError(t, securedir.Secure(dir, securedir.UsersRead))
}

func TestSecure_replacesEmptyFolderOfAnotherOwner(t *testing.T) {
	requireAdministrator(t)

	dir := filepath.Join(t.TempDir(), "planted")
	require.NoError(t, os.Mkdir(dir, 0o700))
	giveToCurrentUser(t, dir)

	require.NoError(t, securedir.Secure(dir, securedir.AdminsOnly))

	sddl, err := securedir.Describe(dir)
	require.NoError(t, err)
	require.Contains(t, sddl, "O:BA")
	require.Contains(t, sddl, "D:P")
}

func TestSecure_rejectsFolderOfAnotherOwnerWithContents(t *testing.T) {
	requireAdministrator(t)

	dir := filepath.Join(t.TempDir(), "planted")
	file := filepath.Join(dir, "kopia.exe")

	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	giveToCurrentUser(t, dir)

	require.ErrorIs(t, securedir.Secure(dir, securedir.AdminsOnly), securedir.ErrUntrusted)

	// it is left as it was, for the administrator to look at
	require.FileExists(t, file)
	require.NotEqual(t, "O:BA", ownerOf(t, dir))
}

func TestSecure_rejectsFolderOfAnotherOwnerThatIsHeldOpen(t *testing.T) {
	requireAdministrator(t)

	dir := filepath.Join(t.TempDir(), "planted")
	require.NoError(t, os.Mkdir(dir, 0o700))
	giveToCurrentUser(t, dir)

	// whoever planted the folder keeps a handle to it
	h, err := os.Open(dir)
	require.NoError(t, err)

	err = securedir.Secure(dir, securedir.AdminsOnly)

	require.NoError(t, h.Close())
	require.ErrorIs(t, err, securedir.ErrUntrusted)
}

func TestSecure_rejectsEntriesOfOtherOwners(t *testing.T) {
	requireAdministrator(t)

	for _, name := range []string{"state.json.tmp", filepath.Join("cache", "nested")} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "secured")
			planted := filepath.Join(dir, name)

			require.NoError(t, securedir.Secure(dir, securedir.AdminsOnly))
			require.NoError(t, os.MkdirAll(filepath.Dir(planted), 0o700))
			require.NoError(t, os.WriteFile(planted, []byte("x"), 0o600))
			giveToCurrentUser(t, planted)

			require.ErrorIs(t, securedir.Secure(dir, securedir.AdminsOnly), securedir.ErrUntrusted)

			require.NoError(t, os.Remove(planted))
			require.NoError(t, securedir.Secure(dir, securedir.AdminsOnly))
		})
	}
}

func TestSecure_rejectsLinks(t *testing.T) {
	requireAdministrator(t)

	target := t.TempDir()

	t.Run("the folder itself", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		makeJunction(t, link, target)

		require.ErrorIs(t, securedir.Secure(link, securedir.AdminsOnly), securedir.ErrUntrusted)
	})

	t.Run("an entry", func(t *testing.T) {
		dir := t.TempDir()
		makeJunction(t, filepath.Join(dir, "link"), target)

		require.ErrorIs(t, securedir.Secure(dir, securedir.AdminsOnly), securedir.ErrUntrusted)
	})
}

func makeJunction(t *testing.T, link, target string) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	require.NoError(t, err, "mklink: %s", out)
}

func TestSecureFile(t *testing.T) {
	requireAdministrator(t)

	dir := t.TempDir()
	require.NoError(t, securedir.Secure(dir, securedir.UsersRead))

	lock := filepath.Join(dir, "state.json.lock")

	// creates the file, and is repeatable on an existing one
	for range 2 {
		require.NoError(t, securedir.SecureFile(lock))

		sddl, err := securedir.Describe(lock)
		require.NoError(t, err)
		require.Contains(t, sddl, "O:BA")
		require.Contains(t, sddl, "D:P")
		require.NotContains(t, sddl, ";;;BU)", "users can read the folder but not this file")
	}

	// the folder still passes its own check with the restricted file in it
	require.NoError(t, securedir.Secure(dir, securedir.UsersRead))

	require.Error(t, securedir.SecureFile(dir), "a folder is not a file")
}
