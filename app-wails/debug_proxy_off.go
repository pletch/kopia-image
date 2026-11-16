//go:build !debugproxy

package main

// startDebugProxy does nothing: the troubleshooting proxy, which gives access
// to the UI without its password, exists only in builds with the "debugproxy"
// tag (go build -tags debugproxy). See debug_proxy.go.
func startDebugProxy(*shell) {}
