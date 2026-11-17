package main

import "github.com/wailsapp/wails/v3/pkg/application"

// windowsTitleBarTheme colours the Windows title bar in dark mode to match the
// web UI's default dark theme ("linear"): the title bar uses the sidebar's
// surface colour so the two read as one strip. Light mode keeps the system
// title bar.
var windowsTitleBarTheme = application.ThemeSettings{
	DarkModeActive: &application.WindowTheme{
		TitleBarColour:  application.NewRGBPtr(0x0f, 0x10, 0x12),
		TitleTextColour: application.NewRGBPtr(0xf7, 0xf8, 0xf8),
		BorderColour:    application.NewRGBPtr(0x1f, 0x20, 0x23),
	},
	DarkModeInactive: &application.WindowTheme{
		TitleBarColour:  application.NewRGBPtr(0x0f, 0x10, 0x12),
		TitleTextColour: application.NewRGBPtr(0x8a, 0x8f, 0x98),
		BorderColour:    application.NewRGBPtr(0x1f, 0x20, 0x23),
	},
}

// windowBackground returns the colour a new window shows until the web UI has
// painted: the UI's canvas colour in dark mode, white otherwise.
func windowBackground(darkMode bool) application.RGBA {
	if darkMode {
		return application.NewRGB(0x08, 0x09, 0x0a)
	}

	return application.NewRGB(0xff, 0xff, 0xff)
}
