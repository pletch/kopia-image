package disksource

import (
	"strings"

	"github.com/kopia/kopia/snapshot/image"
)

// Sources of exclusion rules, recorded in image.ExcludedFolder.Source.
const (
	SourceKopiaCache         = "kopia cache"
	SourceFilesNotToSnapshot = "FilesNotToSnapshot"
	SourceFilesNotToBackup   = "FilesNotToBackup"
)

// FilesNotToBackupSubset are the entries of the FilesNotToBackup registry
// list, which Windows and programs use to tell file backups what to skip,
// that are safe to delete after a restore: temporary files, crash reports
// and dumps, the BITS transfer queue and real-time event trace buffers.
// They are built in rather than read from the registry, where a value's
// name does not vouch for its paths. %TEMP% names the running account's
// folder only, so the temporary files are those of every profile and of
// Windows. Left out are entries whose files Windows needs, such as the
// Distributed Transaction Coordinator's log and netlogon.chg, files that are
// imaged as zeros anyway, such as pagefile.sys, and Windows Update's folder,
// which FilesNotToSnapshot names.
var FilesNotToBackupSubset = []FileSpec{
	{Source: SourceFilesNotToBackup + `\Temporary Files`, Path: `$UserProfile$\AppData\Local\Temp\*`, Recursive: true},
	{Source: SourceFilesNotToBackup + `\Temporary Files`, Path: `%SystemRoot%\Temp\*`, Recursive: true},
	{Source: SourceFilesNotToBackup + `\WER`, Path: `%ProgramData%\Microsoft\Windows\WER\*`, Recursive: true},
	{Source: SourceFilesNotToBackup + `\Kernel Dumps`, Path: `%SystemRoot%\Minidump\*`, Recursive: true},
	{Source: SourceFilesNotToBackup + `\Kernel Dumps`, Path: `%SystemRoot%\memory.dmp`},
	{Source: SourceFilesNotToBackup + `\BITS_metadata`, Path: `%ProgramData%\Microsoft\Network\Downloader\*`, Recursive: true},
	{Source: SourceFilesNotToBackup + `\ETW`, Path: `%SystemRoot%\System32\LogFiles\WMI\RtBackup\*`},
}

// Tokens of the Windows registry lists of files to leave out of backups and
// shadow copies: every user profile, and the root of every volume.
const (
	tokenUserProfile = "$userprofile$"
	tokenAllVolumes  = "$allvolumes$"

	recursiveSuffix = " /s"

	systemVolumeInformation = "System Volume Information"
)

// ExcludeRule selects files of a volume to read as zeros.
type ExcludeRule struct {
	// Source says where the rule comes from, see image.ExcludedFolder.
	Source string

	// Dir is the folder relative to the volume root, with backslash
	// separators; "" is the root.
	Dir string

	// Pattern selects files by name with * and ? wildcards, as Windows
	// does; "" selects every file.
	Pattern string

	// Recursive includes Dir's subfolders.
	Recursive bool

	// SafeAsZeros is set when the programs using the files recover from
	// them holding zeros, see image.ExcludedFolder.
	SafeAsZeros bool
}

// Folder returns the description of the rule recorded in an image.
func (r *ExcludeRule) Folder() image.ExcludedFolder {
	return image.ExcludedFolder{
		Source:      r.Source,
		Path:        r.Dir,
		Pattern:     r.Pattern,
		Recursive:   r.Recursive,
		SafeAsZeros: r.SafeAsZeros,
	}
}

// FileSpec is an entry of a Windows registry list of files to leave out, such
// as FilesNotToSnapshot: `%windir%\softwaredistribution\*.* /s` or
// `$UserProfile$\AppData\Local\Microsoft\Outlook\*.ost`.
type FileSpec struct {
	Source    string
	Path      string
	Recursive bool
}

// ParseFileSpec parses an entry of a registry list. Source names the list
// and value it comes from.
func ParseFileSpec(source, spec string) (FileSpec, bool) {
	// the leading space makes a lone "/s" a suffix too
	spec = " " + strings.TrimSpace(spec)

	recursive := false

	if strings.HasSuffix(strings.ToLower(spec), recursiveSuffix) {
		recursive = true
		spec = spec[:len(spec)-len(recursiveSuffix)]
	}

	spec = strings.TrimSpace(spec)

	if spec == "" {
		return FileSpec{}, false
	}

	return FileSpec{Source: source, Path: spec, Recursive: recursive}, true
}

// VolumeContext is what placing a FileSpec on a volume needs.
type VolumeContext struct {
	// MountPoints are where the volume is mounted, e.g. `C:\`.
	MountPoints []string

	// Profiles are the user profile folders on the volume, relative to its
	// root, e.g. `Users\me`.
	Profiles []string

	// Expand expands %VARIABLE% references.
	Expand func(string) string
}

