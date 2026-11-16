package passwordpersist_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/internal/testlogging"
)

func TestMachineFile_roundTrip(t *testing.T) {
	ctx := testlogging.Context(t)
	configFile := filepath.Join(t.TempDir(), "repository.config")

	for _, password := range []string{"some-password", "", "pässwörd with spaces"} {
		require.NoError(t, passwordpersist.MachineFile().PersistPassword(ctx, configFile, password))

		// the plain file strategy, which scheduled runs use, reads it back
		got, err := passwordpersist.File().GetPassword(ctx, configFile)
		require.NoError(t, err)
		require.Equal(t, password, got)
	}

	require.NoError(t, passwordpersist.MachineFile().DeletePassword(ctx, configFile))

	_, err := passwordpersist.File().GetPassword(ctx, configFile)
	require.ErrorIs(t, err, passwordpersist.ErrPasswordNotFound)
}

func TestMachineFile_doesNotStoreThePasswordEncoded(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("machine-scope protection is only available on Windows")
	}

	ctx := testlogging.Context(t)
	configFile := filepath.Join(t.TempDir(), "repository.config")

	require.NoError(t, passwordpersist.MachineFile().PersistPassword(ctx, configFile, "some-password"))

	b, err := os.ReadFile(configFile + ".kopia-password")
	require.NoError(t, err)

	encoded, ok := strings.CutPrefix(string(b), "dpapi:")
	require.True(t, ok, "protected files are marked")

	blob, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	require.NotContains(t, string(blob), "some-password")

	// a damaged file is an error, not a wrong password
	require.NoError(t, os.WriteFile(configFile+".kopia-password", []byte("dpapi:"+base64.StdEncoding.EncodeToString(blob[:len(blob)-1])), 0o600))

	_, err = passwordpersist.File().GetPassword(ctx, configFile)
	require.Error(t, err)
}
