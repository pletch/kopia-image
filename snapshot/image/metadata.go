package image

import (
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo/object"
)

// Names of the entries in the root directory of an image snapshot.
const (
	MetadataFileName      = "image.json"
	LayoutFileName        = "layout.json"
	RestoreScriptFileName = "restore.sh"

	volumeEntrySuffix = ".img"
	bitmapEntrySuffix = ".bitmap"
)

// Versions of the image snapshot format described by Metadata.
const (
	// FormatVersion is written when restoring an image takes only writing
	// its volumes.
	FormatVersion = 1

	// FormatVersionDeletesExcluded is written when a restore must also
	// delete excluded files whose programs would not recognize them holding
	// zeros (see ExcludedFolder.SafeAsZeros). kopia versions that read only
	// FormatVersion refuse such images instead of leaving the zeros.
	FormatVersionDeletesExcluded = 2

	// MaxFormatVersion is the newest version this kopia reads.
	MaxFormatVersion = FormatVersionDeletesExcluded
)

// DefaultChunkSize is the fixed chunk size of image objects. Every chunk i of
// an image covers bytes [i*ChunkSize, (i+1)*ChunkSize) of its volume.
const DefaultChunkSize = 4 << 20

// VolumeKind is how a partition was imaged.
type VolumeKind string

// Supported volume kinds.
const (
	// VolumeKindNTFS is an NTFS volume read from a shadow copy with free and
	// unstable clusters zeroed.
	VolumeKindNTFS VolumeKind = "ntfs"

	// VolumeKindRaw is a partition read raw, twice, with matching digests.
	VolumeKindRaw VolumeKind = "raw"
)

