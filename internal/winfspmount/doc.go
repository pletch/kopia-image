//go:build windows

// Package winfspmount mounts a kopia directory as a Windows drive through
// WinFsp (https://winfsp.dev), a file system driver that Windows programs see
// as a local disk. Unlike a WebDAV drive it has no file size limit and reads
// file data on demand instead of downloading whole files first.
//
// It is built only without cgo: with cgo, the WinFsp FUSE layer needs the
// WinFsp headers at build time. Builds with cgo, such as race-detector
// tests, report WinFsp as unavailable and mount with WebDAV instead.
package winfspmount
