//go:build windows

package main

import (
	"testing"
	"unsafe"
)

// TestShellExecuteInfoSize checks shellExecuteInfo against the size of
// SHELLEXECUTEINFOW, which ShellExecuteExW checks before doing anything.
func TestShellExecuteInfoSize(t *testing.T) {
	want := uintptr(112) // 64-bit
	if unsafe.Sizeof(uintptr(0)) == 4 {
		want = 60
	}

	if got := unsafe.Sizeof(shellExecuteInfo{}); got != want {
		t.Fatalf("shellExecuteInfo is %v bytes, SHELLEXECUTEINFOW %v", got, want)
	}
}
