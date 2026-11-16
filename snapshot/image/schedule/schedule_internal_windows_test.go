package schedule

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestIsAccessDenied(t *testing.T) {
	require.True(t, isAccessDenied([]byte("ERROR: Access is denied.\r\n")))
	require.True(t, isAccessDenied([]byte("ERROR: "+windows.ERROR_ACCESS_DENIED.Error()+"\r\n")))
	require.False(t, isAccessDenied([]byte("ERROR: The system cannot find the file specified.\r\n")))
}
