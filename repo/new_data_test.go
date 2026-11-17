package repo_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo"
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
