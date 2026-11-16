package image

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/compression"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

// Snapshot tags set on every image snapshot. SaveSnapshot reserves the
// "type" label, so the kind of snapshot is recorded as a "tag:" label, which
// "kopia snapshot list --tags type:image" matches.
const (
	TagKey   = "tag:type"
	TagValue = "image"
)

const (
	imageSplitter            = "FIXED-4M"
	defaultRawReadAttempts   = 3
	failureFlushTimeout      = time.Minute
	filePermissions          = 0o644
	executablePermissions    = 0o755
	directoryPermissions     = 0o755
	imageObjectDescription   = "IMAGE:"
	metadataObjectDesciption = "IMAGE-META:"
)

// VolumeSource provides the contents of one partition to image.
type VolumeSource struct {
	PartitionNumber int
	Label           string
	Kind            VolumeKind

	// Size is the exact number of bytes Open's reader yields.
	Size int64

	// Open returns a new reader positioned at the start of the volume. For
	// raw volumes it is called at least twice, see ReadTwice. If the reader
	// has a SourceBytesRead() int64 method, as MaskedReader does, it is used
	// to report the bytes read from the device.
	Open func() (io.ReadCloser, error)

	// NTFS volume details recorded in image.json.
	ClusterSize     int64
	SerialNumber    string
	FreeBytes       int64
	UnstableExtents []Extent

	// Bitmap, when set, is stored next to the image as the .bitmap entry.
	Bitmap []byte
}

// CreateOptions configure Create.
type CreateOptions struct {
	Source      snapshot.SourceInfo
	Description string
	Layout      *Layout
	Volumes     []VolumeSource

	// Compressor compresses image chunks, MetadataCompressor the image
	// indexes, metadata files and directory. Both normally come from the
	// source's policy.
	Compressor         compression.Name
	MetadataCompressor compression.Name

	// AsyncWrites is the number of chunks hashed and written in parallel.
	AsyncWrites int

	// FlushInterval is how often the repository is flushed while imaging, so
	// that a failed run's uploaded chunks are indexed and a retry does not
	// upload them again. Zero disables periodic flushes.
	FlushInterval time.Duration

	// RawReadAttempts limits the read-twice attempts for raw volumes.
	RawReadAttempts int

	// NewBuffer allocates the chunk read buffer, e.g. aligned for unbuffered
	// device reads. It defaults to make.
	NewBuffer func(n int) []byte

	// Tags are added to the snapshot manifest besides TagKey.
	Tags map[string]string

	// Logf, when set, receives progress messages.
	Logf func(format string, args ...any)
}

// Create images the volumes into a new snapshot of opt.Source. The snapshot
// manifest is saved last, so a failed run leaves no snapshot; contents it
// wrote are reused by the next run or removed by maintenance. Create flushes
// the repository before returning; the caller applies retention.
func Create(ctx context.Context, rep repo.RepositoryWriter, opt *CreateOptions) (*snapshot.Manifest, *Metadata, error) {
	if err := opt.validate(); err != nil {
		return nil, nil, err
	}

	md := &Metadata{
		FormatVersion: FormatVersion,
		ChunkSize:     DefaultChunkSize,
		DiskGUID:      opt.Layout.DiskGUID,
		StartTime:     rep.Time().UTC(),
	}

	uploaded, uploadedOK := uploadCounter(rep)
	uploadedBefore := uploaded()

	stopFlush := startPeriodicFlush(ctx, rep, opt.FlushInterval)

	files, err := opt.writeVolumes(ctx, rep, md)
	if flushErr := stopFlush(); err == nil {
		err = flushErr
	}

	if err != nil {
		flushAfterFailure(ctx, rep, opt.logf)

		return nil, nil, err
	}

	// flushing here makes the pack upload count complete before it is recorded
	if err := rep.Flush(ctx); err != nil {
		return nil, nil, errors.Wrap(err, "unable to flush images")
	}

	if uploadedOK {
		n := uploaded() - uploadedBefore
		md.UploadedBytes = &n
	}

	md.EndTime = rep.Time().UTC()

	man, err := opt.commit(ctx, rep, md, files)
	if err != nil {
		return nil, nil, err
	}

	return man, md, nil
}

