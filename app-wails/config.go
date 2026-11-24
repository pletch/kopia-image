package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	configFileSuffix = ".config"
	defaultRepoID    = "repository"

	// notificationsFileName is shared with Electron KopiaUI (app/public/notifications.js).
	notificationsFileName = "notifications.json"

	// stateFileName holds which repository the window shows; it is this
	// program's own.
	stateFileName = "kopia-image-state.json"

	// legacyStateFileName is stateFileName before the rename to Kopia-Image.
	legacyStateFileName = "kopiaui-wails-state.json"

	stateFilePermissions = 0o600
)

type shellState struct {
	ActiveRepository string `json:"activeRepository"`

	// TitleBar is the title bar colors of the web UI's theme, reported by
	// the page, so that the window opens with them before the page loads.
	TitleBar *titleBarColors `json:"titleBar,omitempty"`
}

// configStore mirrors app/public/config.js: it discovers repository configs
// (<id>.config) in the same directory KopiaUI and the Kopia CLI use, so this
// shell picks up repositories that were connected with Electron KopiaUI.
type configStore struct {
	mu       sync.Mutex
	dir      string
	portable bool
	firstRun bool
	repos    map[string]bool
	level    int
	state    shellState
}

type notificationSettings struct {
	Level int `json:"level"`
}

// portableConfigDirs returns the directories checked for a portable config,
// in the same order as KopiaUI.
func portableConfigDirs() []string {
	var result []string

	if d := os.Getenv("KOPIA_UI_PORTABLE_CONFIG_DIR"); d != "" {
		result = append(result, d)
	}

	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		result = append(result,
			filepath.Join(exeDir, "repositories"),
			filepath.Join(exeDir, "..", "repositories"),
		)
	}

	return result
}

func newConfigStore() (*configStore, error) {
	c := &configStore{repos: map[string]bool{}}

	for _, d := range portableConfigDirs() {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			c.dir = filepath.Clean(d)
			c.portable = true

			break
		}
	}

	if c.dir == "" {
		// same as Electron's app.getPath("appData") + "/kopia", i.e. %APPDATA%\kopia on Windows.
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}

		c.dir = filepath.Join(base, "kopia")
	}

	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, err
	}

	migrateLegacyFile(c.dir, legacyStateFileName, stateFileName)
	migrateLegacyFile(c.dir, legacyWindowStateFileName, windowStateFileName)

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == configFileSuffix {
			c.repos[strings.TrimSuffix(e.Name(), configFileSuffix)] = true
		}
	}

	if len(c.repos) == 0 {
		c.repos[defaultRepoID] = true
		c.firstRun = true
	}

	c.loadNotificationLevel()
	c.loadState()

	return c, nil
}

// migrateLegacyFile renames a file this program wrote as KopiaUI-Wails to its
// new name, unless a file of the new name already exists. The installer does
// it on an upgrade; this covers the portable zip.
func migrateLegacyFile(dir, oldName, newName string) {
	newPath := filepath.Join(dir, newName)
	if _, err := os.Lstat(newPath); !os.IsNotExist(err) {
		return
	}

	if err := os.Rename(filepath.Join(dir, oldName), newPath); err == nil {
		slog.Info("renamed file of KopiaUI-Wails", "from", oldName, "to", newName)
	}
}

// list returns repository IDs with the default repository first.
func (c *configStore) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.listLocked()
}

func (c *configStore) listLocked() []string {
	ids := make([]string, 0, len(c.repos))
	for id := range c.repos {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == defaultRepoID) != (ids[j] == defaultRepoID) {
			return ids[i] == defaultRepoID
		}

		return ids[i] < ids[j]
	})

	return ids
}

func (c *configStore) addNew() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := defaultRepoID
	if len(c.repos) > 0 {
		id = "repository-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	}

	c.repos[id] = true

	return id
}

// removeIfDisconnected forgets a non-default repository whose config file
// does not exist, i.e. the user opened "connect to another repository" and
// closed the window without connecting.
func (c *configStore) removeIfDisconnected(id string) bool {
	if id == defaultRepoID {
		return false
	}

	if _, err := os.Stat(c.configFile(id)); err == nil {
		return false
	}

	c.mu.Lock()
	delete(c.repos, id)
	c.mu.Unlock()

	return true
}

// has reports whether id is a known repository.
func (c *configStore) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.repos[id]
}

// activeRepository returns the repository the window shows: the one chosen
// last, or the first one when that is gone.
func (c *configStore) activeRepository() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.repos[c.state.ActiveRepository] {
		return c.state.ActiveRepository
	}

	if ids := c.listLocked(); len(ids) > 0 {
		return ids[0]
	}

	return defaultRepoID
}

// setActiveRepository chooses the repository the window shows and remembers
// it for the next start.
func (c *configStore) setActiveRepository(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state.ActiveRepository == id {
		return
	}

	c.state.ActiveRepository = id
	c.saveStateLocked()
}

// titleBarColors returns the title bar colors the web UI reported last, or
// nil.
func (c *configStore) titleBarColors() *titleBarColors {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state.TitleBar == nil {
		return nil
	}

	t := *c.state.TitleBar

	return &t
}

// setTitleBarColors remembers the title bar colors for the next start.
func (c *configStore) setTitleBarColors(t titleBarColors) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state.TitleBar != nil && *c.state.TitleBar == t {
		return
	}

	c.state.TitleBar = &t
	c.saveStateLocked()
}

// +checklocks:c.mu
func (c *configStore) saveStateLocked() {
	b, err := json.Marshal(c.state)
	if err != nil {
		return
	}

	if err := os.WriteFile(c.stateFile(), b, stateFilePermissions); err != nil {
		slog.Warn("unable to save the window state", "err", err)
	}
}

func (c *configStore) stateFile() string {
	return filepath.Join(c.dir, stateFileName)
}

func (c *configStore) loadState() {
	b, err := os.ReadFile(c.stateFile())
	if err != nil {
		return
	}

	var st shellState
	if json.Unmarshal(b, &st) != nil {
		return
	}

	// colors from an edited file are not applied
	if st.TitleBar != nil && st.TitleBar.validate() != nil {
		st.TitleBar = nil
	}

	c.state = st
}

func (c *configStore) configFile(id string) string {
	return filepath.Join(c.dir, id+configFileSuffix)
}

func (c *configStore) notificationsFile() string {
	return filepath.Join(c.dir, notificationsFileName)
}

func (c *configStore) loadNotificationLevel() {
	c.level = levelWarningsAndErrors

	b, err := os.ReadFile(c.notificationsFile())
	if err != nil {
		return
	}

	var ns notificationSettings
	if json.Unmarshal(b, &ns) == nil && ns.Level >= levelDisabled && ns.Level <= levelAll {
		c.level = ns.Level
	}
}

func (c *configStore) notificationLevel() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.level
}

func (c *configStore) setNotificationLevel(level int) {
	c.mu.Lock()
	c.level = level
	c.mu.Unlock()

	b, _ := json.Marshal(notificationSettings{Level: level})
	_ = os.WriteFile(c.notificationsFile(), b, 0o600)
}
