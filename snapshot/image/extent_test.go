package image

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeExtents(t *testing.T) {
	cases := []struct {
		name string
		in   []Extent
		want []Extent
	}{
		{"empty", nil, nil},
		{"drops empty", []Extent{{10, 0}}, nil},
		{"sorts", []Extent{{20, 5}, {0, 5}}, []Extent{{0, 5}, {20, 5}}},
		{"merges overlapping", []Extent{{0, 10}, {5, 10}}, []Extent{{0, 15}}},
		{"merges adjacent", []Extent{{0, 10}, {10, 10}}, []Extent{{0, 20}}},
		{"merges contained", []Extent{{0, 100}, {10, 10}, {50, 5}}, []Extent{{0, 100}}},
		{"keeps gaps", []Extent{{0, 10}, {11, 10}}, []Extent{{0, 10}, {11, 10}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]Extent(nil), tc.in...)

			got, err := NormalizeExtents(in)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.in, in, "input modified")
		})
	}
}

func TestNormalizeExtents_invalid(t *testing.T) {
	for _, e := range []Extent{{-1, 10}, {0, -1}, {1, 1<<63 - 1}} {
		_, err := NormalizeExtents([]Extent{e})
		require.Error(t, err, "%+v", e)
	}
}

func TestFreeExtents(t *testing.T) {
	const cs = 4096

	cases := []struct {
		name     string
		bitmap   []byte
		clusters int64
		want     []Extent
	}{
		{"all used", []byte{0xff, 0xff}, 16, nil},
		{"all free", []byte{0, 0}, 16, []Extent{{0, 16 * cs}}},
		{"lsb first", []byte{0b11111110}, 8, []Extent{{0, cs}}},
		{"msb free", []byte{0b01111111}, 8, []Extent{{7 * cs, cs}}},
		{"run across bytes", []byte{0b00001111, 0, 0b11110000}, 24, []Extent{{4 * cs, 16 * cs}}},
		{"partial last byte ignores padding bits", []byte{0xff, 0b00000001}, 12, []Extent{{9 * cs, 3 * cs}}},
		{"padding bits set", []byte{0xff, 0b11110000}, 12, []Extent{{8 * cs, 4 * cs}}},
		{"alternating", []byte{0b01010101}, 8, []Extent{{1 * cs, cs}, {3 * cs, cs}, {5 * cs, cs}, {7 * cs, cs}}},
		{"no clusters", nil, 0, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FreeExtents(tc.bitmap, tc.clusters, cs)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFreeExtents_matchesBitByBit(t *testing.T) {
	bitmap := make([]byte, 64)
	for i := range bitmap {
		bitmap[i] = byte(i * 37)
	}

	bitmap[10], bitmap[11], bitmap[30] = 0, 0, 0xff

	for _, clusters := range []int64{0, 1, 7, 8, 9, 63, 64, 65, 511, 512} {
		got, err := FreeExtents(bitmap, clusters, 512)
		require.NoError(t, err)

		var free []Extent

		for c := range clusters {
			if bitmap[c/8]&(1<<(c%8)) == 0 {
				free = append(free, Extent{c * 512, 512})
			}
		}

		want, err := NormalizeExtents(free)
		require.NoError(t, err)
		require.Equal(t, want, got, "clusters=%v", clusters)
	}
}

func TestFreeExtents_invalid(t *testing.T) {
	_, err := FreeExtents([]byte{0}, 9, 4096)
	require.ErrorContains(t, err, "need 2")

	_, err = FreeExtents(nil, 0, 0)
	require.Error(t, err)

	_, err = FreeExtents(nil, -1, 4096)
	require.Error(t, err)

	_, err = FreeExtents(nil, 1<<62, 4096)
	require.Error(t, err)
}
