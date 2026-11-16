package schedule

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// TestJoinArgs_matchesWindows checks the quoting against the Windows
// command line parser.
func TestJoinArgs_matchesWindows(t *testing.T) {
	for _, args := range [][]string{
		{"plain", "with space", `C:\Program Files\x\`, `quote"inside`, `back\\"slashes`, "", `trailing\\`},
	} {
		parsed, err := windows.DecomposeCommandLine("prog.exe " + JoinArgs(args))
		require.NoError(t, err)
		require.Equal(t, args, parsed[1:])
	}
}
