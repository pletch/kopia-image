package disksource

import (
	stderrors "errors"
	"io/fs"
	"os"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows/registry"

	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/snapshot/image/ntfs"
)

// filesNotToSnapshotKey lists the files that Windows and installed programs
// leave out of shadow copies made for backups, such as Outlook's mailbox
// caches; each value is a list of FileSpec entries.
const filesNotToSnapshotKey = `SYSTEM\CurrentControlSet\Control\BackupRestore\FilesNotToSnapshot`

// excludeFiles reads the files selected by the session's exclusion rules as
// zeros. Leaving them in only makes the image larger, so failures are
// reported and imaging goes on.
func excludeFiles(vol *ntfs.Volume, root string, v *VolumeInfo, opt SessionOptions, logf func(string, ...any)) {
	rules := exclusionRules(root, v, opt, logf)

	seen := map[string]bool{}

	for i := range rules {
		r := &rules[i]

		files, err := matchingFiles(root, r)
		if err != nil {
			logf("%v: some files of %v are imaged: %v", describeVolume(v), r.Source, err)
		}

		// a file selected by two rules is excluded by the first
		files = slicesDeleteSeen(files, seen)
		if len(files) == 0 {
			continue
		}

		if err := vol.ExcludeFiles(root, r.Folder(), files); err != nil {
			logf("%v: some files of %v are imaged: %v", describeVolume(v), r.Source, err)
		}
	}

	type total struct{ files, bytes int64 }

	bySource := map[string]*total{}

	var sources []string

	for _, f := range vol.Excluded {
		source, _, _ := strings.Cut(f.Source, `\`)

		t := bySource[source]
		if t == nil {
			t = &total{}
			bySource[source] = t
			sources = append(sources, source)
		}

		t.files += f.Files
		t.bytes += f.Bytes
	}

	for _, s := range sources {
		logf("%v: %v files (%v) of %v are read as zeros", describeVolume(v), bySource[s].files, units.BytesString(bySource[s].bytes), s)
	}
}

// exclusionRules returns the rules of the session for a volume.
func exclusionRules(root string, v *VolumeInfo, opt SessionOptions, logf func(string, ...any)) []ExcludeRule {
	var rules []ExcludeRule

	if opt.ExcludeKopiaCaches {
		// kopia discards and refetches cache files that hold zeros
		for _, f := range KopiaCacheFolders(root, v.MountPoints, opt.KopiaCacheDirs) {
			rules = append(rules, ExcludeRule{Source: SourceKopiaCache, Dir: f, Recursive: true, SafeAsZeros: true})
		}
	}

	if opt.ExcludeFilesNotToSnapshot {
		specs, err := readFilesNotToSnapshot()
		if err != nil {
			logf("Unable to read the files that Windows leaves out of shadow copies: %v", err)
		}

		for i := range specs {
			rules = append(rules, specs[i].Rules(volumeContext(root, v), false)...)
		}
	}

	if opt.ExcludeFilesNotToBackup {
		for i := range FilesNotToBackupSubset {
			rules = append(rules, FilesNotToBackupSubset[i].Rules(volumeContext(root, v), false)...)
		}
	}

	return rules
}

func volumeContext(root string, v *VolumeInfo) *VolumeContext {
	return &VolumeContext{MountPoints: v.MountPoints, Profiles: profiles(root), Expand: expandEnv}
}

// readFilesNotToSnapshot reads the FilesNotToSnapshot list; tests replace it.
var readFilesNotToSnapshot = func() ([]FileSpec, error) {
	return registryFileSpecs(filesNotToSnapshotKey, SourceFilesNotToSnapshot)
}

// matchingFiles returns the files of a volume that a rule selects, relative
// to the volume root. root is the volume root path ending in a backslash,
// normally a shadow copy device. Only plain folders are walked: a junction
// or other reparse point on the way to or below the rule's folder is not
// followed, as in a shadow copy it leads out of it, to the live volume.
func matchingFiles(root string, r *ExcludeRule) ([]string, error) {
	if !isPlainPath(root, r.Dir) {
		return nil, nil
	}

	var files []string

	err := walkMatching(root, r.Dir, r.Pattern, r.Recursive, &files)

	return files, err
}

func walkMatching(root, dir, pattern string, recursive bool, files *[]string) error {
	entries, err := os.ReadDir(root + dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return errors.Wrapf(err, "unable to list %q", dir)
	}

	var errs []error

	for _, e := range entries {
		rel := joinVolumePath(dir, e.Name())

		// junctions, symbolic links and other reparse points are neither
		switch t := e.Type(); {
		case t == fs.ModeDir:
			if recursive {
				if err := walkMatching(root, rel, pattern, recursive, files); err != nil {
					errs = append(errs, err)
				}
			}
		case t.IsRegular():
			if MatchPattern(pattern, e.Name()) {
				*files = append(*files, rel)
			}
		}
	}

	return stderrors.Join(errs...)
}

// isPlainPath reports whether every folder of dir, relative to root, is a
// plain directory and not a reparse point. A missing folder is not.
func isPlainPath(root, dir string) bool {
	if dir == "" {
		return true
	}

	cur := ""

	for part := range strings.SplitSeq(dir, `\`) {
		cur = joinVolumePath(cur, part)

		if !isPlainDir(root + cur) {
			return false
		}
	}

	return true
}

// slicesDeleteSeen removes the files already in seen, ignoring case, and
// adds the others to it.
func slicesDeleteSeen(files []string, seen map[string]bool) []string {
	out := files[:0]

	for _, f := range files {
		k := strings.ToLower(f)
		if seen[k] {
			continue
		}

		seen[k] = true

		out = append(out, f)
	}

	return out
}

// registryFileSpecs reads a registry list of files, such as
// FilesNotToSnapshot, whose values are lists of FileSpec entries.
func registryFileSpecs(key, source string) ([]FileSpec, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, errors.Wrapf(err, "unable to open %v", key)
	}

	defer k.Close() //nolint:errcheck

	names, err := k.ReadValueNames(0)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to list %v", key)
	}

	var specs []FileSpec

	for _, name := range names {
		var values []string

		switch _, typ, err := k.GetValue(name, nil); {
		case err != nil:
			continue
		case typ == registry.MULTI_SZ:
			values, _, _ = k.GetStringsValue(name)
		case typ == registry.SZ || typ == registry.EXPAND_SZ:
			v, _, _ := k.GetStringValue(name)
			values = []string{v}
		}

		for _, v := range values {
			if spec, ok := ParseFileSpec(source+`\`+name, v); ok {
				specs = append(specs, spec)
			}
		}
	}

	return specs, nil
}

// expandEnv expands %VARIABLE% references with this process's environment;
// image runs run as SYSTEM, whose %TEMP% is Windows\Temp.
func expandEnv(s string) string {
	expanded, err := registry.ExpandString(s)
	if err != nil {
		return s
	}

	return expanded
}
