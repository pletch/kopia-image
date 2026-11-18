package disksource

import (
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kopia/kopia/internal/ospath"
)

// kopiaCacheSubdirs are the folders of a kopia cache directory whose files
// kopia either verifies when it reads them, discarding and refetching what
// fails, or does not read at all, so they can be imaged as zeros: a restored
// computer's kopia recovers from what it finds there. "indexes", the committed
// index cache, is opened without verification and is always imaged.
var kopiaCacheSubdirs = []string{"server-contents", "contents", "metadata", "index-blobs", "blob-list", "own-writes"}

// profileDirs are the profile folders of the system accounts, besides the
// folders in Users.
var profileDirs = []string{
	`Windows\System32\config\systemprofile`,
	`Windows\ServiceProfiles\LocalService`,
	`Windows\ServiceProfiles\NetworkService`,
}

const (
	kopiaConfigDir = `AppData\Roaming\kopia`
	kopiaCacheDir  = `AppData\Local\kopia`

	// the default cache directory of a repository is named after 8 bytes of
	// the hash of its configuration file, see repo.SetCachingOptions.
	defaultCacheDirNameLen = 16

	maxConfigSize = 1 << 20
)

// KopiaCacheFolders returns the folders of the kopia caches on a volume whose
// files can be imaged as zeros, relative to the volume root and sorted. root
// is the path of the volume or its shadow copy, ending in a backslash;
// mountPoints are the paths the volume is mounted at, such as `C:\`, to place
// cache directories given by absolute path; extra are more cache
// directories by absolute path, such as the running kopia's own.
//
// The caches are those of the kopia configuration files in each profile's
// AppData\Roaming\kopia, kopia's default cache directories in each
// profile's AppData\Local\kopia, and extra.
func KopiaCacheFolders(root string, mountPoints, extra []string) []string {
	var caches []string

	for _, dir := range extra {
		if rel, ok := relativeToVolume(dir, mountPoints); ok {
			caches = append(caches, rel)
		}
	}

	for _, profile := range profiles(root) {
		caches = append(caches, configuredCaches(root, profile+`\`+kopiaConfigDir, mountPoints)...)
		caches = append(caches, defaultCaches(root, profile+`\`+kopiaCacheDir)...)
	}

	seen := map[string]bool{}

	var folders []string

	for _, c := range caches {
		c = strings.Trim(filepath.Clean(c), `\`)
		if c == "" || c == "." || strings.HasPrefix(c, "..") || seen[strings.ToLower(c)] {
			continue
		}

		seen[strings.ToLower(c)] = true

		for _, sub := range kopiaCacheSubdirs {
			if isPlainDir(root + c + `\` + sub) {
				folders = append(folders, c+`\`+sub)
			}
		}
	}

	slices.Sort(folders)

	return folders
}

// profiles returns the profile folders on the volume, relative to its root.
func profiles(root string) []string {
	var out []string

	if entries, err := os.ReadDir(root + "Users"); err == nil {
		for _, e := range entries {
			// "All Users" and "Default User" are junctions
			if e.Type() == fs.ModeDir {
				out = append(out, `Users\`+e.Name())
			}
		}
	}

	for _, p := range profileDirs {
		if isPlainDir(root + p) {
			out = append(out, p)
		}
	}

	return out
}

// configuredCaches returns the cache directories of the kopia configuration
// files in configDir, relative to the volume root.
func configuredCaches(root, configDir string, mountPoints []string) []string {
	entries, err := os.ReadDir(root + configDir)
	if err != nil {
		return nil
	}

	var out []string

	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.EqualFold(filepath.Ext(e.Name()), ".config") {
			continue
		}

		dir := cacheDirectoryOf(root + configDir + `\` + e.Name())

		switch {
		case dir == "":
		case ospath.IsAbs(dir):
			if rel, ok := relativeToVolume(dir, mountPoints); ok {
				out = append(out, rel)
			}
		case filepath.VolumeName(dir) == "" && !strings.HasPrefix(dir, `\`):
			// relative to the configuration file, see repo.LoadConfigFromFile
			out = append(out, configDir+`\`+dir)
		}
	}

	return out
}

// cacheDirectoryOf returns the cache directory of a kopia configuration file
// as written in it, or "".
func cacheDirectoryOf(path string) string {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck

	var cfg struct {
		Caching *struct {
			CacheDirectory string `json:"cacheDirectory"`
		} `json:"caching"`
	}

	if err := json.NewDecoder(io.LimitReader(f, maxConfigSize)).Decode(&cfg); err != nil || cfg.Caching == nil {
		return ""
	}

	return cfg.Caching.CacheDirectory
}

// defaultCaches returns the default cache directories in cacheParent,
// relative to the volume root.
func defaultCaches(root, cacheParent string) []string {
	entries, err := os.ReadDir(root + cacheParent)
	if err != nil {
		return nil
	}

	var out []string

	for _, e := range entries {
		if e.Type() == fs.ModeDir && isHex(e.Name(), defaultCacheDirNameLen) {
			out = append(out, cacheParent+`\`+e.Name())
		}
	}

	return out
}

// isPlainDir reports whether path is a directory and not a reparse point.
func isPlainDir(path string) bool {
	fi, err := os.Lstat(path)

	return err == nil && fi.Mode().Type() == fs.ModeDir
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}

	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}

	return true
}
