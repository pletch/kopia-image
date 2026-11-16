package imagerestore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDiskNumber(t *testing.T) {
	for s, want := range map[string]uint32{"0": 0, "7": 7, "63": 63, "4294967295": 4294967295} {
		got, err := parseDiskNumber(s)
		require.NoError(t, err, s)
		require.Equal(t, want, got)
	}
}

func TestParseDiskNumber_rejectsInvalidNumbers(t *testing.T) {
	// 4294967297 would wrap around to disk 1
	for _, s := range []string{"", "x", "1x", "-1", "+1", " 1", "1_0", "4294967296", "4294967297", "99999999999999999999999999"} {
		_, err := parseDiskNumber(s)
		require.Error(t, err, "%q", s)
	}
}
