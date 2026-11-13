//go:build !windows

package main

func webviewDataDir() string { return "" }

func ensureWebViewProfileFree(string) {}
