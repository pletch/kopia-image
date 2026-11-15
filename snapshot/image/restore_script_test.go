package image

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update", false, "update golden files")

func TestWriteRestoreScript_golden(t *testing.T) {
	var sb strings.Builder

	require.NoError(t, WriteRestoreScript(&sb, testLayout(), testMetadata(t)))

	golden := filepath.Join("testdata", "restore.sh.golden")

	if *updateGolden {
		require.NoError(t, os.WriteFile(golden, []byte(sb.String()), 0o600))
	}

	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	require.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), sb.String())
}

func TestWriteRestoreScript_invalidInput(t *testing.T) {
	l := testLayout()
	l.SectorSize = 3

	require.ErrorContains(t, WriteRestoreScript(&strings.Builder{}, l, testMetadata(t)), "invalid layout")

	m := testMetadata(t)
	m.Volumes[0].PartitionNumber = 9

	require.ErrorContains(t, WriteRestoreScript(&strings.Builder{}, testLayout(), m), "invalid metadata")
}

func TestShellQuote(t *testing.T) {
	require.Equal(t, `'abc'`, shellQuote("abc"))
	require.Equal(t, `''`, shellQuote(""))
	require.Equal(t, `'Tim'\''s $(rm -rf /) "x"'`, shellQuote(`Tim's $(rm -rf /) "x"`))
}
