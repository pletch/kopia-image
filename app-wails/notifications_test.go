package main

import (
	"strings"
	"testing"
	"time"
)

// snapshotReport mimics notification/notifytemplate/snapshot-report.txt.
func snapshotReport(severity int, subject string, paths ...string) notificationMessage {
	var b strings.Builder

	for _, p := range paths {
		b.WriteString("Path: " + p + "\n\n  Status:      fatal\n  Start:       Fri Nov 13 15:00:00 EST 2026\n")

		if severity >= severityError {
			b.WriteString("  Error:       unable to connect to server: connection lost\n")
		}

		b.WriteString("\n")
	}

	b.WriteString("Generated at Fri Nov 13 15:00:05 EST 2026 by Kopia 20261113.0.0.\n\nhttps://kopia.io/")

	return notificationMessage{Subject: subject, Body: b.String(), Severity: severity}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestPolicy() (*notificationPolicy, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)}
	p := newNotificationPolicy()
	p.now = clk.now

	return p, clk
}

func TestLevels(t *testing.T) {
	cases := []struct {
		level    int
		severity int
		want     bool
	}{
		{levelDisabled, severityError, false},
		{levelWarningsAndErrors, severityError, true},
		{levelWarningsAndErrors, severityWarning, true},
		{levelWarningsAndErrors, 0, false},
		{levelWarningsAndErrors, -10, false},
		{levelAll, 0, true},
		{levelAll, -10, true},
	}

	for _, tc := range cases {
		p, _ := newTestPolicy()
		msg := notificationMessage{Subject: "s", Body: "b", Severity: tc.severity}

		if got := p.evaluate(tc.level, msg).showToast; got != tc.want {
			t.Errorf("level %d severity %d: showToast=%v, want %v", tc.level, tc.severity, got, tc.want)
		}
	}
}

func TestErrorCooldown(t *testing.T) {
	p, clk := newTestPolicy()
	fail := snapshotReport(severityError, "Failed to create a snapshot of C:\\Docs", `C:\Docs`)

	d := p.evaluate(levelWarningsAndErrors, fail)
	if !d.showToast || d.tooltip == "" || d.tooltipFor != errorTooltipDuration {
		t.Fatalf("first failure: %+v", d)
	}

	// retried 5 minutes later and failing again: no toast, tray still updated.
	clk.advance(5 * time.Minute)

	d = p.evaluate(levelWarningsAndErrors, fail)
	if d.showToast || d.suppressedFor != 25*time.Minute || d.tooltip == "" {
		t.Fatalf("repeat failure within cooldown: %+v", d)
	}

	// suppressed failures do not extend the cooldown.
	clk.advance(25 * time.Minute)

	if d = p.evaluate(levelWarningsAndErrors, fail); !d.showToast {
		t.Fatalf("failure after cooldown should show a toast: %+v", d)
	}
}

func TestCooldownIsPerSource(t *testing.T) {
	p, _ := newTestPolicy()

	if !p.evaluate(levelWarningsAndErrors, snapshotReport(severityError, "x", `C:\Docs`)).showToast {
		t.Fatal("first source should show")
	}

	if !p.evaluate(levelWarningsAndErrors, snapshotReport(severityError, "x", `D:\Photos`)).showToast {
		t.Fatal("a different source should not be affected by the first one's cooldown")
	}
}

func TestSuccessClearsCooldown(t *testing.T) {
	p, clk := newTestPolicy()

	fail := snapshotReport(severityError, "Failed", `C:\Docs`)
	ok := snapshotReport(0, "Successfully created a snapshot of C:\\Docs", `C:\Docs`)

	p.evaluate(levelWarningsAndErrors, fail)
	clk.advance(10 * time.Minute)

	// success is not shown at this level, but still resets the cooldown.
	if d := p.evaluate(levelWarningsAndErrors, ok); d.showToast {
		t.Fatalf("success should not show at warnings level: %+v", d)
	}

	clk.advance(time.Minute)

	if d := p.evaluate(levelWarningsAndErrors, fail); !d.showToast {
		t.Fatalf("failure after recovery should show immediately: %+v", d)
	}
}

func TestWarningsAreNotRateLimited(t *testing.T) {
	p, _ := newTestPolicy()
	warn := snapshotReport(severityWarning, "Created a snapshot with warnings", `C:\Docs`)

	for i := range 3 {
		d := p.evaluate(levelWarningsAndErrors, warn)
		if !d.showToast || d.tooltipFor != warningTooltipDuration {
			t.Fatalf("warning %d: %+v", i, d)
		}
	}
}

func TestDisabledLevelDoesNotStartCooldown(t *testing.T) {
	p, _ := newTestPolicy()
	fail := snapshotReport(severityError, "Failed", `C:\Docs`)

	if d := p.evaluate(levelDisabled, fail); d.showToast || d.tooltip != "" {
		t.Fatalf("disabled: %+v", d)
	}

	if d := p.evaluate(levelWarningsAndErrors, fail); !d.showToast {
		t.Fatalf("re-enabled notifications should show the next failure: %+v", d)
	}
}

func TestSourceKey(t *testing.T) {
	if got := sourceKey(snapshotReport(severityError, "x", `D:\b`, `C:\a`)); got != `C:\a|D:\b` {
		t.Errorf("multi-path key = %q", got)
	}

	msg := notificationMessage{Subject: "Kopia has encountered an error during Maintenance on host", Body: "details"}
	if got := sourceKey(msg); got != msg.Subject {
		t.Errorf("generic error key = %q", got)
	}
}

func TestSummarizeBody(t *testing.T) {
	got := summarizeBody(snapshotReport(severityError, "x", `C:\Docs`).Body)
	want := "Path: C:\\Docs\nError:       unable to connect to server: connection lost"

	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	if got := summarizeBody("\n first \n\n second \n"); got != "first\nsecond" {
		t.Errorf("plain summary = %q", got)
	}

	if got := summarizeBody(strings.Repeat("x", 1000)); len([]rune(got)) != maxToastBodyLength {
		t.Errorf("summary not truncated: %d runes", len([]rune(got)))
	}
}

func TestTrayTooltip(t *testing.T) {
	got := trayTooltip("Snapshot failed", "Path: C:\\Docs\nError: boom")
	if got != `Snapshot failed: Path: C:\Docs` {
		t.Errorf("tooltip = %q", got)
	}

	if got := trayTooltip(strings.Repeat("t", 200), "b"); len([]rune(got)) != maxTrayTooltipLength {
		t.Errorf("tooltip not truncated: %d runes", len([]rune(got)))
	}
}

func TestDefaults(t *testing.T) {
	p, _ := newTestPolicy()

	d := p.evaluate(levelAll, notificationMessage{Severity: 0})
	if d.title != "KopiaUI Notification" || d.body != "An event occurred in your backup" {
		t.Errorf("defaults = %+v", d)
	}
}
