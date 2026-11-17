package image

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

// DriversEntryName is the folder of an image snapshot that holds the driver
// packages of the imaged computer, one subfolder per package.
const DriversEntryName = "drivers"

// Driver identifies a driver package: a storage, network or USB driver that
// recovery media needs to reach the disks and the network.
type Driver struct {
	// Class is the short name of the device class, such as Net.
	Class string `json:"class"`

	// INF is the vendor's INF name, such as rt640x64.inf.
	INF string `json:"inf"`

	Version string `json:"version"`

	// Entry is the package's subfolder of DriversEntryName in an image; empty
	// elsewhere.
	Entry string `json:"entry,omitempty"`
}

func (d Driver) String() string {
	return strings.TrimSpace(d.Class + " " + d.INF + " " + d.Version)
}

// SameDriver tells whether two packages are versions of the same driver.
func SameDriver(a, b Driver) bool {
	return strings.EqualFold(a.INF, b.INF)
}

// DriverSource is a driver package on the local disk, to store with an image.
type DriverSource struct {
	Driver

	// Dir is the folder holding the package's files.
	Dir string
}

// writeDrivers stores the driver packages in the image's drivers folder. A
// package that cannot be stored is logged and left out: drivers help build
// recovery media for the computer, but the image does not depend on them.
func (opt *CreateOptions) writeDrivers(ctx context.Context, rep repo.RepositoryWriter, mtime fs.UTCTimestamp) (*snapshot.DirEntry, []Driver) {
	var (
		b      snapshotfs.DirManifestBuilder
		stored []Driver
	)

	for _, d := range opt.Drivers {
		d.Entry = safeEntryName(d.Entry)
		if d.Entry == "" {
			d.Entry = safeEntryName(strings.TrimSuffix(d.INF, filepath.Ext(d.INF)))
		}

		e, err := opt.writeTree(ctx, rep, DriversEntryName+"/"+d.Entry, d.Dir, d.Entry)
		if err != nil {
			opt.logf("Unable to store the driver %v with the image: %v", d.Driver, err)
			continue
		}

		b.AddEntry(e)

		stored = append(stored, d.Driver)
	}

	if len(stored) == 0 {
		return nil, nil
	}

	e, err := opt.writeDir(ctx, rep, DriversEntryName, DriversEntryName, &b, mtime)
	if err != nil {
		opt.logf("Unable to store the drivers with the image: %v", err)
		return nil, nil
	}

	return e, stored
}

// writeTree stores the files and folders under dir as an entry named name.
func (opt *CreateOptions) writeTree(ctx context.Context, rep repo.RepositoryWriter, relPath, dir, name string) (*snapshot.DirEntry, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, errors.Wrap(err, "unable to read the driver folder")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.Wrap(err, "unable to read the driver folder")
	}

	var b snapshotfs.DirManifestBuilder

	for _, de := range entries {
		p := filepath.Join(dir, de.Name())

		switch {
		case de.IsDir():
			e, err := opt.writeTree(ctx, rep, relPath+"/"+de.Name(), p, de.Name())
			if err != nil {
				return nil, err
			}

			b.AddEntry(e)
		case de.Type().IsRegular():
			e, err := opt.writeLocalFile(ctx, rep, relPath+"/"+de.Name(), p, de.Name())
			if err != nil {
				return nil, err
			}

			b.AddEntry(e)
		}
	}

	return opt.writeDir(ctx, rep, relPath, name, &b, fs.UTCTimestampFromTime(fi.ModTime()))
}

func (opt *CreateOptions) writeDir(ctx context.Context, rep repo.RepositoryWriter, relPath, name string, b *snapshotfs.DirManifestBuilder, mtime fs.UTCTimestamp) (*snapshot.DirEntry, error) {
	dm := b.Build(mtime, "")

	oid, err := snapshotfs.WriteDirManifest(ctx, rep, relPath, dm, opt.MetadataCompressor)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to write the folder %v", relPath)
	}

	return &snapshot.DirEntry{
		Name:        name,
		Type:        snapshot.EntryTypeDirectory,
		Permissions: directoryPermissions,
		ModTime:     mtime,
		ObjectID:    oid,
		DirSummary:  dm.Summary,
	}, nil
}

func (opt *CreateOptions) writeLocalFile(ctx context.Context, rep repo.RepositoryWriter, relPath, path, name string) (*snapshot.DirEntry, error) {
	f, err := os.Open(path) //nolint:gosec // a file of a driver package that kopia exported
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open %v", path)
	}
	defer f.Close() //nolint:errcheck

	fi, err := f.Stat()
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read %v", path)
	}

	w := rep.NewObjectWriter(ctx, object.WriterOptions{
		Description:        "FILE:" + relPath,
		Compressor:         opt.MetadataCompressor,
		MetadataCompressor: opt.MetadataCompressor,
	})
	defer w.Close() //nolint:errcheck

	n, err := io.Copy(w, f)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to store %v", path)
	}

	oid, err := w.Result()
	if err != nil {
		return nil, errors.Wrapf(err, "unable to store %v", path)
	}

	return opt.fileEntry(name, n, oid, fi.ModTime(), filePermissions), nil
}

