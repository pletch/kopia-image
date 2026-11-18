package ntfsfs

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
)

func TestClusterUses(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	tv := newTestVolume(t)
	oid := writeImage(ctx, t, env.RepositoryWriter, tv.img)

	v, err := Open(ctx, env.RepositoryWriter, oid, "p2-test", Options{})
	require.NoError(t, err)
	t.Cleanup(v.Close)

	clusters := int64(len(tv.img))/tv.clusterSize - 1
	mftLCN := tv.mftOffset / tv.clusterSize

	// duplicates and any order are accepted
	lcns := []int64{mftLCN, 0, mftLCN}
	for lcn := clusters - 1; lcn >= 0; lcn-- {
		lcns = append(lcns, lcn)
	}

	var progress []int64

	uses, err := v.ClusterUses(ctx, lcns, ClusterUseOptions{Progress: func(done, total int64) {
		require.LessOrEqual(t, done, total)
		progress = append(progress, done)
	}})
	require.NoError(t, err)
	require.NotEmpty(t, progress)
	require.Len(t, uses, int(clusters))

	require.Equal(t, ClusterUse{Path: "$Boot"}, uses[0])
	require.Equal(t, ClusterUse{Path: "$MFT"}, uses[mftLCN])

	perPath := map[string]int{}
	free := 0

	for _, u := range uses {
		if u.Path != "" {
			require.False(t, u.Free, "%v uses a free cluster", u.Path)
			perPath[u.Path]++
		}

		if u.Free {
			free++
		}
	}

	t.Logf("clusters per path: %v, free: %v", perPath, free)

	// ones.bin is NTFS-compressed, 2949120 bytes in fewer clusters
	require.Positive(t, perPath["ones.bin"])
	require.Less(t, int64(perPath["ones.bin"])*tv.clusterSize, int64(2949120))

	// nested paths are resolved through the parent directories
	require.Positive(t, perPath[`$Extend\$RmMetadata\$TxfLog\$TxfLog.blf`])
	require.Positive(t, free)
}

func TestClusterUses_none(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, repotesting.FormatNotImportant)
	oid := writeImage(ctx, t, env.RepositoryWriter, testImage(t))

	v, err := Open(ctx, env.RepositoryWriter, oid, "p2-test", Options{})
	require.NoError(t, err)
	t.Cleanup(v.Close)

	uses, err := v.ClusterUses(ctx, nil, ClusterUseOptions{})
	require.NoError(t, err)
	require.Empty(t, uses)
}
