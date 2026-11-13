package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	windowStateFileName  = "kopiaui-wails-windows.json"
	windowStateSaveDelay = time.Second

	// a restored window must show at least this much of itself on some screen.
	minVisibleWidth  = 120
	minVisibleHeight = 60
)

// rect is a rectangle in device-independent pixels, the unit Wails uses for
// both window bounds and screen work areas.
type rect struct {
	X, Y, Width, Height int
}

func (r rect) intersection(o rect) rect {
	x1, y1 := max(r.X, o.X), max(r.Y, o.Y)
	x2, y2 := min(r.X+r.Width, o.X+o.Width), min(r.Y+r.Height, o.Y+o.Height)

	if x2 <= x1 || y2 <= y1 {
		return rect{}
	}

	return rect{X: x1, Y: y1, Width: x2 - x1, Height: y2 - y1}
}

// screenInfo is the part of a screen that identifies a monitor layout.
type screenInfo struct {
	Bounds   rect
	WorkArea rect
	Scale    float32
}

// windowGeometry is the remembered size and position of a repository window.
type windowGeometry struct {
	Bounds    rect `json:"bounds"`
	Maximized bool `json:"maximized"`
}

// windowStateStore remembers window geometry per monitor layout, like
// Electron KopiaUI, so moving between e.g. a laptop screen and a docked
// setup restores the right placement for each.
type windowStateStore struct {
	mu        sync.Mutex
	path      string
	layouts   map[string]windowGeometry
	saveTimer *time.Timer
}

func newWindowStateStore(dir string) *windowStateStore {
	st := &windowStateStore{
		path:    filepath.Join(dir, windowStateFileName),
		layouts: map[string]windowGeometry{},
	}

	if b, err := os.ReadFile(st.path); err == nil {
		var data struct {
			Layouts map[string]windowGeometry `json:"layouts"`
		}

		if json.Unmarshal(b, &data) == nil && data.Layouts != nil {
			st.layouts = data.Layouts
		}
	}

	return st
}

// layoutKey identifies a monitor layout by the screens' positions, sizes and
// scale factors.
func layoutKey(screens []screenInfo) string {
	parts := make([]string, 0, len(screens))

	for _, s := range screens {
		parts = append(parts, fmt.Sprintf("%d,%d,%dx%d@%.2f", s.Bounds.X, s.Bounds.Y, s.Bounds.Width, s.Bounds.Height, s.Scale))
	}

	sort.Strings(parts)

	sum := sha256.Sum256([]byte(strings.Join(parts, ";")))

	return hex.EncodeToString(sum[:8])
}

// visibleOn reports whether enough of r is on one of the screens' work areas
// to grab and move the window.
func visibleOn(r rect, screens []screenInfo) bool {
	for _, s := range screens {
		in := r.intersection(s.WorkArea)
		if in.Width >= minVisibleWidth && in.Height >= minVisibleHeight {
			return true
		}
	}

	return false
}

// restore returns the geometry to apply for the current layout, if any.
func (st *windowStateStore) restore(screens []screenInfo) (windowGeometry, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()

	g, ok := st.layouts[layoutKey(screens)]
	if !ok || g.Bounds.Width <= 0 || g.Bounds.Height <= 0 || !visibleOn(g.Bounds, screens) {
		return windowGeometry{}, false
	}

	return g, true
}

// updateBounds records the normal (not maximized) bounds for the layout.
func (st *windowStateStore) updateBounds(screens []screenInfo, bounds rect) {
	st.update(screens, func(g *windowGeometry) {
		g.Bounds = bounds
		g.Maximized = false
	})
}

// updateMaximized records the maximized state, keeping the normal bounds so
// that un-maximizing after a restart returns to them.
func (st *windowStateStore) updateMaximized(screens []screenInfo, maximized bool) {
	st.update(screens, func(g *windowGeometry) { g.Maximized = maximized })
}

func (st *windowStateStore) update(screens []screenInfo, f func(g *windowGeometry)) {
	key := layoutKey(screens)

	st.mu.Lock()
	defer st.mu.Unlock()

	g := st.layouts[key]
	f(&g)
	st.layouts[key] = g

	// debounce: moving or resizing fires many events.
	if st.saveTimer == nil {
		st.saveTimer = time.AfterFunc(windowStateSaveDelay, st.save)
	}
}

func (st *windowStateStore) save() {
	st.mu.Lock()
	st.saveTimer = nil
	b, err := json.MarshalIndent(map[string]any{"layouts": st.layouts}, "", "  ")
	st.mu.Unlock()

	if err != nil {
		return
	}

	if err := os.WriteFile(st.path, b, 0o600); err != nil {
		slog.Warn("unable to save window state", "err", err)
	}
}

// flush writes pending changes immediately, e.g. on shutdown.
func (st *windowStateStore) flush() {
	st.mu.Lock()
	pending := st.saveTimer != nil
	if pending {
		st.saveTimer.Stop()
	}
	st.mu.Unlock()

	if pending {
		st.save()
	}
}