// Rules returns the rules of a FileSpec on a volume: none when it names
// files on other volumes, one per profile for $UserProfile$. Specs with
// wildcards in folder names are not supported and give no rules, nor do
// specs inside System Volume Information, whose files only SYSTEM can
// delete after a restore and whose shadow copy storage is never imaged.
func (fs *FileSpec) Rules(vc *VolumeContext, safeAsZeros bool) []ExcludeRule {
	var dirs []string

	p := fs.Path
	lower := strings.ToLower(p)

	switch {
	case strings.HasPrefix(lower, tokenUserProfile):
		rest := strings.TrimPrefix(p[len(tokenUserProfile):], `\`)

		for _, profile := range vc.Profiles {
			dirs = append(dirs, joinVolumePath(profile, rest))
		}

	case strings.HasPrefix(lower, tokenAllVolumes):
		dirs = append(dirs, strings.TrimPrefix(p[len(tokenAllVolumes):], `\`))

	case strings.HasPrefix(p, `\`) && !strings.HasPrefix(p, `\\`):
		// a path without a drive is on every volume
		dirs = append(dirs, strings.TrimPrefix(p, `\`))

	default:
		if vc.Expand != nil {
			p = vc.Expand(p)
		}

		rel, ok := relativeToVolume(p, vc.MountPoints)
		if !ok {
			return nil
		}

		dirs = append(dirs, rel)
	}

	var rules []ExcludeRule

	for _, full := range dirs {
		dir, pattern := splitPattern(full)
		if !validRuleDir(dir) || pattern == "" {
			continue
		}

		if pattern == "*" || pattern == "*.*" {
			pattern = ""
		}

		rules = append(rules, ExcludeRule{
			Source:      fs.Source,
			Dir:         dir,
			Pattern:     pattern,
			Recursive:   fs.Recursive,
			SafeAsZeros: safeAsZeros,
		})
	}

	return rules
}

// splitPattern splits a volume path into its folder and file name pattern.
func splitPattern(p string) (dir, pattern string) {
	p = strings.Trim(p, `\`)

	i := strings.LastIndex(p, `\`)
	if i < 0 {
		return "", p
	}

	return p[:i], p[i+1:]
}

// validRuleDir reports whether a rule folder is a plain volume path: no
// wildcards, no "." or "..", and not in System Volume Information.
func validRuleDir(dir string) bool {
	if dir == "" {
		return true
	}

	if strings.ContainsAny(dir, "*?") || !image.IsVolumeRelativePath(dir) {
		return false
	}

	first, _, _ := strings.Cut(dir, `\`)

	return !strings.EqualFold(first, systemVolumeInformation)
}

func joinVolumePath(dir, rest string) string {
	switch {
	case dir == "":
		return rest
	case rest == "":
		return dir
	default:
		return dir + `\` + rest
	}
}

// MatchPattern reports whether a file name matches a pattern with * and ?
// wildcards, ignoring case as Windows does. "" and "*" match every name.
func MatchPattern(pattern, name string) bool {
	if pattern == "" {
		return true
	}

	return matchWildcards([]rune(strings.ToLower(pattern)), []rune(strings.ToLower(name)))
}

func matchWildcards(p, s []rune) bool {
	// the position after the last * and the name position it is matched at
	star, retry := -1, 0

	for i, j := 0, 0; j < len(s); {
		switch {
		case i < len(p) && (p[i] == '?' || p[i] == s[j]):
			i++
			j++
		case i < len(p) && p[i] == '*':
			star, retry = i+1, j
			i++
		case star >= 0:
			retry++
			i, j = star, retry
		default:
			return false
		}

		if j == len(s) {
			for i < len(p) && p[i] == '*' {
				i++
			}

			return i == len(p)
		}
	}

	for _, c := range p {
		if c != '*' {
			return false
		}
	}

	return true
}

// relativeToVolume returns an absolute Windows path relative to the volume
// mounted at mountPoints, using the longest mount point that contains it.
func relativeToVolume(path string, mountPoints []string) (string, bool) {
	sep := `\`
	p := strings.TrimRight(path, sep)
	lp := strings.ToLower(p) + sep

	best := -1

	for _, mp := range mountPoints {
		m := strings.ToLower(strings.TrimRight(mp, sep)) + sep

		if strings.HasPrefix(lp, m) && len(m) > best {
			best = len(m)
		}
	}

	if best < 0 {
		return "", false
	}

	return strings.TrimRight((p + sep)[best:], sep), true
}
