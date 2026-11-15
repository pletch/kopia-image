package mount

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func Test_formatSnapshotDriveLabel(t *testing.T) {
	ts := time.Date(2026, 11, 15, 10, 2, 49, 0, time.UTC)

	cases := []struct {
		path string
		want string
	}{
		{`C:\Users\Tim\Documents`, "Kopia - Documents - 2026-11-15 10:02"},
		{`C:\Users\Tim\Documents\`, "Kopia - Documents - 2026-11-15 10:02"},
		{"/home/tim/photos", "Kopia - photos - 2026-11-15 10:02"},
		{"/home/tim/photos/", "Kopia - photos - 2026-11-15 10:02"},
		{`C:\`, "Kopia - C: - 2026-11-15 10:02"},
		{"/", "Kopia - / - 2026-11-15 10:02"},
		{"", "Kopia - 2026-11-15 10:02"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			require.Equal(t, tc.want, formatSnapshotDriveLabel(tc.path, ts))
		})
	}
}

func Test_sourceLabelName_truncatesLongNames(t *testing.T) {
	long := strings.Repeat("x", maxSnapshotLabelNameLength+10)

	got := sourceLabelName("/data/" + long)

	require.Len(t, []rune(got), maxSnapshotLabelNameLength)
	require.True(t, strings.HasSuffix(got, "..."))
}
