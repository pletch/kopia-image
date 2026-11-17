package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func validScheduleSettings() *imageScheduleSettings {
	return &imageScheduleSettings{
		Volumes:  []string{"C:", "D:"},
		At:       "02:30",
		Days:     []string{"Monday", "Thursday"},
		CheckDay: "Sunday",
		CheckAt:  "12:00",
		Wake:     true,
	}
}

func TestImageScheduleSettings_installArgs(t *testing.T) {
	args, err := validScheduleSettings().installArgs(`C:\cfg\repository.config`, "id1")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"--config-file", `C:\cfg\repository.config`, "image", "schedule", "install", "C:", "D:",
		"--at", "02:30", "--check-day", "Sunday", "--check-at", "12:00", "--days", "Monday,Thursday",
		"--wake", "--no-allow-on-battery", "--request-id", "id1",
	}

	if !slices.Equal(args, want) {
		t.Fatalf("got %q\nwant %q", args, want)
	}

	// every day, no weekly check
	s := validScheduleSettings()
	s.Days = nil
	s.CheckDay = "none"

	args, err = s.installArgs("cfg", "id2")
	if err != nil {
		t.Fatal(err)
	}

	if slices.Contains(args, "--days") || !slices.Contains(args, "none") {
		t.Fatalf("got %q", args)
	}
}

// TestImageScheduleSettings_rejects checks the values that must never reach
// a command line run as administrator.
func TestImageScheduleSettings_rejects(t *testing.T) {
	cases := map[string]func(s *imageScheduleSettings){
		"no volumes":               func(s *imageScheduleSettings) { s.Volumes = nil },
		"a path as a volume":       func(s *imageScheduleSettings) { s.Volumes = []string{`C:\Windows`} },
		"a flag as a volume":       func(s *imageScheduleSettings) { s.Volumes = []string{"--kopia-exe=evil.exe"} },
		"a lower-case letter":      func(s *imageScheduleSettings) { s.Volumes = []string{"c:"} },
		"the same volume twice":    func(s *imageScheduleSettings) { s.Volumes = []string{"C:", "C:"} },
		"a bad time":               func(s *imageScheduleSettings) { s.At = "25:00" },
		"a time with a flag":       func(s *imageScheduleSettings) { s.At = "02:00 --kopia-exe=x" },
		"a bad check time":         func(s *imageScheduleSettings) { s.CheckAt = "noon" },
		"an unknown day":           func(s *imageScheduleSettings) { s.Days = []string{"Funday"} },
		"a day list in one string": func(s *imageScheduleSettings) { s.Days = []string{"Monday,Tuesday"} },
		"the same day twice":       func(s *imageScheduleSettings) { s.Days = []string{"Monday", "Monday"} },
		"an unknown check day":     func(s *imageScheduleSettings) { s.CheckDay = "--check-ntfs" },
	}

	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := validScheduleSettings()
			change(s)

			if args, err := s.installArgs("cfg", "id"); !errors.Is(err, errInvalidSchedule) {
				t.Fatalf("accepted: %q", args)
			}
		})
	}
}

// fakeKopia stands in for kopia run as administrator: it records an outcome
// under the request ID it was given, as kopia does.
func fakeKopia(t *testing.T, dir string, outcome func(args []string) imageScheduleOutcome, ran *[][]string) elevatedRunner {
	t.Helper()

	return func(_ string, args []string, _ time.Duration) (uint32, error) {
		*ran = append(*ran, args)

		o := outcome(args)
		o.ID = args[slices.Index(args, "--request-id")+1]

		b, err := json.Marshal(o)
		if err != nil {
			return 0, err
		}

		if err := os.WriteFile(filepath.Join(dir, imageScheduleRequestFile), b, 0o600); err != nil {
			return 0, err
		}

		if !o.Success {
			return 1, nil
		}

		return 0, nil
	}
}

func scheduleShell(t *testing.T) *shell {
	t.Helper()

	sh := &shell{servers: map[string]*repoServer{}, configs: &configStore{dir: t.TempDir()}, imageStateDir: t.TempDir()}
	sh.servers[defaultRepoID] = newRepoServer(defaultRepoID, sh)

	return sh
}

func TestInstallImageSchedule(t *testing.T) {
	sh := scheduleShell(t)

	var ran [][]string

	sh.runElevated = fakeKopia(t, sh.imageStateDir, func([]string) imageScheduleOutcome {
		return imageScheduleOutcome{Success: true}
	}, &ran)

	if err := sh.installImageSchedule(defaultRepoID, validScheduleSettings()); err != nil {
		t.Fatal(err)
	}

	// the window's own repository connection
	if len(ran) != 1 || ran[0][1] != sh.configs.configFile(defaultRepoID) {
		t.Fatalf("ran %q", ran)
	}

	// kopia's reason for a failure reaches the page
	sh.runElevated = fakeKopia(t, sh.imageStateDir, func([]string) imageScheduleOutcome {
		return imageScheduleOutcome{Error: "unable to get the repository password"}
	}, &ran)

	err := sh.installImageSchedule(defaultRepoID, validScheduleSettings())
	if err == nil || !strings.Contains(err.Error(), "unable to get the repository password") {
		t.Fatalf("got %v", err)
	}

	// an outcome of another request does not count
	sh.runElevated = func(string, []string, time.Duration) (uint32, error) { return 2, nil }

	err = sh.installImageSchedule(defaultRepoID, validScheduleSettings())
	if err == nil || !strings.Contains(err.Error(), "exit code 2") {
		t.Fatalf("got %v", err)
	}

	// a declined prompt
	sh.runElevated = func(string, []string, time.Duration) (uint32, error) { return 0, errElevationCanceled }

	if err := sh.installImageSchedule(defaultRepoID, validScheduleSettings()); !errors.Is(err, errElevationCanceled) {
		t.Fatalf("got %v", err)
	}

	// another window's repository is not this window's to schedule
	if err := sh.installImageSchedule("other", validScheduleSettings()); err == nil {
		t.Fatal("installed for an unknown repository")
	}
}

