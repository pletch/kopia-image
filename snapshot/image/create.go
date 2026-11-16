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
	"github.com/kopia/kopia/repo/manifest"
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
	imageSplitter             = "FIXED-4M"
	defaultRawReadAttempts    = 3
	failureFlushTimeout       = time.Minute
	defaultCheckpointInterval = 45 * time.Minute

	// incompleteReasonCheckpoint marks checkpoint manifests like the
	// snapshot uploader does (upload.IncompleteReasonCheckpoint).
	incompleteReasonCheckpoint = "checkpoint"

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

	// CheckpointInterval is how often an incomplete snapshot of what has been
	// written so far is saved. Until the image snapshot is saved, nothing
	// else refers to the chunks a run uploads, and maintenance may remove
	// unreferenced contents that are old enough: those of a run that takes
	// very long, or of a failed run that a later run reuses. The checkpoint
	// refers to them. Zero means 45 minutes, like file snapshots; a negative
	// value disables checkpoints.
	CheckpointInterval time.Duration

	// RawReadAttempts limits the read-twice attempts for raw volumes.
	RawReadAttempts int

	// NewBuffer allocates the chunk read buffer, e.g. aligned for unbuffered
	// device reads. It defaults to make.
	NewBuffer func(n int) []byte

	// Tags are added to the snapshot manifest besides TagKey.
	Tags map[string]string

	// Logf, when set, receives progress messages.
	Logf func(format string, args ...any)

	// progress is what the run has written so far; Create sets it.
	progress *progress
}

