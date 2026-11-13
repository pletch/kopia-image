package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Port of the custom toast notifications from app/public/notifications.js
// (see app/NOTIFICATIONS.md), adapted to the message format the Kopia server
// actually prints: {"subject", "body", "severity"} rather than
// {"type", "title", "description", "source"}.

// Notification levels, stored as {"level": N} in notifications.json - the same
// file and values Electron KopiaUI uses, so the setting carries over.
const (
	levelDisabled          = 0
	levelWarningsAndErrors = 1
	levelAll               = 2
)

// Kopia notification severities (notification/notification.go).
const (
	severityWarning = 10
	severityError   = 20
)

const (
	// notificationCooldown suppresses repeated error toasts for the same source,
	// e.g. a snapshot retried every 5 minutes while the network is down.
	notificationCooldown = 30 * time.Minute

	errorTooltipDuration   = 15 * time.Second
	warningTooltipDuration = 7 * time.Second
	defaultTrayTooltip     = "KopiaUI (Wails) - Click to manage backups"
	maxTrayTooltipLength   = 120
	trayTooltipBodyLength  = 50
	maxToastBodyLength     = 300

	errorCategoryID   = "kopia-error"
	actionViewDetails = "view-details"
	actionDismiss     = "dismiss"
)

type notificationKind string

const (
	kindError   notificationKind = "error"
	kindWarning notificationKind = "warning"
	kindInfo    notificationKind = "info"
)

func classify(severity int) notificationKind {
	switch {
	case severity >= severityError:
		return kindError
	case severity >= severityWarning:
		return kindWarning
	default:
		return kindInfo
	}
}

func levelAllows(level int, kind notificationKind) bool {
	switch level {
	case levelDisabled:
		return false
	case levelAll:
		return true
	default:
		return kind == kindError || kind == kindWarning
	}
}

// notificationDecision is what to do with one server notification.
type notificationDecision struct {
	kind  notificationKind
	title string
	body  string

	showToast bool

	// tooltip, when set, is shown in the tray for tooltipFor.
	tooltip    string
	tooltipFor time.Duration

	// suppressedFor is the remaining cooldown when an error toast was suppressed.
	suppressedFor time.Duration
}

// notificationPolicy holds the per-source cooldown state.
type notificationPolicy struct {
	mu          sync.Mutex
	now         func() time.Time
	lastFailure map[string]time.Time
}

func newNotificationPolicy() *notificationPolicy {
	return &notificationPolicy{now: time.Now, lastFailure: map[string]time.Time{}}
}

func (p *notificationPolicy) evaluate(level int, msg notificationMessage) notificationDecision {
	d := notificationDecision{
		kind:  classify(msg.Severity),
		title: strings.TrimSpace(msg.Subject),
		body:  summarizeBody(msg.Body),
	}

	if d.title == "" {
		d.title = "KopiaUI Notification"
	}

	if d.body == "" {
		d.body = "An event occurred in your backup"
	}

	key := sourceKey(msg)

	p.mu.Lock()
	defer p.mu.Unlock()

	if d.kind == kindInfo {
		// A successful report for the same source means it recovered, so the
		// next failure is reported immediately instead of waiting out the cooldown.
		delete(p.lastFailure, key)
	}

	if !levelAllows(level, d.kind) {
		return d
	}

	switch d.kind {
	case kindError:
		d.tooltip = trayTooltip(d.title, d.body)
		d.tooltipFor = errorTooltipDuration

		if last, ok := p.lastFailure[key]; ok {
			if elapsed := p.now().Sub(last); elapsed < notificationCooldown {
				d.suppressedFor = notificationCooldown - elapsed
				return d
			}
		}

		p.lastFailure[key] = p.now()

	case kindWarning:
		d.tooltip = trayTooltip(d.title, d.body)
		d.tooltipFor = warningTooltipDuration
	}

	d.showToast = true

	return d
}

// clearCooldown forgets the cooldown for one source, or for all when key is empty.
func (p *notificationPolicy) clearCooldown(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if key == "" {
		p.lastFailure = map[string]time.Time{}
		return
	}

	delete(p.lastFailure, key)
}

// sourceKey identifies what a notification is about for the cooldown: the
// snapshot paths listed in a snapshot report ("Path: ..." lines), otherwise the
// subject, which for generic errors names the operation.
func sourceKey(msg notificationMessage) string {
	var paths []string

	for _, line := range strings.Split(msg.Body, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "Path:"); ok {
			paths = append(paths, strings.TrimSpace(p))
		}
	}

	if len(paths) == 0 {
		return strings.TrimSpace(msg.Subject)
	}

	sort.Strings(paths)

	return strings.Join(paths, "|")
}

// summarizeBody condenses a notification body for a toast. Snapshot reports
// are reduced to their paths and errors; other bodies to their first lines.
func summarizeBody(body string) string {
	var (
		summary []string
		lines   []string
	)

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		lines = append(lines, line)

		if strings.HasPrefix(line, "Path:") || strings.HasPrefix(line, "Error:") {
			summary = append(summary, line)
		}
	}

	if len(summary) == 0 {
		summary = lines
	}

	return truncate(strings.Join(summary, "\n"), maxToastBodyLength)
}

func trayTooltip(title, body string) string {
	firstLine, _, _ := strings.Cut(body, "\n")

	return truncate(fmt.Sprintf("%s: %s", title, truncate(firstLine, trayTooltipBodyLength)), maxTrayTooltipLength)
}

// truncate shortens s to at most n runes, marking the cut with an ellipsis.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}

	r := []rune(s)

	return string(r[:n-1]) + "…"
}
