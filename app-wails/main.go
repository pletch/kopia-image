// Command kopiaui-wails is a prototype KopiaUI shell built with Wails v3.
//
// It replaces the Electron shell in app/ (tray, notifications, server process
// management and repository windows) while using the system webview (WebView2
// on Windows) instead of a bundled Chromium. The web UI itself is unchanged:
// it is served by `kopia server start --ui` and reached through an in-process
// proxy (see proxy.go).
package main

//go:generate go run ./tools/genres

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

const (
	// appName is used for toast notifications (shown as their source) and the
	// launch-at-startup entry. It differs from Electron KopiaUI's on purpose.
	appName          = "KopiaUI Wails"
	windowTitle      = "KopiaUI (Wails prototype)"
	singleInstanceID = "io.kopia.kopiaui-wails"
	logFileName      = "kopiaui-wails.log"

	// quitArg asks a running instance to quit; used by the installer.
	quitArg = "--quit"
)

var isWindows = runtime.GOOS == "windows"

// buildVersion is set at build time with -ldflags "-X main.buildVersion=...".
var buildVersion = "dev"

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}

	return false
}

//go:embed icons/kopia-tray.ico
var trayIconICO []byte

//go:embed icons/kopia-tray.png
var trayIconPNG []byte

// appIconICO is the window icon. Wails looks for an icon resource (ID 3) in the
// executable first and falls back to Options.Icon when there is none.
//
//go:embed icons/app.ico
var appIconICO []byte

// shell holds the application state shared by the tray, windows and servers.
type shell struct {
	app       *application.App
	tray      *application.SystemTray
	notifier  *notifications.NotificationService
	policy    *notificationPolicy
	windows   *windowStateStore
	configs   *configStore
	kopiaPath string
	logPath   string

	mu      sync.Mutex
	servers map[string]*repoServer

	tooltipMu    sync.Mutex
	tooltipReset *time.Timer

	trayDirty chan struct{}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configs, err := newConfigStore()
	if err != nil {
		return fmt.Errorf("unable to load configuration: %w", err)
	}

	s := &shell{
		configs:   configs,
		kopiaPath: defaultKopiaPath(),
		servers:   map[string]*repoServer{},
		trayDirty: make(chan struct{}, 1),
		notifier:  notifications.New(),
		policy:    newNotificationPolicy(),
		windows:   newWindowStateStore(configs.dir),
	}

	// Log output is buffered until this process knows it is the primary
	// instance, so a second launch cannot rotate or truncate the log of the
	// running one.
	logOut := &deferredWriter{}
	logger := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s.logPath = filepath.Join(s.configs.dir, logFileName)

	slog.SetDefault(logger)

	webviewDir := webviewDataDir()

	s.app = application.New(application.Options{
		Name:        appName,
		Description: "Kopia backup UI (Wails prototype)",
		Icon:        appIconICO,
		Logger:      logger,
		LogLevel:    slog.LevelInfo,
		Services:    []application.Service{application.NewService(s.notifier)},
		Assets: application.AssetOptions{
			Handler: &proxyHandler{host: s},
		},
		Windows: application.WindowsOptions{
			DisableQuitOnLastWindowClosed: true,
			WebviewUserDataPath:           webviewDir,
		},
		Linux: application.LinuxOptions{
			DisableQuitOnLastWindowClosed: true,
		},
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyAccessory,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: singleInstanceID,
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				if hasArg(data.Args, quitArg) {
					slog.Info("quit requested by another process")
					s.app.Quit()

					return
				}

				s.showAllWindows()
			},
		},
		OnShutdown: func() {
			s.windows.flush()
			s.stopAllServers()
		},
	})

	// Past this point this is the only instance: a second instance hands its
	// arguments to the first one and exits inside application.New.
	if hasArg(os.Args[1:], quitArg) {
		return nil // nothing to quit, no other instance is running
	}

	closeLog := s.openLog(logOut)
	defer closeLog()

	slog.Info("KopiaUI Wails prototype", "version", buildVersion)

	ensureWebViewProfileFree(webviewDir)

	s.notifier.OnNotificationResponse(func(result notifications.NotificationResult) {
		if result.Error != nil {
			return
		}

		if result.Response.ActionIdentifier == actionDismiss {
			return
		}

		// clicking the toast itself or "View Details" opens the repository window.
		if repoID, ok := result.Response.UserInfo["repo"].(string); ok {
			s.showRepoWindow(repoID)
		}
	})

	s.setupTray()

	s.app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		s.startup()
	})

	go s.trayUpdater()

	return s.app.Run()
}