func (opt *CreateOptions) validate() error {
	if opt.Layout == nil {
		return errors.New("missing layout")
	}

	if err := opt.Layout.Validate(); err != nil {
		return errors.Wrap(err, "invalid layout")
	}

	if _, err := ParseDiskSourcePath(opt.Source.Path); err != nil {
		return errors.Wrap(err, "invalid source")
	}

	if len(opt.Volumes) == 0 {
		return errors.New("no volumes to image")
	}

	return nil
}

func (opt *CreateOptions) logf(format string, args ...any) {
	if opt.Logf != nil {
		opt.Logf(format, args...)
	}
}

// writeVolumes writes every volume and its bitmap, fills md.Volumes and
// returns the directory entries written.
func (opt *CreateOptions) writeVolumes(ctx context.Context, rep repo.RepositoryWriter, md *Metadata) ([]*snapshot.DirEntry, error) {
	volumes := slices.Clone(opt.Volumes)
	slices.SortFunc(volumes, func(a, b VolumeSource) int { return a.PartitionNumber - b.PartitionNumber })

	var files []*snapshot.DirEntry

	for i := range volumes {
		v := &volumes[i]
		name := VolumeEntryName(v.PartitionNumber, v.Label)

		opt.logf("Imaging partition %v (%v, %v bytes) as %v", v.PartitionNumber, v.Kind, v.Size, name)

		oid, stats, err := opt.writeVolume(ctx, rep, v, name)
		if err != nil {
			return nil, errors.Wrapf(err, "partition %v", v.PartitionNumber)
		}

		if _, err := rep.VerifyObject(ctx, oid); err != nil {
			return nil, errors.Wrapf(err, "image of partition %v is incomplete", v.PartitionNumber)
		}

		md.Volumes = append(md.Volumes, Volume{
			EntryName:       name,
			PartitionNumber: v.PartitionNumber,
			Kind:            v.Kind,
			ObjectID:        oid,
			Size:            v.Size,
			ClusterSize:     v.ClusterSize,
			SerialNumber:    v.SerialNumber,
			FreeBytes:       v.FreeBytes,
			UnstableExtents: v.UnstableExtents,
			Stats:           stats,
		})

		files = append(files, opt.fileEntry(name, v.Size, oid, md.StartTime, filePermissions))

		if v.Bitmap != nil {
			bitmapName := BitmapEntryName(v.PartitionNumber, v.Label)

			boid, err := opt.writeSmallObject(ctx, rep, bitmapName, v.Bitmap, opt.Compressor)
			if err != nil {
				return nil, err
			}

			files = append(files, opt.fileEntry(bitmapName, int64(len(v.Bitmap)), boid, md.StartTime, filePermissions))
		}
	}

	return files, nil
}

func (opt *CreateOptions) writeVolume(ctx context.Context, rep repo.RepositoryWriter, v *VolumeSource, name string) (object.ID, VolumeStats, error) {
	type result struct {
		oid   object.ID
		stats VolumeStats
	}

	consume := func(r io.Reader) (result, error) {
		oid, stats, err := opt.writeImageObject(ctx, rep, r, v.Size, name)
		return result{oid, stats}, err
	}

	if v.Kind == VolumeKindRaw {
		attempts := opt.RawReadAttempts
		if attempts <= 0 {
			attempts = defaultRawReadAttempts
		}

		res, err := ReadTwice(ctx, attempts, v.Open, consume)

		return res.oid, res.stats, err
	}

	rc, err := v.Open()
	if err != nil {
		return object.EmptyID, VolumeStats{}, errors.Wrap(err, "unable to open volume")
	}
	defer rc.Close() //nolint:errcheck

	res, err := consume(rc)

	if sr, ok := rc.(interface{ SourceBytesRead() int64 }); ok {
		res.stats.BytesRead = sr.SourceBytesRead()
	}

	return res.oid, res.stats, err
}

