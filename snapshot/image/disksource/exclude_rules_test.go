package disksource

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseFileSpec(t *testing.T) {
	for _, tc := range []struct {
		in        string
		path      string
		recursive bool
		ok        bool
	}{
		{`%windir%\softwaredistribution\*.* /s`, `%windir%\softwaredistribution\*.*`, true, true},
		{`$UserProfile$\AppData\Local\Microsoft\Outlook\*.ost`, `$UserProfile$\AppData\Local\Microsoft\Outlook\*.ost`, false, true},
		{`  \Pagefile.sys  `, `\Pagefile.sys`, false, true},
		{`%TEMP%\* /S`, `%TEMP%\*`, true, true},
		{`   `, ``, false, false},
		{` /s`, ``, false, false},
	} {
		got, ok := ParseFileSpec("list\\value", tc.in)
		require.Equal(t, tc.ok, ok, tc.in)

		if ok {
			require.Equal(t, FileSpec{Source: "list\\value", Path: tc.path, Recursive: tc.recursive}, got, tc.in)
		}
	}
}

func TestFileSpecRules(t *testing.T) {
	vc := &VolumeContext{
		MountPoints: []string{`C:\`},
		Profiles:    []string{`Users\alice`, `Users\bob`},
		Expand: func(s string) string {
			return strings.NewReplacer("%windir%", `C:\Windows`, "%ProgramData%", `C:\ProgramData`).Replace(s)
		},
	}

	rules := func(path string, recursive bool) []ExcludeRule {
		fs := FileSpec{Source: "src", Path: path, Recursive: recursive}
		return fs.Rules(vc, false)
	}

	require.Equal(t, []ExcludeRule{
		{Source: "src", Dir: `Windows\softwaredistribution`, Recursive: true},
	}, rules(`%windir%\softwaredistribution\*.*`, true), "*.* is every file")

	require.Equal(t, []ExcludeRule{
		{Source: "src", Dir: `Users\alice\AppData\Local\Microsoft\Outlook`, Pattern: "*.ost"},
		{Source: "src", Dir: `Users\bob\AppData\Local\Microsoft\Outlook`, Pattern: "*.ost"},
	}, rules(`$UserProfile$\AppData\Local\Microsoft\Outlook\*.ost`, false), "one rule per profile")

	require.Equal(t, []ExcludeRule{
		{Source: "src", Dir: ``, Pattern: "pagefile.sys"},
	}, rules(`\pagefile.sys`, false), "a path without a drive is on every volume")

	require.Equal(t, []ExcludeRule{
		{Source: "src", Dir: `ProgramData\Data`, Pattern: "x?.log"},
	}, rules(`$AllVolumes$\ProgramData\Data\x?.log`, false))

	require.Equal(t, []ExcludeRule{
		{Source: "src", Dir: `ProgramData\Microsoft\Windows\WER`, Recursive: true},
	}, rules(`%ProgramData%\Microsoft\Windows\WER\*`, true))

	for _, skipped := range []string{
		`D:\elsewhere\*.*`,                               // another volume
		`%unknown%\x.txt`,                                // not expanded, not absolute
		`\System Volume Information\Heat\*.*`,            // only SYSTEM can delete these
		`$AllVolumes$\system volume information\FVE.{x}`, // in any case
		`$UserProfile$\*\Cache\*`,                        // a wildcard folder
		`C:\Windows\..\x.txt`,                            // not a plain path
		`C:\`,                                            // no file pattern
	} {
		require.Empty(t, rules(skipped, true), skipped)
	}

	safe := FileSpec{Source: "kopia", Path: `C:\cache\*`}
	require.True(t, safe.Rules(vc, true)[0].SafeAsZeros)

	require.Equal(t, "kopia", safe.Rules(vc, true)[0].Folder().Source)
}

func TestMatchPattern(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"", "anything", true},
		{"*", "anything", true},
		{"*.ost", "Mail.OST", true},
		{"*.ost", "mail.ost.bak", false},
		{"*.ost", "ost", false},
		{"x?.log", "x1.log", true},
		{"x?.log", "x.log", false},
		{"x?.log", "x12.log", false},
		{"*abc*", "zzabczz", true},
		{"*abc*", "zzabzz", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXbY", false},
		{"pagefile.sys", "PAGEFILE.SYS", true},
		{"[x].txt", "[x].txt", true},
		{"[x].txt", "x.txt", false},
	} {
		require.Equal(t, tc.want, MatchPattern(tc.pattern, tc.name), "%q %q", tc.pattern, tc.name)
	}
}

func TestRelativeToVolume(t *testing.T) {
	for _, tc := range []struct {
		path   string
		mounts []string
		want   string
		ok     bool
	}{
		{`C:\Users\me\x`, []string{`C:\`}, `Users\me\x`, true},
		{`c:\users\me\x\`, []string{`C:\`}, `users\me\x`, true},
		{`D:\mnt\data\cache`, []string{`D:\mnt\data\`, `E:\`}, `cache`, true},
		{`D:\mnt\database`, []string{`D:\mnt\data\`}, ``, false},
		{`C:\`, []string{`C:\`}, ``, true},
		{`E:\x`, []string{`C:\`}, ``, false},
		{`E:\x`, nil, ``, false},
	} {
		got, ok := relativeToVolume(tc.path, tc.mounts)
		require.Equal(t, tc.ok, ok, tc.path)
		require.Equal(t, tc.want, got, tc.path)
	}
}

func TestFilesNotToBackupSubset(t *testing.T) {
	vc := &VolumeContext{
		MountPoints: []string{`C:\`},
		Profiles:    []string{`Users\alice`, `Windows\System32\config\systemprofile`},
		Expand: func(s string) string {
			return strings.NewReplacer("%SystemRoot%", `C:\Windows`, "%ProgramData%", `C:\ProgramData`).Replace(s)
		},
	}

	var got []ExcludeRule

	for i := range FilesNotToBackupSubset {
		got = append(got, FilesNotToBackupSubset[i].Rules(vc, false)...)
	}

	const src = SourceFilesNotToBackup + `\`

	require.Equal(t, []ExcludeRule{
		{Source: src + "Temporary Files", Dir: `Users\alice\AppData\Local\Temp`, Recursive: true},
		{Source: src + "Temporary Files", Dir: `Windows\System32\config\systemprofile\AppData\Local\Temp`, Recursive: true},
		{Source: src + "Temporary Files", Dir: `Windows\Temp`, Recursive: true},
		{Source: src + "WER", Dir: `ProgramData\Microsoft\Windows\WER`, Recursive: true},
		{Source: src + "Kernel Dumps", Dir: `Windows\Minidump`, Recursive: true},
		{Source: src + "Kernel Dumps", Dir: `Windows`, Pattern: "memory.dmp"},
		{Source: src + "BITS_metadata", Dir: `ProgramData\Microsoft\Network\Downloader`, Recursive: true},
		{Source: src + "ETW", Dir: `Windows\System32\LogFiles\WMI\RtBackup`},
	}, got)

	// none is safe as zeros: a restore must delete them
	for _, r := range got {
		require.False(t, r.SafeAsZeros, r.Dir)
	}

	// on a data volume, only the profiles there count
	vc.MountPoints = []string{`D:\`}
	vc.Profiles = nil

	for i := range FilesNotToBackupSubset {
		require.Empty(t, FilesNotToBackupSubset[i].Rules(vc, false))
	}
}

func TestCommonCaches(t *testing.T) {
	vc := &VolumeContext{
		MountPoints: []string{`C:\`},
		Profiles:    []string{`Users\alice`},
		Expand:      func(s string) string { return strings.ReplaceAll(s, "%SystemRoot%", `C:\Windows`) },
	}

	dirs := map[string]string{}

	for _, spec := range CommonCaches() {
		for _, r := range spec.Rules(vc, false) {
			require.False(t, r.SafeAsZeros)
			require.True(t, r.Recursive)
			require.Empty(t, r.Pattern)

			dirs[r.Dir] = r.Source
		}
	}

	require.Equal(t, `common cache\Chrome`, dirs[`Users\alice\AppData\Local\Google\Chrome\User Data\*\Cache`])
	require.Equal(t, `common cache\Edge`, dirs[`Users\alice\AppData\Local\Microsoft\Edge\User Data\*\Code Cache`])
	require.Equal(t, `common cache\Brave`, dirs[`Users\alice\AppData\Local\BraveSoftware\Brave-Browser\User Data\GrShaderCache`])
	require.Equal(t, `common cache\Firefox`, dirs[`Users\alice\AppData\Local\Mozilla\Firefox\Profiles\*\cache2`])
	require.Equal(t, `common cache\Windows Update`, dirs[`Windows\SoftwareDistribution\Download`])
	require.Equal(t, `common cache\Delivery Optimization`, dirs[`Windows\ServiceProfiles\NetworkService\AppData\Local\Microsoft\Windows\DeliveryOptimization\Cache`])
	require.Len(t, dirs, 5*6+3)
}

func TestAnyFolder(t *testing.T) {
	vc := &VolumeContext{MountPoints: []string{`C:\`}}

	rules := func(path string, anyFolder bool) []ExcludeRule {
		fs := FileSpec{Source: "s", Path: path, Recursive: true, AnyFolder: anyFolder}
		return fs.Rules(vc, false)
	}

	require.Equal(t, `a\*\b`, rules(`C:\a\*\b\*`, true)[0].Dir)
	require.Empty(t, rules(`C:\a\*\b\*`, false), "registry specs have no wildcard folders")
	require.Empty(t, rules(`C:\a\Profile*\b\*`, true), "only whole names")
	require.Empty(t, rules(`C:\a\?\b\*`, true))
}