// openLog rotates the previous log and directs log output to a new file.
func (s *shell) openLog(out *deferredWriter) func() {
	// keep one previous log for troubleshooting.
	_ = os.Rename(s.logPath, s.logPath+".1")

	f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		out.setTarget(os.Stderr)
		return func() {}
	}

	out.setTarget(f)

	return func() { _ = f.Close() }
}

// deferredWriter buffers writes until a target is set, then forwards them.
type deferredWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	target io.Writer
}

func (w *deferredWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.target == nil {
		return w.buf.Write(p)
	}

	return w.target.Write(p)
}

func (w *deferredWriter) setTarget(t io.Writer) {
	w.mu.Lock()
	defer w.mu.Unlock()

	_, _ = w.buf.WriteTo(t)
	w.target = t
}

func (s *shell) startup() {
	slog.Info("starting", "configDir", s.configs.dir, "portable", s.configs.portable, "kopia", s.kopiaPath)

	// Only a shared (non-portable) config directory conflicts with Electron KopiaUI.
	if !s.configs.portable && electronKopiaUIRunning() {
		s.app.Dialog.Warning().
			SetTitle(appName).
			SetMessage("Electron KopiaUI is running.\n\nQuit it from its tray icon before using this prototype. " +
				"Both would start servers for the same repositories and could run scheduled snapshots twice.").
			Show()
		s.app.Quit()

		return
	}

	if err := s.notifier.RegisterNotificationCategory(notifications.NotificationCategory{
		ID: errorCategoryID,
		Actions: []notifications.NotificationAction{
			{ID: actionViewDetails, Title: "View Details"},
			{ID: actionDismiss, Title: "Dismiss"},
		},
	}); err != nil {
		slog.Warn("unable to register notification actions", "err", err)
	}

	for _, id := range s.configs.list() {
		s.startServer(id)
	}

	startDebugProxy(s)

	if s.configs.firstRun {
		s.showAllWindows()
	}

	s.statusChanged()
}

func (s *shell) server(repoID string) *repoServer {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.servers[repoID]
}

func (s *shell) startServer(repoID string) {
	s.mu.Lock()
	srv := s.servers[repoID]
	if srv == nil {
		srv = newRepoServer(repoID, s)
		s.servers[repoID] = srv
	}
	s.mu.Unlock()

	if err := srv.start(); err != nil {
		slog.Error("unable to start server", "repo", repoID, "err", err)
		s.app.Dialog.Error().SetTitle(appName).SetMessage(err.Error()).Show()
	}
}

func (s *shell) stopAllServers() {
	s.mu.Lock()
	servers := make([]*repoServer, 0, len(s.servers))
	for _, srv := range s.servers {
		servers = append(servers, srv)
	}
	s.mu.Unlock()

	var wg sync.WaitGroup

	for _, srv := range servers {
		wg.Add(1)

		go func() {
			defer wg.Done()
			srv.stop()
		}()
	}

	wg.Wait()
}

func windowName(repoID string) string {
	return windowNamePrefix + repoID
}

