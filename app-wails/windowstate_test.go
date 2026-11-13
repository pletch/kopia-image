package main

import (
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var laptop = []screenInfo{{
	Bounds:   rect{0, 0, 1920, 1080},
	WorkArea: rect{0, 0, 1920, 1032},
	Scale:    1.5,
}}

var docked = []screenInfo{
	{Bounds: rect{0, 0, 2560, 1440}, WorkArea: rect{0, 0, 2560, 1392}, Scale: 1},
	{Bounds: rect{2560, 0, 1920, 1080}, WorkArea: rect{2560, 0, 1920, 1032}, Scale: 1},
}

func TestLayoutKey(t *testing.T) {
	if layoutKey(laptop) == layoutKey(docked) {
		t.Fatal("different layouts must have different keys")
	}

	reversed := []screenInfo{docked[1], docked[0]}
	if layoutKey(docked) != layoutKey(reversed) {
		t.Fatal("screen order must not change the key")
	}

	scaled := []screenInfo{laptop[0]}
	scaled[0].Scale = 1.25

	if layoutKey(laptop) == layoutKey(scaled) {
		t.Fatal("a scale change must change the key")
	}
}

func TestVisibleOn(t *testing.T) {
	cases := []struct {
		r    rect
		want bool
	}{
		{rect{100, 100, 1000, 700}, true},
		{rect{1900, 100, 1000, 700}, false}, // only 20px on screen
		{rect{1700, 100, 1000, 700}, true},  // 220px on screen
		{rect{-5000, -5000, 1000, 700}, false},
		{rect{3000, 100, 1000, 700}, false}, // second monitor no longer attached
	}

	for _, tc := range cases {
		if got := visibleOn(tc.r, laptop); got != tc.want {
			t.Errorf("visibleOn(%+v) = %v, want %v", tc.r, got, tc.want)
		}
	}

	if !visibleOn(rect{3000, 100, 1000, 700}, docked) {
		t.Error("window on the second monitor should be visible when docked")
	}
}

func TestWindowStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	st := newWindowStateStore(dir)
	st.updateBounds(laptop, rect{100, 50, 1200, 800})
	st.updateBounds(docked, rect{2700, 40, 1400, 900})
	st.updateMaximized(docked, true)
	st.flush()

	st2 := newWindowStateStore(dir)

	g, ok := st2.restore(laptop)
	if !ok || g.Bounds != (rect{100, 50, 1200, 800}) || g.Maximized {
		t.Fatalf("laptop geometry = %+v, %v", g, ok)
	}

	g, ok = st2.restore(docked)
	if !ok || g.Bounds != (rect{2700, 40, 1400, 900}) || !g.Maximized {
		t.Fatalf("docked geometry = %+v, %v", g, ok)
	}

	// moving after un-maximizing records normal bounds again.
	st2.updateBounds(docked, rect{10, 10, 900, 600})

	if g, _ = st2.restore(docked); g.Maximized {
		t.Fatal("moving a normal window should clear the maximized flag")
	}
}

func TestWindowStateIgnoresUnknownLayoutAndOffscreen(t *testing.T) {
	st := newWindowStateStore(t.TempDir())

	if _, ok := st.restore(laptop); ok {
		t.Fatal("nothing saved yet")
	}

	st.updateBounds(laptop, rect{-3000, 0, 800, 600})

	if _, ok := st.restore(laptop); ok {
		t.Fatal("off-screen geometry must not be restored")
	}
}

func TestWindowStateDebouncedSave(t *testing.T) {
	dir := t.TempDir()
	st := newWindowStateStore(dir)

	st.updateBounds(laptop, rect{1, 2, 800, 600})
	time.Sleep(windowStateSaveDelay + 500*time.Millisecond)

	if _, ok := newWindowStateStore(dir).restore(laptop); !ok {
		t.Fatal("geometry should be saved after the debounce delay")
	}
}

func TestStartupPage(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStartupPage(rec, startupProblem{elapsed: 5 * time.Second})

	body := rec.Body.String()
	if !strings.Contains(body, `http-equiv="refresh"`) || !strings.Contains(body, "Starting Kopia server") || !strings.Contains(body, "(5s)") {
		t.Fatalf("starting page:\n%s", body)
	}

	rec = httptest.NewRecorder()
	writeStartupPage(rec, startupProblem{
		failed: true,
		reason: "The Kopia server exited unexpectedly (exit status 1).",
		logs:   []string{"ERROR unable to open repository", "<script>alert(1)</script>"},
	})

	body = rec.Body.String()
	for _, want := range []string{"exited unexpectedly", "Restart Server", "/__kopiaui/restart", "unable to open repository", "&lt;script&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("error page missing %q", want)
		}
	}

	if strings.Contains(body, "<script>alert") || strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("error page must escape log lines and must not auto-refresh")
	}
}

func TestStartupProblem(t *testing.T) {
	s := &repoServer{logLines: []string{"a", "b"}}

	if p := s.startupProblem(); !p.failed || p.reason != "The Kopia server is not running." || len(p.logs) != 2 {
		t.Fatalf("not started: %+v", p)
	}

	s.lastExit = "The Kopia server exited unexpectedly (exit status 2)."
	if p := s.startupProblem(); p.reason != s.lastExit {
		t.Fatalf("exited: %+v", p)
	}
}

func TestStartupProblemTimeout(t *testing.T) {
	s := &repoServer{cmd: &exec.Cmd{}, startedAt: time.Now().Add(-5 * time.Second)}

	if p := s.startupProblem(); p.failed || p.elapsed < 5*time.Second {
		t.Fatalf("still starting: %+v", p)
	}

	s.startedAt = time.Now().Add(-serverStartTimeout - time.Second)

	if p := s.startupProblem(); !p.failed || !strings.Contains(p.reason, "did not finish starting") {
		t.Fatalf("timed out: %+v", p)
	}
}