// Create images the volumes into a new snapshot of opt.Source. The snapshot
// manifest is saved last, so a failed run leaves no image snapshot. While it
// runs and when it fails, Create saves an incomplete "checkpoint" snapshot of
// what it has written, so the contents stay referenced until the next run
// reuses them; retention removes checkpoints once a newer image exists.
// Create flushes the repository before returning; the caller applies
// retention.
func Create(ctx context.Context, rep repo.RepositoryWriter, options *CreateOptions) (*snapshot.Manifest, *Metadata, error) {
	if err := options.validate(); err != nil {
		return nil, nil, err
	}

	// the caller's options stay untouched
	withProgress := *options
	withProgress.progress = &progress{}
	opt := &withProgress

	md := &Metadata{
		FormatVersion: FormatVersion,
		ChunkSize:     DefaultChunkSize,
		DiskGUID:      opt.Layout.DiskGUID,
		StartTime:     rep.Time().UTC(),
	}

	uploaded, uploadedOK := uploadCounter(rep)
	uploadedBefore := uploaded()

	stopPeriodic := opt.startPeriodic(ctx, rep, md)

	files, err := opt.writeVolumes(ctx, rep, md)
	if flushErr := stopPeriodic(); err == nil {
		err = flushErr
	}

	if err != nil {
		opt.saveProgressAfterFailure(ctx, rep, md)

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

		// a repository server makes the contents sent to it visible only once
		// flushed; a direct connection sees its own pending contents
		if _, direct := rep.(repo.DirectRepositoryWriter); !direct {
			if err := rep.Flush(ctx); err != nil {
				return nil, errors.Wrapf(err, "unable to flush the image of partition %v", v.PartitionNumber)
			}
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
		opt.progress.addFile(files[len(files)-1])

		if v.Bitmap != nil {
			bitmapName := BitmapEntryName(v.PartitionNumber, v.Label)

			boid, err := opt.writeSmallObject(ctx, rep, bitmapName, v.Bitmap, opt.Compressor)
			if err != nil {
				return nil, err
			}

			files = append(files, opt.fileEntry(bitmapName, int64(len(v.Bitmap)), boid, md.StartTime, filePermissions))
			opt.progress.addFile(files[len(files)-1])
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

	man, err := opt.saveManifest(ctx, rep, md, files, md.EndTime, "")
	if err != nil {
		return nil, err
	}

	// the image now refers to everything the checkpoint did
	opt.progress.replaceCheckpoint(ctx, rep, "", opt.logf)

	if err := rep.Flush(ctx); err != nil {
		return nil, errors.Wrap(err, "unable to flush image snapshot")
	}

	return man, nil
}

// saveManifest writes the root directory holding files and saves a snapshot
// manifest for it: the image snapshot, or with an incompleteReason a
// checkpoint.
func (opt *CreateOptions) saveManifest(ctx context.Context, rep repo.RepositoryWriter, md *Metadata, files []*snapshot.DirEntry, endTime time.Time, incompleteReason string) (*snapshot.Manifest, error) {
	var b snapshotfs.DirManifestBuilder

	for _, f := range files {
		b.AddEntry(f)
	}

	mtime := fs.UTCTimestampFromTime(md.StartTime)
	dm := b.Build(mtime, incompleteReason)

	rootOID, err := snapshotfs.WriteDirManifest(ctx, rep, "", dm, opt.MetadataCompressor)
	if err != nil {
		return nil, errors.Wrap(err, "unable to write image directory")
	}

	tags := map[string]string{TagKey: TagValue}
	maps.Copy(tags, opt.Tags)

	man := &snapshot.Manifest{
		Source:           opt.Source,
		Description:      opt.Description,
		StartTime:        fs.UTCTimestampFromTime(md.StartTime),
		EndTime:          fs.UTCTimestampFromTime(endTime),
		IncompleteReason: incompleteReason,
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

	if md.UploadedBytes != nil {
		n := *md.UploadedBytes
		man.UploadedBytes = &n
	}

	if _, err := snapshot.SaveSnapshot(ctx, rep, man); err != nil {
		return nil, errors.Wrap(err, "unable to save image snapshot")
	}

	return man, nil
}

// checkpoint saves an incomplete snapshot of the volumes written so far and
// the written part of the volume in progress, replacing the run's previous
// checkpoint. It saves nothing before the first chunk is written.
func (opt *CreateOptions) checkpoint(ctx context.Context, rep repo.RepositoryWriter, md *Metadata) error {
	files, name, entries := opt.progress.snapshot()

	if len(entries) > 0 {
		oid, err := opt.writeChunkList(ctx, rep, entries, name)
		if err != nil {
			return err
		}

		last := entries[len(entries)-1]
		files = append(files, opt.fileEntry(name, last.Start+last.Length, oid, md.StartTime, filePermissions))
	}

	if len(files) == 0 {
		return nil
	}

	man, err := opt.saveManifest(ctx, rep, md, files, rep.Time().UTC(), incompleteReasonCheckpoint)
	if err != nil {
		return err
	}

	opt.progress.replaceCheckpoint(ctx, rep, man.ID, opt.logf)

	return nil
}

// progress is what a run has written so far. The imaging goroutines update
// it and checkpoints read it. A nil progress tracks nothing, for writing
// volumes outside Create.
type progress struct {
	mu sync.Mutex
	// files are the entries of the finished volumes and bitmaps.
	// +checklocks:mu
	files []*snapshot.DirEntry
	// volume names the volume being written and entries is its chunk list,
	// which fills in any order; a chunk without an object is not written yet.
	// +checklocks:mu
	volume string
	// +checklocks:mu
	entries []object.IndirectObjectEntry
	// checkpoint is the run's latest checkpoint manifest.
	// +checklocks:mu
	checkpoint manifest.ID
}

func (p *progress) addFile(e *snapshot.DirEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.files = append(p.files, e)
}

// beginVolume starts tracking the chunk list of a volume. Until endVolume,
// the objects of entries may only be set with setChunk.
func (p *progress) beginVolume(name string, entries []object.IndirectObjectEntry) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.volume, p.entries = name, entries
}

func (p *progress) endVolume() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.volume, p.entries = "", nil
}

func (p *progress) setChunk(entry *object.IndirectObjectEntry, oid object.ID) {
	if p == nil {
		entry.Object = oid
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entry.Object = oid
}

// snapshot returns copies of the finished entries and of the chunks written
// from the start of the volume in progress up to the first gap.
func (p *progress) snapshot() (files []*snapshot.DirEntry, volume string, entries []object.IndirectObjectEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := 0
	for n < len(p.entries) && p.entries[n].Object != object.EmptyID {
		n++
	}

	return slices.Clone(p.files), p.volume, slices.Clone(p.entries[:n])
}

// replaceCheckpoint records id as the run's checkpoint and deletes the one it
// replaces. Deleting is best effort: a client that may only add snapshots
// cannot, and retention removes old checkpoints anyway.
func (p *progress) replaceCheckpoint(ctx context.Context, rep repo.RepositoryWriter, id manifest.ID, logf func(string, ...any)) {
	p.mu.Lock()
	previous := p.checkpoint
	p.checkpoint = id
	p.mu.Unlock()

	if previous == "" {
		return
	}

	if err := rep.DeleteManifest(ctx, previous); err != nil {
		logf("Unable to delete the checkpoint %v, which retention will remove: %v", previous, err)
	}
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

// saveProgressAfterFailure saves a checkpoint of what a failed run wrote and
// indexes its chunks, so that the next run does not upload them again and
// maintenance does not remove them before then. It is best effort: when the
// failure was the connection, this fails too and the chunks are uploaded
// again. It runs even if ctx was canceled, but for at most
// failureFlushTimeout.
func (opt *CreateOptions) saveProgressAfterFailure(ctx context.Context, rep repo.RepositoryWriter, md *Metadata) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureFlushTimeout)
	defer cancel()

	if opt.CheckpointInterval >= 0 {
		if err := opt.checkpoint(ctx, rep, md); err != nil {
			opt.logf("Unable to save a checkpoint of the failed run: %v", err)
		}
	}

	if err := rep.Flush(ctx); err != nil {
		opt.logf("Unable to save the progress of the failed run: %v", err)
	}
}

// tick returns a ticker's channel and stop function, or a channel that never
// fires for a non-positive interval.
func tick(interval time.Duration) (<-chan time.Time, func()) {
	if interval <= 0 {
		return nil, func() {}
	}

	t := time.NewTicker(interval)

	return t.C, t.Stop
}

// startPeriodic flushes rep every FlushInterval and saves a checkpoint every
// CheckpointInterval until the returned function is called, which reports the
// first flush error. A checkpoint that cannot be saved is only logged: the
// run does not depend on it.
func (opt *CreateOptions) startPeriodic(ctx context.Context, rep repo.RepositoryWriter, md *Metadata) func() error {
	checkpointInterval := opt.CheckpointInterval
	if checkpointInterval == 0 {
		checkpointInterval = defaultCheckpointInterval
	}

	if opt.FlushInterval <= 0 && checkpointInterval < 0 {
		return func() error { return nil }
	}

	done := make(chan struct{})

	var (
		wg       sync.WaitGroup
		flushErr error
	)

	wg.Go(func() {
		flushTick, stopFlushTick := tick(opt.FlushInterval)
		defer stopFlushTick()

		checkpointTick, stopCheckpointTick := tick(checkpointInterval)
		defer stopCheckpointTick()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-checkpointTick:
				if err := opt.checkpoint(ctx, rep, md); err != nil {
					opt.logf("Unable to save a checkpoint: %v", err)
					continue
				}
			case <-flushTick:
			}

			// a checkpoint is flushed at once, so that it is in the repository
			if err := rep.Flush(ctx); err != nil {
				flushErr = errors.Wrap(err, "periodic flush failed")
				return
			}
		}
	})

	return func() error {
		close(done)
		wg.Wait()

		return flushErr
	}
}