func (s *shell) showRepoWindow(repoID string) {
	if w, ok := s.app.Window.GetByName(windowName(repoID)); ok {
		if w.IsMinimised() {
			w.Restore()
		}

		w.Show()
		w.Focus()
		s.pollAll()

		return
	}

	// Created hidden so the remembered geometry is applied before it appears.
	w := s.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            windowName(repoID),
		Title:           windowTitle,
		Width:           1000,
		Height:          700,
		MinWidth:        600,
		MinHeight:       400,
		URL:             "/",
		InitialPosition: application.WindowCentered,
		Hidden:          true,
	})

	geometry, restored := s.windows.restore(s.screens())
	if restored {
		b := geometry.Bounds
		w.SetBounds(application.Rect{X: b.X, Y: b.Y, Width: b.Width, Height: b.Height})
	}

	trackBounds := func(*application.WindowEvent) {
		if w.IsMinimised() || w.IsMaximised() {
			return
		}

		b := w.Bounds()
		s.windows.updateBounds(s.screens(), rect{X: b.X, Y: b.Y, Width: b.Width, Height: b.Height})
	}

	w.OnWindowEvent(events.Common.WindowDidMove, trackBounds)
	w.OnWindowEvent(events.Common.WindowDidResize, trackBounds)
	w.OnWindowEvent(events.Common.WindowMaximise, func(*application.WindowEvent) {
		s.windows.updateMaximized(s.screens(), true)
	})
	w.OnWindowEvent(events.Common.WindowUnMaximise, func(*application.WindowEvent) {
		s.windows.updateMaximized(s.screens(), false)
	})

	w.OnWindowEvent(events.Common.WindowFocus, func(*application.WindowEvent) {
		s.pollAll()
	})

	w.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) {
		// A repository added with "Connect To Another Repository..." that was never
		// connected is forgotten when its window closes, like in Electron KopiaUI.
		if s.configs.removeIfDisconnected(repoID) {
			s.mu.Lock()
			srv := s.servers[repoID]
			delete(s.servers, repoID)
			s.mu.Unlock()

			if srv != nil {
				go srv.stop()
			}

			s.statusChanged()
		}
	})

	w.Show()

	if restored && geometry.Maximized {
		w.Maximise()
	}

	s.pollAll()
}

// screens describes the current monitor layout for windowStateStore.
func (s *shell) screens() []screenInfo {
	all := s.app.Screen.GetAll()
	result := make([]screenInfo, 0, len(all))

	for _, sc := range all {
		result = append(result, screenInfo{
			Bounds:   rect{X: sc.Bounds.X, Y: sc.Bounds.Y, Width: sc.Bounds.Width, Height: sc.Bounds.Height},
			WorkArea: rect{X: sc.WorkArea.X, Y: sc.WorkArea.Y, Width: sc.WorkArea.Width, Height: sc.WorkArea.Height},
			Scale:    sc.ScaleFactor,
		})
	}

	return result
}

func (s *shell) showAllWindows() {
	for _, id := range s.configs.list() {
		s.showRepoWindow(id)
	}
}

func (s *shell) addRepository() {
	id := s.configs.addNew()
	s.startServer(id)
	s.showRepoWindow(id)
}

func (s *shell) anyWindowVisible() bool {
	for _, w := range s.app.Window.GetAll() {
		if strings.HasPrefix(w.Name(), windowNamePrefix) && w.IsVisible() && !w.IsMinimised() {
			return true
		}
	}

	return false
}

func (s *shell) pollAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, srv := range s.servers {
		srv.pollNow()
	}
}

func (s *shell) selectDirectory(repoID string) (string, error) {
	dlg := s.app.Dialog.OpenFile().
		CanChooseFiles(false).
		CanChooseDirectories(true).
		CanCreateDirectories(true).
		SetTitle("Select Directory")

	if w, ok := s.app.Window.GetByName(windowName(repoID)); ok {
		dlg = dlg.AttachToWindow(w)
	}

	return dlg.PromptForSingleSelection()
}

func (s *shell) browseDirectory(path string) {
	if err := s.app.Browser.OpenFile(path); err != nil {
		slog.Warn("unable to open directory", "path", path, "err", err)
	}
}

