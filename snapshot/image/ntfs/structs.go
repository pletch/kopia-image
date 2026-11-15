// Package ntfs queries NTFS volumes and volume shadow copies for kopia image:
// volume geometry, the cluster allocation bitmap, and the extents of files
// that a shadow copy does not freeze.
//
// The decoders of the Windows file system control structures are platform
// independent, so they are tested on every platform; the queries themselves
// are Windows-only.
package ntfs

import (
	"encoding/binary"
	"strings"
	"unicode/utf16"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
)

// Layout of NTFS_VOLUME_DATA_BUFFER.
const (
	volumeDataSize           = 96
	volumeDataSerialOff      = 0
	volumeDataTotalClusters  = 16
	volumeDataFreeClusters   = 24
	volumeDataBytesPerSector = 40
	volumeDataBytesPerClust  = 44
	volumeDataMftStartLcn    = 64
)

// Layout of VOLUME_BITMAP_BUFFER and RETRIEVAL_POINTERS_BUFFER.
const (
	bitmapHeaderSize        = 16
	retrievalHeaderSize     = 16
	retrievalExtentSize     = 16
	retrievalStartingVcnOff = 8
)

// Layout of FILE_ID_BOTH_DIR_INFO.
const (
	dirInfoNameLengthOff = 60
	dirInfoNameOff       = 104
)

// unallocatedLcn marks a sparse or compressed run without clusters.
const unallocatedLcn = -1

// shadowStorageSuffix ends the names of the volume shadow copy diff area files
// in System Volume Information.
const shadowStorageSuffix = "{3808876b-c176-4e48-b7ae-04046e6cc752}"

// VolumeData is the geometry of an NTFS volume.
type VolumeData struct {
	SerialNumber   uint64
	TotalClusters  int64
	FreeClusters   int64
	BytesPerSector int64
	ClusterSize    int64
	MftStartLcn    int64
}

func decodeVolumeData(b []byte) (VolumeData, error) {
	if len(b) < volumeDataSize {
		return VolumeData{}, errors.Errorf("volume data is %v bytes, too short", len(b))
	}

	vd := VolumeData{
		SerialNumber:   binary.LittleEndian.Uint64(b[volumeDataSerialOff:]),
		TotalClusters:  int64(binary.LittleEndian.Uint64(b[volumeDataTotalClusters:])), //nolint:gosec
		FreeClusters:   int64(binary.LittleEndian.Uint64(b[volumeDataFreeClusters:])),  //nolint:gosec
		BytesPerSector: int64(binary.LittleEndian.Uint32(b[volumeDataBytesPerSector:])),
		ClusterSize:    int64(binary.LittleEndian.Uint32(b[volumeDataBytesPerClust:])),
		MftStartLcn:    int64(binary.LittleEndian.Uint64(b[volumeDataMftStartLcn:])), //nolint:gosec
	}

	if vd.ClusterSize <= 0 || vd.BytesPerSector <= 0 || vd.ClusterSize%vd.BytesPerSector != 0 || vd.TotalClusters <= 0 {
		return VolumeData{}, errors.Errorf("invalid volume data %+v", vd)
	}

	return vd, nil
}

// decodeBitmapChunk decodes one VOLUME_BITMAP_BUFFER into its starting
// cluster and the bitmap bytes it carries. n is the number of bytes returned.
func decodeBitmapChunk(b []byte) (startingLcn int64, bitmap []byte, err error) {
	if len(b) < bitmapHeaderSize {
		return 0, nil, errors.Errorf("volume bitmap is %v bytes, too short", len(b))
	}

	startingLcn = int64(binary.LittleEndian.Uint64(b)) //nolint:gosec
	if startingLcn < 0 || startingLcn%8 != 0 {
		return 0, nil, errors.Errorf("invalid bitmap starting cluster %v", startingLcn)
	}

	return startingLcn, b[bitmapHeaderSize:], nil
}

// appendRetrievalExtents decodes a RETRIEVAL_POINTERS_BUFFER and appends the
// byte extents of its allocated runs to out. It returns the next virtual
// cluster to query.
func appendRetrievalExtents(out []image.Extent, b []byte, clusterSize int64) ([]image.Extent, int64, error) {
	if len(b) < retrievalHeaderSize {
		return out, 0, errors.Errorf("retrieval pointers are %v bytes, too short", len(b))
	}

	count := int(binary.LittleEndian.Uint32(b))
	if len(b) < retrievalHeaderSize+count*retrievalExtentSize {
		return out, 0, errors.Errorf("retrieval pointers are %v bytes, too short for %v extents", len(b), count)
	}

	vcn := int64(binary.LittleEndian.Uint64(b[retrievalStartingVcnOff:])) //nolint:gosec

	for i := range count {
		e := b[retrievalHeaderSize+i*retrievalExtentSize:]
		nextVcn := int64(binary.LittleEndian.Uint64(e)) //nolint:gosec
		lcn := int64(binary.LittleEndian.Uint64(e[8:])) //nolint:gosec

		if nextVcn <= vcn {
			return out, 0, errors.Errorf("retrieval pointers are not increasing at extent %v", i)
		}

		if lcn != unallocatedLcn {
			if lcn < 0 {
				return out, 0, errors.Errorf("invalid cluster %v at extent %v", lcn, i)
			}

			out = append(out, image.Extent{Start: lcn * clusterSize, Length: (nextVcn - vcn) * clusterSize})
		}

		vcn = nextVcn
	}

	return out, vcn, nil
}

// decodeDirectoryNames decodes the file names in a buffer of
// FILE_ID_BOTH_DIR_INFO entries.
func decodeDirectoryNames(b []byte) ([]string, error) {
	var names []string

	for off := 0; off < len(b); {
		e := b[off:]
		if len(e) < dirInfoNameOff {
			return nil, errors.Errorf("directory entry at %v is truncated", off)
		}

		nameLen := int(binary.LittleEndian.Uint32(e[dirInfoNameLengthOff:]))
		if nameLen%2 != 0 || len(e) < dirInfoNameOff+nameLen {
			return nil, errors.Errorf("directory entry at %v has invalid name length %v", off, nameLen)
		}

		u := make([]uint16, nameLen/2) //nolint:mnd
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(e[dirInfoNameOff+2*i:])
		}

		names = append(names, string(utf16.Decode(u)))

		next := int(binary.LittleEndian.Uint32(e))
		if next == 0 {
			break
		}

		if next < dirInfoNameOff {
			return nil, errors.Errorf("directory entry at %v has invalid next offset %v", off, next)
		}

		off += next
	}

	return names, nil
}

// isShadowStorageFile reports whether a System Volume Information entry is a
// volume shadow copy diff area file.
func isShadowStorageFile(name string) bool {
	return len(name) > len(shadowStorageSuffix) && strings.HasSuffix(strings.ToLower(name), shadowStorageSuffix)
}
