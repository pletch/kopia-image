package passwordpersist

import (
	"context"
	"encoding/base64"
	"os"

	"github.com/pkg/errors"
)

// machineProtectedPrefix marks a password file whose content is encrypted
// with a key of this computer rather than only base64-encoded.
const machineProtectedPrefix = "dpapi:"

// machinePayloadVersion starts the encrypted payload, which also keeps the
// payload of an empty password from being empty: DPAPI refuses empty input.
const machinePayloadVersion = 1

// MachineFile is a Strategy that persists the password in a file next to the
// repository config file, like File, but encrypted with a key that never
// leaves this computer (Windows DPAPI, machine scope). A copy of the file is
// useless on another computer. Every account on this computer that can read
// the file can still decrypt it, so the file's permissions keep protecting
// it. File reads passwords persisted this way.
//
// On platforms without DPAPI it persists passwords exactly like File.
func MachineFile() Strategy {
	return machineFilePasswordStorage{}
}

type machineFilePasswordStorage struct {
	filePasswordStorage
}

func (machineFilePasswordStorage) PersistPassword(ctx context.Context, configFile, password string) error {
	protected, err := protectForMachine(append([]byte{machinePayloadVersion}, password...))
	if errors.Is(err, errMachineProtectionNotSupported) {
		return filePasswordStorage{}.PersistPassword(ctx, configFile, password)
	}

	if err != nil {
		return errors.Wrap(err, "error protecting the password")
	}

	fn := passwordFileName(configFile)
	log(ctx).Debugf("Saving protected password to file %v.", fn)

	//nolint:wrapcheck
	return os.WriteFile(fn, []byte(machineProtectedPrefix+base64.StdEncoding.EncodeToString(protected)), passwordFileMode)
}

var errMachineProtectionNotSupported = errors.New("machine-scope password protection is not supported on this platform")

// machinePayloadPassword returns the password in a decrypted payload.
func machinePayloadPassword(payload []byte) ([]byte, error) {
	if len(payload) == 0 || payload[0] != machinePayloadVersion {
		return nil, errors.New("unsupported protected password format")
	}

	return payload[1:], nil
}
