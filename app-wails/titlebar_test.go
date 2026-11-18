package main

import (
	"net/http"
	"testing"
)

func TestParseHexColor(t *testing.T) {
	c, err := parseHexColor("#0F1012")
	if err != nil || c != (rgb{0x0f, 0x10, 0x12}) || c.String() != "#0f1012" {
		t.Fatalf("got %v, %v", c, err)
	}

	if c.colorref() != 0x0012100f {
		t.Fatalf("COLORREF is 0x00BBGGRR, got %#x", c.colorref())
	}

	for _, bad := range []string{"", "0f1012", "#0f10", "#0f10123", "#gg1012", "red", "rgb(1,2,3)"} {
		if _, err := parseHexColor(bad); err == nil {
			t.Errorf("parseHexColor(%q) succeeded", bad)
		}
	}
}

func TestTitleBarColors_inactiveTextIsDimmer(t *testing.T) {
	c := titleBarColors{Caption: "#000000", Text: "#ffffff"}

	if got := c.textFor(true); got != (rgb{255, 255, 255}) {
		t.Fatalf("active text %v", got)
	}

	inactive := c.textFor(false)
	if inactive.r == 255 || inactive.r == 0 {
		t.Fatalf("inactive text %v is not between the text and the caption", inactive)
	}

	if b := c.border(); b.r == 0 || b.r > 64 {
		t.Fatalf("border %v should be close to the caption", b)
	}
}

func TestShim_windowColorsAreRememberedAndValidated(t *testing.T) {
	sh := switchTestShell(t, "repository")

	if rec := mainWindowRequest(t, sh, "window-colors", `{"caption":"#222222","text":"#cfcfcf"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("got %v: %v", rec.Code, rec.Body.String())
	}

	if got := sh.configs.titleBarColors(); got == nil || *got != (titleBarColors{Caption: "#222222", Text: "#cfcfcf"}) {
		t.Fatalf("got %+v", got)
	}

	// the next start reads them back, with the shown repository
	sh.configs.setActiveRepository("repository")

	c2 := &configStore{dir: sh.configs.dir, repos: map[string]bool{"repository": true}}
	c2.loadState()

	if got := c2.titleBarColors(); got == nil || got.Caption != "#222222" {
		t.Fatalf("after restart: %+v", got)
	}

	if rec := mainWindowRequest(t, sh, "window-colors", `{"caption":"red","text":"#cfcfcf"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("an invalid color: got %v", rec.Code)
	}

	if got := sh.configs.titleBarColors(); got.Caption != "#222222" {
		t.Fatalf("an invalid color replaced the colors: %+v", got)
	}
}
