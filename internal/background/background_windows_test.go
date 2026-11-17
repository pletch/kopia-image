package background_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/internal/background"
)

// TestEnter checks that the process enters background mode and leaves it:
// Windows refuses to enter background mode twice.
func TestEnter(t *testing.T) {
	leave, err := background.Enter()
	require.NoError(t, err)

	// already in background mode
	require.Error(t, windows.SetPriorityClass(windows.CurrentProcess(), windows.PROCESS_MODE_BACKGROUND_BEGIN))

	leave()

	// left: entering again works
	leave, err = background.Enter()
	require.NoError(t, err)

	leave()
}
