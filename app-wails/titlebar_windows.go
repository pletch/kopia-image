package main

import (
	"log/slog"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/sys/windows"
)

// applyTitleBarColors sets the title bar colors of a window through DWM,
// which keeps them whether the window is active or not. Windows 10 ignores
// them and keeps its own.
func applyTitleBarColors(w *application.WebviewWindow, c *titleBarColors, active bool) {
	if c == nil || c.validate() != nil {
		return
	}

	hwnd := windows.HWND(uintptr(w.NativeWindow()))
	caption, _ := parseHexColor(c.Caption)

	for _, a := range []struct {
		attr  uint32
		color rgb
	}{
		{windows.DWMWA_CAPTION_COLOR, caption},
		{windows.DWMWA_TEXT_COLOR, c.textFor(active)},
		{windows.DWMWA_BORDER_COLOR, c.border()},
	} {
		v := a.color.colorref()

		//nolint:gosec // DwmSetWindowAttribute takes a pointer to the value and its size
		if err := windows.DwmSetWindowAttribute(hwnd, a.attr, unsafe.Pointer(&v), uint32(unsafe.Sizeof(v))); err != nil {
			slog.Debug("unable to set the title bar color", "attr", a.attr, "err", err)
		}
	}
}
