//go:build !windows

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// applyTitleBarColors does nothing: other systems draw their own title bars.
func applyTitleBarColors(*application.WebviewWindow, *titleBarColors, bool) {}
