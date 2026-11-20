//go:build !windows

package cli

// keepConsoleRunning does nothing: only Windows consoles stop programs while
// text is selected in them.
func keepConsoleRunning() func() {
	return func() {}
}
