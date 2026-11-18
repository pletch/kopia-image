package disksource

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()

	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o700))
	}
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
}

func TestKopiaCacheFolders(t *testing.T) {
	tmp := t.TempDir()
	root := tmp + `\`

	const (
		roaming = `Users\alice\AppData\Roaming\kopia`
		local   = `Users\alice\AppData\Local\kopia`
	)

	mkdirs(t, tmp,
		roaming,
		// a default cache, named after its configuration file
		local+`\0123456789abcdef\server-contents`,
		local+`\0123456789abcdef\indexes`,
		// a cache configured by relative path
		local+`\aaaaaaaaaaaaaaaa\contents`,
		local+`\aaaaaaaaaaaaaaaa\metadata`,
		local+`\aaaaaaaaaaaaaaaa\indexes`,
		local+`\aaaaaaaaaaaaaaaa\something-else`,
		// not a cache: logs, even with a folder named like a cache's
		local+`\cli-logs\contents`,
		// a cache configured by absolute path, on this volume
		`caches\k1\blob-list`,
		`caches\k1\own-writes`,
		// the running kopia's own
		`ProgramData\kopia\image\cache\server-contents`,
		// a system account's default cache
		`Windows\System32\config\systemprofile\AppData\Local\kopia\fedcba9876543210\index-blobs`,
	)

	writeFile(t, tmp, roaming+`\repository.config`, `{"caching":{"cacheDirectory":"..\\..\\Local\\kopia\\aaaaaaaaaaaaaaaa"}}`)
	writeFile(t, tmp, roaming+`\other.config`, `{"caching":{"cacheDirectory":"X:\\caches\\k1"}}`)
	writeFile(t, tmp, roaming+`\elsewhere.config`, `{"caching":{"cacheDirectory":"Y:\\caches\\k2"}}`)
	writeFile(t, tmp, roaming+`\broken.config`, `{"caching":`)
	writeFile(t, tmp, roaming+`\nocache.config`, `{"storage":{}}`)
	writeFile(t, tmp, roaming+`\notes.txt`, `{"caching":{"cacheDirectory":"X:\\notes"}}`)

	// profile junctions are not followed
	out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", filepath.Join(tmp, `Users\Default User`), filepath.Join(tmp, `Users\alice`)).CombinedOutput()
	require.NoError(t, err, string(out))

	got := KopiaCacheFolders(root, []string{`X:\`}, []string{`x:\ProgramData\kopia\image\cache`, `Z:\elsewhere`})

	require.Equal(t, []string{
		`ProgramData\kopia\image\cache\server-contents`,
		local + `\0123456789abcdef\server-contents`,
		local + `\aaaaaaaaaaaaaaaa\contents`,
		local + `\aaaaaaaaaaaaaaaa\metadata`,
		`Windows\System32\config\systemprofile\AppData\Local\kopia\fedcba9876543210\index-blobs`,
		`caches\k1\blob-list`,
		`caches\k1\own-writes`,
	}, got)
}
