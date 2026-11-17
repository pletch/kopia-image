//go:build !windows

package ntfsfs

import "os"

// processIsAdmin tells whether this process runs as root.
func processIsAdmin() bool {
	return os.Geteuid() == 0
}
