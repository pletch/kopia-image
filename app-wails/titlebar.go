package main

import (
	"errors"
	"fmt"
	"strconv"
)

// titleBarColors are the window title bar colors, as "#rrggbb", taken from
// the web UI's theme so that the title bar matches the page whatever the
// Windows theme is.
type titleBarColors struct {
	Caption string `json:"caption"`
	Text    string `json:"text"`
}

var errInvalidColor = errors.New(`colors must be "#rrggbb"`)

type rgb struct{ r, g, b uint8 }

func parseHexColor(s string) (rgb, error) {
	if len(s) != 7 || s[0] != '#' {
		return rgb{}, errInvalidColor
	}

	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return rgb{}, errInvalidColor
	}

	return rgb{uint8(v >> 16), uint8(v >> 8), uint8(v)}, nil //nolint:gosec,mnd
}

func (c rgb) String() string {
	return fmt.Sprintf("#%02x%02x%02x", c.r, c.g, c.b)
}

// mix returns c moved toward other by weight (0 to 1).
func (c rgb) mix(other rgb, weight float64) rgb {
	m := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*weight + 0.5) } //nolint:mnd

	return rgb{m(c.r, other.r), m(c.g, other.g), m(c.b, other.b)}
}

// colorref returns the color as a Windows COLORREF, 0x00BBGGRR.
func (c rgb) colorref() uint32 {
	return uint32(c.b)<<16 | uint32(c.g)<<8 | uint32(c.r)
}

// validate checks both colors.
func (t titleBarColors) validate() error {
	if _, err := parseHexColor(t.Caption); err != nil {
		return err
	}

	_, err := parseHexColor(t.Text)

	return err
}

// textFor returns the title text color: the theme's text color when the
// window is active, and when it is not, a dimmer one, as Windows dims it.
func (t titleBarColors) textFor(active bool) rgb {
	text, _ := parseHexColor(t.Text)
	if active {
		return text
	}

	caption, _ := parseHexColor(t.Caption)

	return text.mix(caption, 0.45) //nolint:mnd
}

// border returns the window border color: the caption color moved slightly
// toward the text, so that the window edge shows on a background of the
// same color.
func (t titleBarColors) border() rgb {
	text, _ := parseHexColor(t.Text)
	caption, _ := parseHexColor(t.Caption)

	return caption.mix(text, 0.12) //nolint:mnd
}

// defaultTitleBarColors are used until the web UI reports its theme: the
// default dark theme's when Windows is in dark mode, none otherwise.
func defaultTitleBarColors(darkMode bool) *titleBarColors {
	if !darkMode {
		return nil
	}

	return &titleBarColors{Caption: "#0f1012", Text: "#f7f8f8"}
}
