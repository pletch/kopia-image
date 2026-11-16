package blockdev

import (
	"encoding/binary"
	"strings"

	"github.com/pkg/errors"
)

// Layout of STORAGE_DEVICE_DESCRIPTOR.
const (
	deviceDescriptorMinSize      = 36
	deviceDescriptorRemovableOff = 10
	deviceDescriptorVendorOff    = 12
	deviceDescriptorProductOff   = 16
	deviceDescriptorBusTypeOff   = 28
)

// busTypeNames are the STORAGE_BUS_TYPE values that disks commonly report.
var busTypeNames = map[uint32]string{
	1:  "SCSI",
	3:  "ATA",
	6:  "Fiber Channel",
	7:  "USB",
	8:  "RAID",
	9:  "iSCSI",
	10: "SAS",
	11: "SATA",
	12: "SD",
	13: "MMC",
	14: "Virtual",
	15: "Virtual disk file",
	16: "Storage Spaces",
	17: "NVMe",
}

// BusUSB is the Bus of disks attached through USB.
const BusUSB = "USB"

// DiskDescriptor identifies a physical disk to a person.
type DiskDescriptor struct {
	// Model is the vendor and product, e.g. "Samsung SSD 860 EVO 1TB".
	Model string

	// Bus is how the disk is attached, e.g. "NVMe", "SATA" or "USB".
	Bus string

	Removable bool
}

// decodeDeviceDescriptor decodes a STORAGE_DEVICE_DESCRIPTOR.
func decodeDeviceDescriptor(b []byte) (DiskDescriptor, error) {
	if len(b) < deviceDescriptorMinSize {
		return DiskDescriptor{}, errors.Errorf("device descriptor is %v bytes, expected at least %v", len(b), deviceDescriptorMinSize)
	}

	vendor := descriptorString(b, binary.LittleEndian.Uint32(b[deviceDescriptorVendorOff:]))
	product := descriptorString(b, binary.LittleEndian.Uint32(b[deviceDescriptorProductOff:]))
	busType := binary.LittleEndian.Uint32(b[deviceDescriptorBusTypeOff:])

	bus, ok := busTypeNames[busType]
	if !ok {
		bus = "unknown"
	}

	return DiskDescriptor{
		Model:     strings.TrimSpace(vendor + " " + product),
		Bus:       bus,
		Removable: b[deviceDescriptorRemovableOff] != 0,
	}, nil
}

// descriptorString returns the NUL-terminated ASCII string at an offset of a
// descriptor; offset 0 means that the device does not report the string.
func descriptorString(b []byte, off uint32) string {
	if off == 0 || int(off) >= len(b) {
		return ""
	}

	s := b[off:]
	for i, c := range s {
		if c == 0 {
			s = s[:i]
			break
		}
	}

	return strings.TrimSpace(string(s))
}
