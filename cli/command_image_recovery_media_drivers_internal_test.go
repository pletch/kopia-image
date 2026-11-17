package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image"
)

func TestSelectDriverHosts(t *testing.T) {
	inv := []image.HostDrivers{{Host: "DESKTOP"}, {Host: "laptop"}, {Host: "nas"}}

	hosts := func(hs []image.HostDrivers) []string {
		var out []string
		for _, h := range hs {
			out = append(out, h.Host)
		}

		return out
	}

	got, err := selectDriverHosts(inv, []string{"all"}, "desktop", true)
	require.NoError(t, err)
	require.Equal(t, []string{"laptop", "nas"}, hosts(got), "this computer's drivers come from its driver store")

	got, err = selectDriverHosts(inv, []string{"ALL"}, "desktop", false)
	require.NoError(t, err)
	require.Equal(t, []string{"DESKTOP", "laptop", "nas"}, hosts(got))

	got, err = selectDriverHosts(inv, []string{"Laptop", "desktop", "laptop"}, "desktop", true)
	require.NoError(t, err)
	require.Equal(t, []string{"laptop", "DESKTOP"}, hosts(got), "a computer that is named is added")

	_, err = selectDriverHosts(inv, []string{"server"}, "desktop", true)
	require.ErrorContains(t, err, "no image of server stores drivers")

	_, err = selectDriverHosts(nil, []string{"all"}, "desktop", true)
	require.ErrorContains(t, err, "no image stores drivers yet")
}

func TestAddedDriverINFs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "net", "x64"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "net", "x64", "e1d.inf"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "net", "e1d.sys"), nil, 0o600))

	single := filepath.Join(dir, "iaStorVD.INF")
	require.NoError(t, os.WriteFile(single, nil, 0o600))

	failed := filepath.Join(dir, "failed.inf")
	require.NoError(t, os.WriteFile(failed, nil, 0o600))

	got := addedDriverINFs([]string{filepath.Join(dir, "net"), single, failed}, []string{filepath.Join(dir, "net"), single})
	require.Equal(t, []image.Driver{{INF: "e1d.inf"}, {INF: "iaStorVD.INF"}}, got)
}