func TestRemoveImageSchedule(t *testing.T) {
	sh := scheduleShell(t)

	var ran [][]string

	sh.runElevated = fakeKopia(t, sh.imageStateDir, func([]string) imageScheduleOutcome {
		return imageScheduleOutcome{Success: true}
	}, &ran)

	if err := sh.removeImageSchedule(); err != nil {
		t.Fatal(err)
	}

	if len(ran) != 1 || !slices.Equal(ran[0][:3], []string{"image", "schedule", "remove"}) {
		t.Fatalf("ran %q", ran)
	}
}

func TestServeShim_imageSchedule(t *testing.T) {
	sh := scheduleShell(t)

	var ran [][]string

	sh.runElevated = fakeKopia(t, sh.imageStateDir, func([]string) imageScheduleOutcome {
		return imageScheduleOutcome{Success: true}
	}, &ran)

	post := func(action, contentType, site, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, shimPrefix+action, strings.NewReader(body))
		r.Header.Set(windowNameHeader, windowName(defaultRepoID))
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Sec-Fetch-Site", site)

		rec := httptest.NewRecorder()
		(&proxyHandler{host: sh}).ServeHTTP(rec, r)

		return rec
	}

	body, err := json.Marshal(validScheduleSettings())
	if err != nil {
		t.Fatal(err)
	}

	if rec := post("install-image-schedule", "application/json", "same-origin", string(body)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"done"`) {
		t.Fatalf("install: got %v: %s", rec.Code, rec.Body)
	}

	if rec := post("remove-image-schedule", "application/json", "same-origin", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("remove: got %v: %s", rec.Code, rec.Body)
	}

	// invalid settings are refused before any prompt
	if rec := post("install-image-schedule", "application/json", "same-origin", `{"volumes":["C:\\Windows"],"at":"02:00","checkDay":"none","checkAt":"12:00"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid settings: got %v: %s", rec.Code, rec.Body)
	}

	// other sites and forms cannot act
	if rec := post("install-image-schedule", "application/json", "cross-site", string(body)); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site: got %v", rec.Code)
	}

	if rec := post("remove-image-schedule", "application/x-www-form-urlencoded", "", `{}`); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form: got %v", rec.Code)
	}

	if len(ran) != 2 {
		t.Fatalf("ran %v times, want 2", len(ran))
	}
}

func TestServeShim_createImageAndRecoveryMedia(t *testing.T) {
	sh := scheduleShell(t)

	var consoles [][]string

	sh.runConsole = func(exe string, args []string) error {
		consoles = append(consoles, append([]string{exe}, args...))
		return nil
	}

	post := func(action, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, shimPrefix+action, strings.NewReader(body))
		r.Header.Set(windowNameHeader, windowName(defaultRepoID))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", "same-origin")

		rec := httptest.NewRecorder()
		(&proxyHandler{host: sh}).ServeHTTP(rec, r)

		return rec
	}

	if rec := post("create-image", `{"volumes":["C:","D:"]}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"started"`) {
		t.Fatalf("create-image: got %v: %s", rec.Code, rec.Body)
	}

	cfg := sh.configs.configFile(defaultRepoID)

	if want := []string{sh.kopiaPath, "--config-file", cfg, "image", "create", "C:", "D:"}; !slices.Equal(consoles[0], want) {
		t.Fatalf("ran %q, want %q", consoles[0], want)
	}

	if rec := post("create-recovery-media", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("create-recovery-media: got %v: %s", rec.Code, rec.Body)
	}

	if want := []string{sh.kopiaPath, "--config-file", cfg, "image", "recovery-media", "create"}; !slices.Equal(consoles[1], want) {
		t.Fatalf("ran %q, want %q", consoles[1], want)
	}

	// what reaches the administrator command line is checked
	for _, body := range []string{`{"volumes":[]}`, `{"volumes":["C:\Windows"]}`, `{"volumes":["--config-file=x"]}`} {
		if rec := post("create-image", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: got %v", body, rec.Code)
		}
	}

	// a declined prompt
	sh.runConsole = func(string, []string) error { return errElevationCanceled }

	if rec := post("create-image", `{"volumes":["C:"]}`); !strings.Contains(rec.Body.String(), `"canceled"`) {
		t.Fatalf("declined: got %v: %s", rec.Code, rec.Body)
	}

	if len(consoles) != 2 {
		t.Fatalf("ran %v consoles, want 2", len(consoles))
	}
}

func TestUpdateImageSchedule(t *testing.T) {
	sh := scheduleShell(t)

	var ran [][]string

	sh.runElevated = fakeKopia(t, sh.imageStateDir, func([]string) imageScheduleOutcome {
		return imageScheduleOutcome{Success: true}
	}, &ran)

	if err := sh.updateImageSchedule(); err != nil {
		t.Fatal(err)
	}

	if len(ran) != 1 || !slices.Equal(ran[0][:3], []string{"image", "schedule", "update"}) {
		t.Fatalf("ran %q", ran)
	}

	// kopia's reason reaches the page
	sh.runElevated = fakeKopia(t, sh.imageStateDir, func([]string) imageScheduleOutcome {
		return imageScheduleOutcome{Error: "the scheduled tasks' repository connection is missing"}
	}, &ran)

	if err := sh.updateImageSchedule(); err == nil || !strings.Contains(err.Error(), "connection is missing") {
		t.Fatalf("got %v", err)
	}
}
