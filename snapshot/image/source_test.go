package image

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const testDiskGUID = "0f6ab3f2-1c3d-4e5f-8a9b-0c1d2e3f4a5b"

func TestDiskSourcePath(t *testing.T) {
	for _, in := range []string{testDiskGUID, "{0F6AB3F2-1C3D-4E5F-8A9B-0C1D2E3F4A5B}"} {
		p, err := DiskSourcePath(in)
		require.NoError(t, err)
		require.Equal(t, "image:disk:"+testDiskGUID, p)
		require.True(t, IsSourcePath(p))

		g, err := ParseDiskSourcePath(p)
		require.NoError(t, err)
		require.Equal(t, testDiskGUID, g)
	}
}

func TestDiskSourcePath_invalid(t *testing.T) {
	for _, in := range []string{"", "not-a-guid", "urn:uuid:" + testDiskGUID, testDiskGUID + "0"} {
		_, err := DiskSourcePath(in)
		require.Error(t, err, in)
	}
}

func TestParseDiskSourcePath_invalid(t *testing.T) {
	for _, in := range []string{
		"C:\\",
		"/home/tim",
		"image:volume:" + testDiskGUID,
		"image:disk:",
		"image:disk:{" + testDiskGUID + "}",
		"image:disk:0F6AB3F2-1C3D-4E5F-8A9B-0C1D2E3F4A5B",
	} {
		_, err := ParseDiskSourcePath(in)
		require.Error(t, err, in)
	}
}

func TestIsSourcePath(t *testing.T) {
	require.True(t, IsSourcePath("image:anything"))
	require.False(t, IsSourcePath("C:\\image"))
	require.False(t, IsSourcePath("/image:disk:x"))
	require.False(t, IsSourcePath(""))
}
