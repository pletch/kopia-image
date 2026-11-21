package blockdev

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func makeDeviceDescriptor(vendor, product string, busType uint32, removable bool) []byte {
	b := make([]byte, deviceDescriptorMinSize, deviceDescriptorMinSize+len(vendor)+len(product)+2) //nolint:makezero // the strings follow the fixed fields

	if removable {
		b[deviceDescriptorRemovableOff] = 1
	}

	binary.LittleEndian.PutUint32(b[deviceDescriptorBusTypeOff:], busType)

	if vendor != "" {
		binary.LittleEndian.PutUint32(b[deviceDescriptorVendorOff:], uint32(len(b))) //nolint:gosec
		b = append(append(b, vendor...), 0)
	}

	if product != "" {
		binary.LittleEndian.PutUint32(b[deviceDescriptorProductOff:], uint32(len(b))) //nolint:gosec
		b = append(append(b, product...), 0)
	}

	return b
}

func Test_decodeDeviceDescriptor(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want DiskDescriptor
	}{
		{"usb stick", makeDeviceDescriptor("SanDisk ", " Ultra Fit ", 7, true), DiskDescriptor{Model: "SanDisk Ultra Fit", Bus: BusUSB, Removable: true}},
		{"nvme without vendor", makeDeviceDescriptor("", "SHGP31-1000GM", 17, false), DiskDescriptor{Model: "SHGP31-1000GM", Bus: "NVMe"}},
		{"unknown bus", makeDeviceDescriptor("", "", 99, false), DiskDescriptor{Bus: "unknown"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeDeviceDescriptor(tc.in)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func Test_decodeDeviceDescriptor_Invalid(t *testing.T) {
	_, err := decodeDeviceDescriptor(make([]byte, deviceDescriptorMinSize-1))
	require.ErrorContains(t, err, "expected at least")

	// the serial number follows the other strings
	b := makeDeviceDescriptor("", "Ultra", 7, true)
	binary.LittleEndian.PutUint32(b[deviceDescriptorSerialOff:], uint32(len(b))) //nolint:gosec
	b = append(append(b, " 4C530001  "...), 0)

	got, err := decodeDeviceDescriptor(b)
	require.NoError(t, err)
	require.Equal(t, "4C530001", got.Serial)

	// a string offset past the end is ignored rather than read
	b = makeDeviceDescriptor("", "", 7, false)
	binary.LittleEndian.PutUint32(b[deviceDescriptorProductOff:], 4096)

	got, err = decodeDeviceDescriptor(b)
	require.NoError(t, err)
	require.Empty(t, got.Model)
}
