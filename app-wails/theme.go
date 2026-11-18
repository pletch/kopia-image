package main

import "github.com/wailsapp/wails/v3/pkg/application"

// The title bar takes the web UI theme's colors: see titlebar.go.

// windowBackground returns the color a new window shows until the web UI has
// painted: the UI's canvas color in dark mode, white otherwise.
func windowBackground(darkMode bool) application.RGBA {
	if darkMode {
		return application.NewRGB(0x08, 0x09, 0x0a)
	}

	return application.NewRGB(0xff, 0xff, 0xff)
}
