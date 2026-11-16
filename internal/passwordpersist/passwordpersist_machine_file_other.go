//go:build !windows

package passwordpersist

func protectForMachine([]byte) ([]byte, error) {
	return nil, errMachineProtectionNotSupported
}

func unprotectForMachine([]byte) ([]byte, error) {
	return nil, errMachineProtectionNotSupported
}