// safeEntryName makes a name usable as a folder name, or returns "".
func safeEntryName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, s)

	if strings.Trim(s, ".") == "" {
		return ""
	}

	return s
}

// ExtractDriver copies a driver package stored with an image into dest,
// which must not exist yet.
func ExtractDriver(ctx context.Context, rep repo.Repository, m *snapshot.Manifest, d Driver, dest string) error {
	if d.Entry == "" || d.Entry != safeEntryName(d.Entry) {
		return errors.Errorf("invalid driver folder %q", d.Entry)
	}

	root, err := snapshotfs.SnapshotRoot(rep, m)
	if err != nil {
		return errors.Wrap(err, "unable to open the image")
	}

	e, err := childDir(ctx, root, DriversEntryName)
	if err != nil {
		return err
	}

	if e, err = childDir(ctx, e, d.Entry); err != nil {
		return err
	}

	return extractDir(ctx, e, dest)
}

func childDir(ctx context.Context, e fs.Entry, name string) (fs.Directory, error) {
	dir, ok := e.(fs.Directory)
	if !ok {
		return nil, errors.Errorf("%v is not a folder", e.Name())
	}

	c, err := dir.Child(ctx, name)
	if err != nil {
		return nil, errors.Wrapf(err, "the image has no %v", name)
	}

	cd, ok := c.(fs.Directory)
	if !ok {
		return nil, errors.Errorf("%v is not a folder", name)
	}

	return cd, nil
}

func extractDir(ctx context.Context, dir fs.Directory, dest string) error {
	if err := os.Mkdir(dest, 0o700); err != nil { //nolint:mnd
		return errors.Wrap(err, "unable to create the driver folder")
	}

	return errors.Wrapf(fs.IterateEntries(ctx, dir, func(ctx context.Context, e fs.Entry) error {
		name := e.Name()
		if name != filepath.Base(name) || name == "." || name == ".." {
			return errors.Errorf("invalid name %q", name)
		}

		p := filepath.Join(dest, name)

		switch e := e.(type) {
		case fs.Directory:
			return extractDir(ctx, e, p)
		case fs.File:
			return extractFile(ctx, e, p)
		default:
			return nil
		}
	}), "unable to extract %v", dir.Name())
}

func extractFile(ctx context.Context, f fs.File, path string) error {
	r, err := f.Open(ctx)
	if err != nil {
		return errors.Wrapf(err, "unable to open %v", f.Name())
	}
	defer r.Close() //nolint:errcheck

	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec,mnd
	if err != nil {
		return errors.Wrapf(err, "unable to create %v", path)
	}

	if _, err := io.Copy(out, r); err != nil {
		out.Close() //nolint:errcheck
		return errors.Wrapf(err, "unable to write %v", path)
	}

	return errors.Wrapf(out.Close(), "unable to write %v", path)
}

// HostDrivers are the drivers stored with a computer's newest image that
// stores any.
type HostDrivers struct {
	Host string `json:"host"`

	// Image is the image snapshot that stores the drivers.
	Image *snapshot.Manifest `json:"-"`

	ImageID   manifest.ID `json:"imageId"`
	ImageTime time.Time   `json:"imageTime"`

	Drivers []Driver `json:"drivers"`
}

// DriverInventory returns the drivers of every computer with an image that
// stores drivers, ordered by computer.
func DriverInventory(ctx context.Context, rep repo.Repository) ([]HostDrivers, error) {
	mans, err := ListSnapshots(ctx, rep)
	if err != nil {
		return nil, err
	}

	slices.SortFunc(mans, func(a, b *snapshot.Manifest) int { return b.StartTime.ToTime().Compare(a.StartTime.ToTime()) })

	var out []HostDrivers

	for _, m := range mans {
		if slices.ContainsFunc(out, func(h HostDrivers) bool { return strings.EqualFold(h.Host, m.Source.Host) }) {
			continue
		}

		md, err := ReadMetadata(ctx, rep, m)
		if err != nil || len(md.Drivers) == 0 {
			continue
		}

		out = append(out, HostDrivers{Host: m.Source.Host, Image: m, ImageID: m.ID, ImageTime: m.StartTime.ToTime(), Drivers: md.Drivers})
	}

	slices.SortFunc(out, func(a, b HostDrivers) int { return strings.Compare(strings.ToLower(a.Host), strings.ToLower(b.Host)) })

	return out, nil
}
