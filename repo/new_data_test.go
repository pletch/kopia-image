package repo_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/repotesting"
	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/format"
	"github.com/kopia/kopia/repo/object"
)

func TestNewDataSince(t *testing.T) {
	cases := []struct {
		name          string
		before, after repo.NewData
		wantNew       int64
		wantStored    *int64
	}{
		{
			name:       "direct",
			before:     repo.NewData{Bytes: 10, StoredBytes: 5, StoredKnown: true},
			after:      repo.NewData{Bytes: 110, StoredBytes: 45, StoredKnown: true},
			wantNew:    100,
			wantStored: new(int64(40)),
		},
		{
			name:       "server reported first after the starting reading",
			before:     repo.NewData{Bytes: 3},
			after:      repo.NewData{Bytes: 100, StoredBytes: 40, StoredKnown: true},
			wantNew:    100,
			wantStored: new(int64(40)),
		},
		{
			name:    "server does not report",
			before:  repo.NewData{Bytes: 3},
			after:   repo.NewData{Bytes: 103},
			wantNew: 100,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotNew, gotStored := tc.after.Since(tc.before)
			require.Equal(t, tc.wantNew, gotNew)
			require.Equal(t, tc.wantStored, gotStored)
		})
	}
}

// countingWriter stands in for a writer through a repository server: it
// reports new data and counts its flushes.
type countingWriter struct {
	repo.RepositoryWriter

	flushes int
}

func (w *countingWriter) Flush(context.Context) error {
	w.flushes++
	return nil
}

func (w *countingWriter) NewData() repo.NewData {
	return repo.NewData{}
}

// plainWriter is a writer that neither is direct nor reports new data.
type plainWriter struct {
	repo.RepositoryWriter

	flushes int
}

func (w *plainWriter) Flush(context.Context) error {
	w.flushes++
	return nil
}

func TestFlushForNewData_flushesOnlyWritersThatReportOnFlush(t *testing.T) {
	ctx := testlogging.Context(t)

	server := &countingWriter{}
	require.NoError(t, repo.FlushForNewData(ctx, server))
	require.Equal(t, 1, server.flushes)

	plain := &plainWriter{}
	require.NoError(t, repo.FlushForNewData(ctx, plain))
	require.Zero(t, plain.flushes)
}

func TestFlushForNewData_directRepositoryCountsWithoutFlushing(t *testing.T) {
	ctx, env := repotesting.NewEnvironment(t, format.FormatVersion3)
	w := env.RepositoryWriter

	indexBlobs := func() int {
		n := 0

		require.NoError(t, w.BlobReader().ListBlobs(ctx, "x", func(blob.Metadata) error {
			n++
			return nil
		}))

		return n
	}

	before := repo.NewDataOf(w)
	indexesBefore := indexBlobs()

	ow := w.NewObjectWriter(ctx, object.WriterOptions{})
	_, err := ow.Write(bytes.Repeat([]byte("kopia "), 10000))
	require.NoError(t, err)
	_, err = ow.Result()
	require.NoError(t, err)
	require.NoError(t, ow.Close())

	require.NoError(t, repo.FlushForNewData(ctx, w))

	// nothing was flushed, so no index blob was written...
	require.Equal(t, indexesBefore, indexBlobs())

	// ...yet the new data is counted, stored size included
	newBytes, stored := repo.NewDataOf(w).Since(before)
	require.Positive(t, newBytes)
	require.NotNil(t, stored)
	require.Positive(t, *stored)
}
