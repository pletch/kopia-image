// Command kopiaui-wails is a prototype KopiaUI shell built with Wails v3.
//
// It replaces the Electron shell in app/ (tray, notifications, server process
// management and repository windows) while using the system webview (WebView2
// on Windows) instead of a bundled Chromium. The web UI itself is unchanged:
// it is served by `kopia server start --ui` and reached through an in-process
// proxy (see proxy.go).
package main

import (
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
	appName               = "KopiaUI"
	singleInstanceID      = "io.kopia.kopiaui-wails"
	logFileName           = "kopiaui-wails.log"
	maxNotificationLength = 500
	warningSeverity       = 10
)

var isWindows = runtime.GOOS == "windows"

//go:embed icons/kopia-tray.ico
var trayIconICO []byte

//go:embed icons/kopia-tray.png
var trayIconPNG []byte

// shell holds the application state shared by the tray, windows and servers.
type shell struct {
	app       *application.App
	tray      *application.SystemTray
	notifier  *notifications.NotificationService
	configs   *configStore
	kopiaPath string
	logPath   string

	mu      sync.Mutex
	servers map[string]*repoServer

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
	}

	logger, closeLog := s.openLog()
	defer closeLog()

	slog.SetDefault(logger)

	s.app = application.New(application.Options{
		Name:        appName,
		Description: "Kopia backup UI (Wails prototype)",
		Logger:      logger,
		LogLevel:    slog.LevelInfo,
		Services:    []application.Service{application.NewService(s.notifier)},
		Assets: application.AssetOptions{
			Handler: &proxyHandler{host: s},
		},
		Windows: application.WindowsOptions{
			DisableQuitOnLastWindowClosed: true,
		},
		Linux: application.LinuxOptions{
			DisableQuitOnLastWindowClosed: true,
		},
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyAccessory,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: singleInstanceID,
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				s.showAllWindows()
			},
		},
		OnShutdown: s.stopAllServers,
	})

	s.notifier.OnNotificationResponse(func(result notifications.NotificationResult) {
		if result.Error != nil {
			return
		}

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

func (s *shell) openLog() (*slog.Logger, func()) {
	s.logPath = filepath.Join(s.configs.dir, logFileName)

	// keep one previous log for troubleshooting.
	_ = os.Rename(s.logPath, s.logPath+".1")

	f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return slog.New(slog.NewTextHandler(os.Stderr, nil)), func() {}
	}

	var w io.Writer = f

	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})), func() { _ = f.Close() }
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

	w := s.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            windowName(repoID),
		Title:           appName,
		Width:           1000,
		Height:          700,
		MinWidth:        600,
		MinHeight:       400,
		URL:             "/",
		InitialPosition: application.WindowCentered,
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
	s.pollAll()
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

// notify shows a desktop notification for a message printed by a repository
// server, filtered by the notification level chosen in the tray menu.
func (s *shell) notify(repoID string, msg notificationMessage) {
	switch s.configs.notificationLevel() {
	case notifyDisabled:
		return
	case notifyWarnErrors:
		if msg.Severity < warningSeverity {
			return
		}
	}

	body := msg.Body
	if len(body) > maxNotificationLength {
		body = body[:maxNotificationLength] + "..."
	}

	err := s.notifier.SendNotification(notifications.NotificationOptions{
		ID:    fmt.Sprintf("%s-%d", repoID, time.Now().UnixNano()),
		Title: msg.Subject,
		Body:  body,
		Data:  map[string]any{"repo": repoID},
	})
	if err != nil {
		slog.Warn("unable to show notification", "err", err)
	}
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

	s.tray.SetTooltip(appName)
	s.tray.OnClick(s.showAllWindows)
	s.tray.OnRightClick(s.tray.OpenMenu)

	s.rebuildTrayMenu()
}

func (s *shell) rebuildTrayMenu() {
	menu := s.app.NewMenu()

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

	for _, opt := range []struct{ label, value string }{
		{"Enabled", notifyAll},
		{"Warnings And Errors", notifyWarnErrors},
		{"Disabled", notifyDisabled},
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