// writeImageObject writes exactly size bytes from r as a FIXED-4M object.
func (opt *CreateOptions) writeImageObject(ctx context.Context, rep repo.RepositoryWriter, r io.Reader, size int64, name string) (object.ID, VolumeStats, error) {
	w := rep.NewObjectWriter(ctx, object.WriterOptions{
		Description:        imageObjectDescription + name,
		Splitter:           imageSplitter,
		Compressor:         opt.Compressor,
		MetadataCompressor: opt.MetadataCompressor,
		AsyncWrites:        opt.AsyncWrites,
	})
	defer w.Close() //nolint:errcheck

	newBuffer := opt.NewBuffer
	if newBuffer == nil {
		newBuffer = func(n int) []byte { return make([]byte, n) }
	}

	buf := newBuffer(DefaultChunkSize)
	zero := make([]byte, DefaultChunkSize)

	var stats VolumeStats

	for total := int64(0); total < size; {
		if err := ctx.Err(); err != nil {
			return object.EmptyID, stats, errors.Wrap(err, "imaging canceled")
		}

		chunk := buf[:min(int64(len(buf)), size-total)]

		n, err := io.ReadFull(r, chunk)
		if err != nil {
			return object.EmptyID, stats, errors.Wrapf(err, "error reading volume at offset %v of %v", total+int64(n), size)
		}

		if bytes.Equal(chunk, zero[:n]) {
			stats.ZeroChunks++
		}

		if _, err := w.Write(chunk); err != nil {
			return object.EmptyID, stats, errors.Wrap(err, "error writing image")
		}

		stats.ChunksTotal++
		stats.BytesRead += int64(n)
		total += int64(n)
	}

	oid, err := w.Result()
	if err != nil {
		return object.EmptyID, stats, errors.Wrap(err, "error finishing image")
	}

	return oid, stats, nil
}

func (opt *CreateOptions) writeSmallObject(ctx context.Context, rep repo.RepositoryWriter, name string, data []byte, comp compression.Name) (object.ID, error) {
	w := rep.NewObjectWriter(ctx, object.WriterOptions{
		Description:        metadataObjectDesciption + name,
		Compressor:         comp,
		MetadataCompressor: opt.MetadataCompressor,
	})
	defer w.Close() //nolint:errcheck

	if _, err := w.Write(data); err != nil {
		return object.EmptyID, errors.Wrapf(err, "error writing %v", name)
	}

	oid, err := w.Result()

	return oid, errors.Wrapf(err, "error writing %v", name)
}

func (opt *CreateOptions) fileEntry(name string, size int64, oid object.ID, mtime time.Time, perm snapshot.Permissions) *snapshot.DirEntry {
	return &snapshot.DirEntry{
		Name:        name,
		Type:        snapshot.EntryTypeFile,
		Permissions: perm,
		FileSize:    size,
		ModTime:     fs.UTCTimestampFromTime(mtime),
		ObjectID:    oid,
	}
}