// Metadata is the content of image.json, written last in an image snapshot.
type Metadata struct {
	FormatVersion int `json:"formatVersion"`

	// ChunkSize is the fixed chunk size of every volume object, in bytes.
	ChunkSize int64 `json:"chunkSize"`

	// DiskGUID is the GPT disk GUID of the imaged disk, matching layout.json.
	DiskGUID string `json:"diskGuid"`

	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`

	// Volumes holds one entry per imaged partition, ordered by partition number.
	Volumes []Volume `json:"volumes"`

	// NewDataBytes is the size of the new data imaging the volumes stored,
	// before compression; chunks that were already in the repository are not
	// counted. Through a repository server too old to report it, it is what
	// was sent to the server. Nil for images taken before kopia recorded it.
	NewDataBytes *int64 `json:"newDataBytes,omitempty"`

	// UploadedBytes is the size of the same data as stored, compressed and
	// encrypted. Nil when not known: through a repository server too old to
	// report it. Images of kopia v0.23.1-wails.15 recorded what was sent to a
	// repository server here, before compression.
	UploadedBytes *int64 `json:"uploadedBytes,omitempty"`

	// Drivers are the storage, network and USB driver packages of the imaged
	// computer stored in the image's drivers folder, for recovery media built
	// on other computers. Empty for images taken without them.
	Drivers []Driver `json:"drivers,omitempty"`
}

// Volume describes the image of one partition.
type Volume struct {
	// EntryName is the name of the image file in the snapshot root, e.g. "p3-C.img".
	EntryName string `json:"entryName"`

	// PartitionNumber is the GPT entry number in layout.json.
	PartitionNumber int `json:"partitionNumber"`

	Kind VolumeKind `json:"kind"`

	// ObjectID is the image object. restore.sh streams it with "kopia show".
	ObjectID object.ID `json:"objectId"`

	// Size is the length of the image in bytes.
	Size int64 `json:"size"`

	// ClusterSize and SerialNumber are set for NTFS volumes.
	ClusterSize  int64  `json:"clusterSize,omitempty"`
	SerialNumber string `json:"serialNumber,omitempty"`

	// FreeBytes is the size of the free clusters read as zeros. The free
	// extents themselves are in the volume's .bitmap entry.
	FreeBytes int64 `json:"freeBytes,omitempty"`

	// UnstableExtents are the extents of files that the shadow copy does not
	// freeze, such as pagefile.sys, which were read as zeros.
	UnstableExtents []Extent `json:"unstableExtents,omitempty"`

	// Excluded are the folders whose files were read as zeros because the
	// programs using them recreate their contents, such as kopia's caches.
	// The files are in the image full of zeros; restoring the image deletes
	// them.
	Excluded []ExcludedFolder `json:"excluded,omitempty"`

	// ExcludedFilesID is the list of the excluded files, also stored as the
	// volume's .excluded entry: restoring the image deletes them, and
	// browsing it leaves them out. See ReadExcludedFiles.
	ExcludedFilesID object.ID `json:"excludedFilesObjectId,omitempty"`

	Stats VolumeStats `json:"stats"`
}

// VolumeStats are the statistics of imaging one volume.
type VolumeStats struct {
	// BytesRead is the number of bytes read from the source device.
	BytesRead int64 `json:"bytesRead"`

	// ChunksTotal is the number of chunks in the image and ZeroChunks those
	// consisting only of zeros.
	ChunksTotal int64 `json:"chunksTotal"`
	ZeroChunks  int64 `json:"zeroChunks"`
}

// Validate checks the metadata for internal consistency and against the layout
// of the imaged disk.
func (m *Metadata) Validate(layout *Layout) error {
	if m.FormatVersion < FormatVersion || m.FormatVersion > MaxFormatVersion {
		return errors.Errorf("unsupported image format version %v; this kopia reads versions %v to %v", m.FormatVersion, FormatVersion, MaxFormatVersion)
	}

	if m.ChunkSize <= 0 || m.ChunkSize%layout.SectorSize != 0 {
		return errors.Errorf("invalid chunk size %v", m.ChunkSize)
	}

	if m.DiskGUID != layout.DiskGUID {
		return errors.Errorf("disk GUID %q does not match layout disk GUID %q", m.DiskGUID, layout.DiskGUID)
	}

	if len(m.Volumes) == 0 {
		return errors.New("no volumes")
	}

	for i := range m.Volumes {
		v := &m.Volumes[i]

		if err := v.validate(layout); err != nil {
			return errors.Wrapf(err, "volume %q", v.EntryName)
		}

		if i > 0 && v.PartitionNumber <= m.Volumes[i-1].PartitionNumber {
			return errors.Errorf("volumes are not ordered by distinct partition numbers at %q", v.EntryName)
		}
	}

	return nil
}

func (v *Volume) validate(layout *Layout) error {
	p, ok := layout.Partition(v.PartitionNumber)
	if !ok {
		return errors.Errorf("partition %v is not in the layout", v.PartitionNumber)
	}

	if !strings.HasPrefix(v.EntryName, "p"+strconv.Itoa(v.PartitionNumber)+"-") || !strings.HasSuffix(v.EntryName, volumeEntrySuffix) {
		return errors.Errorf("entry name does not match partition %v", v.PartitionNumber)
	}

	if v.Kind != VolumeKindNTFS && v.Kind != VolumeKindRaw {
		return errors.Errorf("unknown kind %q", v.Kind)
	}

	if v.ObjectID == object.EmptyID {
		return errors.New("missing object ID")
	}

	if v.Size <= 0 || v.Size > p.Length {
		return errors.Errorf("size %v does not fit partition length %v", v.Size, p.Length)
	}

	return nil
}

// VolumeEntryName returns the snapshot entry name for the image of a
// partition, e.g. "p3-C.img" for partition 3 with label "C". Characters other
// than ASCII letters, digits, '-' and '_' in the label become '_'.
func VolumeEntryName(partitionNumber int, label string) string {
	return volumeEntryPrefix(partitionNumber, label) + volumeEntrySuffix
}

// BitmapEntryName returns the snapshot entry name for the allocation bitmap
// stored next to the image of an NTFS volume, e.g. "p3-C.bitmap".
func BitmapEntryName(partitionNumber int, label string) string {
	return volumeEntryPrefix(partitionNumber, label) + bitmapEntrySuffix
}

func volumeEntryPrefix(partitionNumber int, label string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, label)

	if safe == "" {
		safe = "part"
	}

	return "p" + strconv.Itoa(partitionNumber) + "-" + safe
}

// ExcludedFolder is a folder of a volume whose files were read as zeros.
type ExcludedFolder struct {
	// Source says why the files were left out: "kopia cache", or the
	// registry list and value name, e.g. `FilesNotToSnapshot\OutlookOST`.
	Source string `json:"source,omitempty"`

	// Path is relative to the volume root, e.g.
	// `Users\me\AppData\Local\kopia\0123456789abcdef\server-contents`.
	Path string `json:"path"`

	// Pattern selects the files of the folder by name, with * and ?
	// wildcards; an empty pattern selects every file. Recursive includes
	// the folder's subfolders.
	Pattern   string `json:"pattern,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`

	// Files is the number of files read as zeros, and Bytes the size of
	// their clusters.
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`

	// SafeAsZeros is set when the programs using the files detect them
	// holding zeros and recreate them, as kopia does with its caches, so a
	// restore that does not delete them still works. Images with other
	// excluded folders have FormatVersionDeletesExcluded.
	SafeAsZeros bool `json:"safeAsZeros,omitempty"`
}

// RequiredFormatVersion returns the format version an image with these
// volumes must have: FormatVersionDeletesExcluded when a restore must delete
// some excluded files, otherwise FormatVersion.
func RequiredFormatVersion(volumes []Volume) int {
	for i := range volumes {
		for _, f := range volumes[i].Excluded {
			if !f.SafeAsZeros {
				return FormatVersionDeletesExcluded
			}
		}
	}

	return FormatVersion
}

// ExcludedBytes returns the size of the clusters of the excluded folders.
func ExcludedBytes(folders []ExcludedFolder) int64 {
	var n int64

	for _, f := range folders {
		n += f.Bytes
	}

	return n
}
