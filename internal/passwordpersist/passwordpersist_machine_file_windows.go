package passwordpersist

import (
	"unsafe"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
)

// machineProtectionPurpose is mixed into the encryption so that the blob is
// only decrypted for this purpose.
var machineProtectionPurpose = []byte("kopia repository password")

const machineProtectionFlags = windows.CRYPTPROTECT_LOCAL_MACHINE | windows.CRYPTPROTECT_UI_FORBIDDEN

func dataBlob(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return &windows.DataBlob{}
	}

	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]} //nolint:gosec
}

// takeBlob copies and releases a blob that DPAPI allocated.
func takeBlob(b *windows.DataBlob) []byte {
	if b.Data == nil {
		return nil
	}

	// the blob is memory that DPAPI allocated with LocalAlloc
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(b.Data))) //nolint:errcheck,gosec

	return append([]byte(nil), unsafe.Slice(b.Data, b.Size)...) //nolint:gosec
}

func protectForMachine(plain []byte) ([]byte, error) {
	var out windows.DataBlob

	if err := windows.CryptProtectData(dataBlob(plain), nil, dataBlob(machineProtectionPurpose), 0, nil, machineProtectionFlags, &out); err != nil {
		return nil, errors.Wrap(err, "CryptProtectData")
	}

	return takeBlob(&out), nil
}

func unprotectForMachine(protected []byte) ([]byte, error) {
	var out windows.DataBlob

	if err := windows.CryptUnprotectData(dataBlob(protected), nil, dataBlob(machineProtectionPurpose), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, errors.Wrap(err, "CryptUnprotectData")
	}

	return takeBlob(&out), nil
}
