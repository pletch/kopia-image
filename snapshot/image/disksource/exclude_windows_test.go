package disksource

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/ntfs"
)

func junction(t *testing.T, link, target string) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestMatchingFiles(t *testing.T) {
	tmp := t.TempDir()
	root := tmp + `\`

	mkdirs(t, tmp, `data\sub\deeper`, `elsewhere`)

	for _, f := range []string{`data\a.ost`, `data\b.OST`, `data\c.txt`, `data\sub\d.ost`, `data\sub\deeper\e.ost`, `elsewhere\f.ost`} {
		writeFile(t, tmp, f, "x")
	}

	// a junction inside the folder is not followed
	junction(t, filepath.Join(tmp, `data\link`), filepath.Join(tmp, "elsewhere"))

	// nor one on the way to it
	junction(t, filepath.Join(tmp, "via"), filepath.Join(tmp, "data"))

	match := func(r ExcludeRule) []string {
		files, err := matchingFiles(root, &r)
		require.NoError(t, err)

		return files
	}

	require.ElementsMatch(t, []string{`data\a.ost`, `data\b.OST`}, match(ExcludeRule{Dir: "data", Pattern: "*.ost"}))
	require.ElementsMatch(t, []string{`data\a.ost`, `data\b.OST`, `data\sub\d.ost`, `data\sub\deeper\e.ost`},
		match(ExcludeRule{Dir: "data", Pattern: "*.ost", Recursive: true}))
	require.ElementsMatch(t, []string{`data\a.ost`, `data\b.OST`, `data\c.txt`, `data\sub\d.ost`, `data\sub\deeper\e.ost`},
		match(ExcludeRule{Dir: "data", Recursive: true}))

	require.Empty(t, match(ExcludeRule{Dir: `via`, Recursive: true}), "the folder is reached through a junction")
	require.Empty(t, match(ExcludeRule{Dir: `via\sub`, Recursive: true}))
	require.Empty(t, match(ExcludeRule{Dir: `missing`, Recursive: true}))
}

func TestExcludeFiles(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.VolumeName(tmp) + `\`
	rel := strings.TrimPrefix(tmp, root)

	mkdirs(t, tmp, `Outlook`, `Cache\sub`)

	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i)
	}

	for _, f := range []string{`Outlook\mail.ost`, `Cache\sub\entry.bin`} {
		require.NoError(t, os.WriteFile(filepath.Join(tmp, f), big, 0o600))
	}

	writeFile(t, tmp, `Outlook\keep.pst`, strings.Repeat("keep", 100_000))

	saved := readFilesNotToSnapshot
	t.Cleanup(func() { readFilesNotToSnapshot = saved })

	readFilesNotToSnapshot = func() ([]FileSpec, error) {
		return []FileSpec{
			{Source: `FilesNotToSnapshot\OST`, Path: `$AllVolumes$\` + rel + `\Outlook\*.ost`},
			{Source: `FilesNotToSnapshot\Cache`, Path: `$AllVolumes$\` + rel + `\Cache\*`, Recursive: true},
			// the same file again: excluded once
			{Source: `FilesNotToSnapshot\Again`, Path: `$AllVolumes$\` + rel + `\Outlook\mail.ost`},
		}, nil
	}

	vol := &ntfs.Volume{Data: ntfs.VolumeData{ClusterSize: 4096}}

	var logged []string

	excludeFiles(vol, root, &VolumeInfo{MountPoints: []string{root}}, SessionOptions{ExcludeFilesNotToSnapshot: true},
		func(format string, args ...any) { logged = append(logged, format) })

	require.ElementsMatch(t, []string{rel + `\Outlook\mail.ost`, rel + `\Cache\sub\entry.bin`}, vol.ExcludedFiles)
	require.Len(t, vol.Excluded, 2)

	byPath := map[string]image.ExcludedFolder{}
	for _, f := range vol.Excluded {
		byPath[f.Path] = f
	}

	ost := byPath[rel+`\Outlook`]
	require.Equal(t, `FilesNotToSnapshot\OST`, ost.Source)
	require.Equal(t, "*.ost", ost.Pattern)
	require.False(t, ost.Recursive)
	require.False(t, ost.SafeAsZeros, "a restore must delete them")
	require.Equal(t, int64(1), ost.Files)
	require.GreaterOrEqual(t, ost.Bytes, int64(1<<20))

	cache := byPath[rel+`\Cache`]
	require.Empty(t, cache.Pattern)
	require.True(t, cache.Recursive)

	require.Equal(t, image.FormatVersionDeletesExcluded, image.RequiredFormatVersion([]image.Volume{{Excluded: vol.Excluded}}))
	require.NotEmpty(t, logged)
}

func TestExclusionRules_options(t *testing.T) {
	tmp := t.TempDir()
	mkdirs(t, tmp, `Users\alice\AppData\Local\Temp`, `Users\alice\AppData\Local\kopia\0123456789abcdef\server-contents`)

	saved := readFilesNotToSnapshot
	t.Cleanup(func() { readFilesNotToSnapshot = saved })

	readFilesNotToSnapshot = func() ([]FileSpec, error) {
		return []FileSpec{{Source: `FilesNotToSnapshot\X`, Path: `$UserProfile$\x.ost`}}, nil
	}

	// a volume mounted where %SystemRoot% is not: only its profiles count
	v := &VolumeInfo{MountPoints: []string{`X:\`}}

	sources := func(opt SessionOptions) []string {
		var out []string

		for _, r := range exclusionRules(tmp+`\`, v, opt, t.Logf) {
			out = append(out, r.Source+" "+r.Dir)
		}

		return out
	}

	require.Empty(t, sources(SessionOptions{}))
	require.Equal(t, []string{`kopia cache Users\alice\AppData\Local\kopia\0123456789abcdef\server-contents`},
		sources(SessionOptions{ExcludeKopiaCaches: true}))
	require.Equal(t, []string{`FilesNotToSnapshot\X Users\alice`},
		sources(SessionOptions{ExcludeFilesNotToSnapshot: true}))
	require.Equal(t, []string{`FilesNotToBackup\Temporary Files Users\alice\AppData\Local\Temp`},
		sources(SessionOptions{ExcludeFilesNotToBackup: true}))
}