// commit writes layout.json, restore.sh and image.json, then the root
// directory and the snapshot manifest.
func (opt *CreateOptions) commit(ctx context.Context, rep repo.RepositoryWriter, md *Metadata, files []*snapshot.DirEntry) (*snapshot.Manifest, error) {
	if err := md.Validate(opt.Layout); err != nil {
		return nil, errors.Wrap(err, "invalid image metadata")
	}

	layoutJSON, err := json.MarshalIndent(opt.Layout, "", "  ")
	if err != nil {
		return nil, errors.Wrap(err, "unable to encode layout")
	}

	var script bytes.Buffer

	if err := WriteRestoreScript(&script, opt.Layout, md); err != nil {
		return nil, err
	}

	metadataJSON, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return nil, errors.Wrap(err, "unable to encode image metadata")
	}

	for _, f := range []struct {
		name string
		data []byte
		perm snapshot.Permissions
	}{
		{LayoutFileName, layoutJSON, filePermissions},
		{RestoreScriptFileName, script.Bytes(), executablePermissions},
		{MetadataFileName, metadataJSON, filePermissions},
	} {
		oid, err := opt.writeSmallObject(ctx, rep, f.name, f.data, opt.MetadataCompressor)
		if err != nil {
			return nil, err
		}

		files = append(files, opt.fileEntry(f.name, int64(len(f.data)), oid, md.StartTime, f.perm))
	}

	var b snapshotfs.DirManifestBuilder

	for _, f := range files {
		b.AddEntry(f)
	}

	mtime := fs.UTCTimestampFromTime(md.StartTime)
	dm := b.Build(mtime, "")

	rootOID, err := snapshotfs.WriteDirManifest(ctx, rep, "", dm, opt.MetadataCompressor)
	if err != nil {
		return nil, errors.Wrap(err, "unable to write image directory")
	}

	tags := map[string]string{TagKey: TagValue}
	maps.Copy(tags, opt.Tags)

	man := &snapshot.Manifest{
		Source:      opt.Source,
		Description: opt.Description,
		StartTime:   fs.UTCTimestampFromTime(md.StartTime),
		EndTime:     fs.UTCTimestampFromTime(md.EndTime),
		Stats: snapshot.Stats{
			TotalFileSize:  dm.Summary.TotalFileSize,
			TotalFileCount: int32(dm.Summary.TotalFileCount), //nolint:gosec
		},
		RootEntry: &snapshot.DirEntry{
			Name:        "disk-" + md.DiskGUID,
			Type:        snapshot.EntryTypeDirectory,
			Permissions: directoryPermissions,
			ModTime:     mtime,
			ObjectID:    rootOID,
			DirSummary:  dm.Summary,
		},
		Tags: tags,
	}

	if _, err := snapshot.SaveSnapshot(ctx, rep, man); err != nil {
		return nil, errors.Wrap(err, "unable to save image snapshot")
	}

	if err := rep.Flush(ctx); err != nil {
		return nil, errors.Wrap(err, "unable to flush image snapshot")
	}

	return man, nil
}

// uploadCounter returns a function reporting the bytes of pack blobs written
// so far, when the repository exposes it.
func uploadCounter(rep repo.RepositoryWriter) (func() int64, bool) {
	dw, ok := rep.(repo.DirectRepositoryWriter)
	if !ok {
		return func() int64 { return 0 }, false
	}

	return func() int64 {
		_, n := dw.ContentManager().Stats.WrittenContent()
		return n
	}, true
}

// flushAfterFailure indexes the chunks a failed run already wrote, so that the
// next run does not upload them again. It is best effort: when the failure was
// the connection, the flush fails too and the chunks are uploaded again. It
// runs even if ctx was canceled, but for at most failureFlushTimeout.
func flushAfterFailure(ctx context.Context, rep repo.RepositoryWriter, logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureFlushTimeout)
	defer cancel()

	if err := rep.Flush(ctx); err != nil {
		logf("Unable to save the progress of the failed run: %v", err)
	}
}

// startPeriodicFlush flushes rep every interval until the returned function is
// called, which reports the first flush error.
func startPeriodicFlush(ctx context.Context, rep repo.RepositoryWriter, interval time.Duration) func() error {
	if interval <= 0 {
		return func() error { return nil }
	}

	done := make(chan struct{})

	var (
		wg       sync.WaitGroup
		flushErr error
	)

	wg.Go(func() {
		t := time.NewTicker(interval)
		defer t.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := rep.Flush(ctx); err != nil {
					flushErr = errors.Wrap(err, "periodic flush failed")
					return
				}
			}
		}
	})

	return func() error {
		close(done)
		wg.Wait()

		return flushErr
	}
}
