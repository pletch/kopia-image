package ntfsfs

import "golang.org/x/sys/windows"

// processIsAdmin tells whether this process runs with administrator rights:
// an elevated token, or the SYSTEM account of scheduled image tasks.
func processIsAdmin() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}