// notify handles a notification printed by a repository server: it applies the
// notification level and the per-source error cooldown, shows a toast and puts
// errors and warnings in the tray tooltip for a while (see notifications.go).
func (s *shell) notify(repoID string, msg notificationMessage) {
	d := s.policy.evaluate(s.configs.notificationLevel(), msg)

	if d.tooltip != "" {
		s.showTrayStatus(d.tooltip, d.tooltipFor)
	}

	if d.suppressedFor > 0 {
		slog.Info("suppressing repeated error notification",
			"repo", repoID, "title", d.title,
			"cooldownRemaining", d.suppressedFor.Round(time.Minute))

		return
	}

	if !d.showToast {
		return
	}

	opts := notifications.NotificationOptions{
		ID:    fmt.Sprintf("%s-%d", repoID, time.Now().UnixNano()),
		Title: d.title,
		Body:  d.body,
		Data:  map[string]any{"repo": repoID},
	}

	var err error

	if d.kind == kindError {
		opts.CategoryID = errorCategoryID
		err = s.notifier.SendNotificationWithActions(opts)
	} else {
		err = s.notifier.SendNotification(opts)
	}

	if err != nil {
		slog.Warn("unable to show notification", "err", err)
		return
	}

	slog.Info("notification shown", "repo", repoID, "kind", d.kind, "title", d.title)
}

// showTrayStatus shows text in the tray tooltip and restores the default
// tooltip after the given duration. A newer status replaces an older one.
func (s *shell) showTrayStatus(text string, duration time.Duration) {
	application.InvokeAsync(func() { s.tray.SetTooltip(text) })

	s.tooltipMu.Lock()
	defer s.tooltipMu.Unlock()

	if s.tooltipReset != nil {
		s.tooltipReset.Stop()
	}

	s.tooltipReset = time.AfterFunc(duration, func() {
		application.InvokeAsync(func() { s.tray.SetTooltip(defaultTrayTooltip) })
	})
}

// statusChanged schedules a tray menu refresh; refreshes are coalesced.
func (s *shell) statusChanged() {
	select {
	case s.trayDirty <- struct{}{}:
	default:
	}
}

func (s *shell) trayUpdater() {
	for range s.trayDirty {
		application.InvokeSync(s.rebuildTrayMenu)
		time.Sleep(250 * time.Millisecond)
	}
}

func (s *shell) setupTray() {
	s.tray = s.app.SystemTray.New()

	if isWindows {
		s.tray.SetIcon(trayIconICO)
	} else {
		s.tray.SetIcon(trayIconPNG)
	}

	s.tray.SetTooltip(defaultTrayTooltip)
	s.tray.OnClick(s.showAllWindows)
	s.tray.OnRightClick(s.tray.OpenMenu)

	s.rebuildTrayMenu()
}

func (s *shell) rebuildTrayMenu() {
	menu := s.app.NewMenu()

	menu.Add("KopiaUI Wails prototype " + buildVersion).SetEnabled(false)
	menu.AddSeparator()

	for _, id := range s.configs.list() {
		label := "<starting up>"
		if srv := s.server(id); srv != nil {
			label = srv.describe()
		}

		repoID := id

		menu.Add(label).
			SetTooltip(label + " (" + repoID + ")").
			OnClick(func(*application.Context) { s.showRepoWindow(repoID) })
	}

	menu.AddSeparator()
	menu.Add("Connect To Another Repository...").OnClick(func(*application.Context) { s.addRepository() })
	menu.AddSeparator()

	autostart, _ := s.app.Autostart.IsEnabled()

	menu.AddCheckbox("Launch At Startup", autostart).OnClick(func(*application.Context) {
		var err error
		if autostart {
			err = s.app.Autostart.Disable()
		} else {
			err = s.app.Autostart.Enable()
		}

		if err != nil {
			slog.Warn("unable to change launch at startup", "err", err)
		}

		s.statusChanged()
	})

	notif := menu.AddSubmenu("Notifications")
	level := s.configs.notificationLevel()

	for _, opt := range []struct {
		label string
		value int
	}{
		{"Enabled", levelAll},
		{"Warnings And Errors", levelWarningsAndErrors},
		{"Disabled", levelDisabled},
	} {
		value := opt.value

		notif.AddRadio(opt.label, level == value).OnClick(func(*application.Context) {
			s.configs.setNotificationLevel(value)
			s.statusChanged()
		})
	}

	menu.Add("Open Log File").OnClick(func(*application.Context) { s.browseDirectory(s.logPath) })
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(*application.Context) { s.app.Quit() })

	s.tray.SetMenu(menu)
}
