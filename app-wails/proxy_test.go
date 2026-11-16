package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInjectShim(t *testing.T) {
	const page = "<html><head><title>x</title></head><body></head></body></html>"

	resp := &http.Response{
		Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Content-Encoding": {"gzip"}, "Content-Length": {"5"}},
		Body:   io.NopCloser(strings.NewReader(page)),
	}

	if err := injectShim(resp); err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	want := `<html><head><title>x</title><script src="/__kopiaui/shim.js"></script></head><body></head></body></html>`
	if string(body) != want {
		t.Fatalf("got %q", body)
	}

	if resp.ContentLength != int64(len(want)) || resp.Header.Get("Content-Length") != "104" {
		t.Fatalf("content length not updated: %v, %q", resp.ContentLength, resp.Header.Get("Content-Length"))
	}

	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("the body is no longer encoded")
	}
}

func TestInjectShim_leavesOtherContentAlone(t *testing.T) {
	var compressed bytes.Buffer

	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write([]byte(`{"a":"</head>"}`))
	_ = zw.Close()

	resp := &http.Response{
		Header: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}},
		Body:   io.NopCloser(bytes.NewReader(compressed.Bytes())),
	}

	if err := injectShim(resp); err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, compressed.Bytes()) || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("a response that is not HTML was changed")
	}
}

func TestRepoIDFromRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(windowNameHeader, windowName("repository-2"))

	if got := repoIDFromRequest(r); got != "repository-2" {
		t.Fatalf("got %q", got)
	}
}

func TestCookieJar(t *testing.T) {
	s := newRepoServer("repository", &shell{})

	s.storeCookies([]*http.Cookie{
		{Name: "session", Value: "one"},
		{Name: "csrf", Value: "token"},
		{Name: "old", Value: "x", Expires: time.Now().Add(-time.Hour)},
		{Name: "brief", Value: "x", Expires: time.Now().Add(time.Hour)},
	})

	// a newer value replaces the old one, MaxAge<0 deletes
	s.storeCookies([]*http.Cookie{{Name: "session", Value: "two"}, {Name: "csrf", MaxAge: -1}})

	got := map[string]string{}
	for _, c := range s.cookies() {
		got[c.Name] = c.Value

		if c.Path != "" || c.HttpOnly || !c.Expires.IsZero() {
			t.Fatalf("only the name and value go to the server: %+v", c)
		}
	}

	if len(got) != 2 || got["session"] != "two" || got["brief"] != "x" {
		t.Fatalf("unexpected cookies %v", got)
	}

	// cookies that expire while stored are dropped
	s.mu.Lock()
	s.jar["brief"].Expires = time.Now().Add(-time.Second)
	s.mu.Unlock()

	if cs := s.cookies(); len(cs) != 1 || cs[0].Name != "session" {
		t.Fatalf("expired cookie was sent: %v", cs)
	}

	// a stopped server's cookies are forgotten
	s.mu.Lock()
	s.resetLocked()
	s.mu.Unlock()

	if cs := s.cookies(); len(cs) != 0 {
		t.Fatalf("cookies survived a restart: %v", cs)
	}
}

func TestLocalDirectory(t *testing.T) {
	dir := t.TempDir()

	got, err := localDirectory(dir + string(filepath.Separator) + "." + string(filepath.Separator))
	if err != nil || got != filepath.Clean(dir) {
		t.Fatalf("got %q, %v", got, err)
	}

	if runtime.GOOS == "windows" {
		// the web UI may send forward slashes
		if got, err := localDirectory(filepath.ToSlash(dir)); err != nil || got != filepath.Clean(dir) {
			t.Fatalf("got %q, %v", got, err)
		}
	}
}

func TestLocalDirectory_refusesEverythingElse(t *testing.T) {
	dir := t.TempDir()

	file := filepath.Join(dir, "run-me.bat")
	if err := os.WriteFile(file, []byte("echo"), 0o600); err != nil {
		t.Fatal(err)
	}

	bad := []string{
		"",
		file, // the system would run it
		filepath.Join(dir, "missing"),
		"relative",
		filepath.Base(dir),
		"https://example.com/",
		"file:///etc",
		"ms-settings://x",
		"calc.exe",
	}

	if runtime.GOOS == "windows" {
		bad = append(bad,
			`\\server\share`,
			`\\server\share\folder`,
			`//server/share`,
			`\\?\C:\Windows`,
			`\\.\C:\Windows`,
			`\\localhost\C$\Windows`,
			`C:`,
			`C:Windows`,
			`\Windows`,
		)
	}

	for _, p := range bad {
		if got, err := localDirectory(p); err == nil {
			t.Errorf("localDirectory(%q) = %q, want an error", p, got)
		}
	}
}

func TestExternalURL(t *testing.T) {
	for _, u := range []string{"https://kopia.io/docs/", "http://example.com/a?b=c#d"} {
		if got, err := externalURL(u); err != nil || got != u {
			t.Errorf("externalURL(%q) = %q, %v", u, got, err)
		}
	}

	for _, u := range []string{"", "kopia.io", "/docs", "file:///C:/Windows/System32/calc.exe", "ms-msdt:/id", "javascript:alert(1)", "https://", "mailto:a@example.com", `C:\Windows`, "http:foo", "%zz"} {
		if got, err := externalURL(u); err == nil {
			t.Errorf("externalURL(%q) = %q, want an error", u, got)
		}
	}
}

// shimRequest sends a request to the proxy's own endpoints. The shell has no
// application, so a request that got past the checks would crash the test.
func shimRequest(method, action, contentType, fetchSite, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, shimPrefix+action, strings.NewReader(body))
	r.Header.Set(windowNameHeader, windowName(defaultRepoID))

	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}

	if fetchSite != "" {
		r.Header.Set("Sec-Fetch-Site", fetchSite)
	}

	rec := httptest.NewRecorder()
	sh := &shell{servers: map[string]*repoServer{}, runElevated: refuseElevation}
	(&proxyHandler{host: sh}).ServeHTTP(rec, r)

	return rec
}

func TestServeShim_script(t *testing.T) {
	rec := shimRequest(http.MethodGet, "shim.js", "", "", "")

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "window.kopiaUI") {
		t.Fatalf("got %v", rec.Code)
	}
}

func TestServeShim_rejectsRequestsThatMustNotAct(t *testing.T) {
	dir := t.TempDir()

	file := filepath.Join(dir, "evil.bat")
	if err := os.WriteFile(file, []byte("echo"), 0o600); err != nil {
		t.Fatal(err)
	}

	jsonPath := func(p string) string { return `{"path":` + quoteJSON(p) + `}` }

	cases := []struct {
		name                                    string
		method, action, contentType, site, body string
		want                                    int
	}{
		{"GET does not act", http.MethodGet, "browse-dir", "application/json", "same-origin", jsonPath(dir), http.StatusMethodNotAllowed},
		{"GET restart", http.MethodGet, "restart", "", "", "", http.StatusMethodNotAllowed},
		{"another site", http.MethodPost, "browse-dir", "application/json", "cross-site", jsonPath(dir), http.StatusForbidden},
		{"another site on this computer", http.MethodPost, "restart", "", "same-site", "", http.StatusForbidden},
		{"another site opens the log", http.MethodPost, "open-log", "", "cross-site", "", http.StatusForbidden},
		{"another site picks a folder", http.MethodPost, "select-dir", "", "cross-site", "", http.StatusForbidden},
		// a form or a no-cors fetch of another site can only send these types
		{"text/plain body", http.MethodPost, "browse-dir", "text/plain", "", jsonPath(dir), http.StatusUnsupportedMediaType},
		{"form body", http.MethodPost, "browse-dir", "application/x-www-form-urlencoded", "same-origin", jsonPath(dir), http.StatusUnsupportedMediaType},
		{"no content type", http.MethodPost, "open-url", "", "same-origin", `{"url":"https://kopia.io"}`, http.StatusUnsupportedMediaType},
		{"not JSON", http.MethodPost, "browse-dir", "application/json", "same-origin", "path=" + dir, http.StatusBadRequest},
		{"a program", http.MethodPost, "browse-dir", "application/json; charset=utf-8", "same-origin", jsonPath(file), http.StatusBadRequest},
		{"a URL as a folder", http.MethodPost, "browse-dir", "application/json", "same-origin", jsonPath("https://example.com"), http.StatusBadRequest},
		{"a network share", http.MethodPost, "browse-dir", "application/json", "same-origin", jsonPath(`\\attacker.example\share`), http.StatusBadRequest},
		{"empty path", http.MethodPost, "browse-dir", "application/json", "same-origin", `{}`, http.StatusBadRequest},
		{"a program as a URL", http.MethodPost, "open-url", "application/json", "same-origin", `{"url":"file:///C:/Windows/System32/calc.exe"}`, http.StatusBadRequest},
		{"GET runs the image task", http.MethodGet, "run-image-task", "application/json", "same-origin", `{}`, http.StatusMethodNotAllowed},
		{"another site runs the image task", http.MethodPost, "run-image-task", "application/json", "cross-site", `{}`, http.StatusForbidden},
		{"a form runs the image task", http.MethodPost, "run-image-task", "application/x-www-form-urlencoded", "", `{}`, http.StatusUnsupportedMediaType},
		{"text runs the image task", http.MethodPost, "run-image-task", "text/plain", "", `{}`, http.StatusUnsupportedMediaType},
		{"unknown repository", http.MethodPost, "restart", "", "same-origin", "", http.StatusNotFound},
		{"unknown action", http.MethodPost, "format-disk", "", "same-origin", "", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := shimRequest(tc.method, tc.action, tc.contentType, tc.site, tc.body); rec.Code != tc.want {
				t.Fatalf("got status %v, want %v: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func quoteJSON(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func TestProxy_unknownWindowAndStartupPage(t *testing.T) {
	sh := &shell{servers: map[string]*repoServer{}}
	sh.servers["repository"] = newRepoServer("repository", sh)

	h := &proxyHandler{host: sh}

	get := func(path, window string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(windowNameHeader, window)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		return rec
	}

	if rec := get("/", windowName("other")); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown window: got %v", rec.Code)
	}

	// a server that is not running: pages get the error page, API calls an error
	rec := get("/snapshots", windowName("repository"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not available") || !strings.Contains(rec.Body.String(), "/__kopiaui/restart") {
		t.Fatalf("expected the error page, got %v: %s", rec.Code, rec.Body)
	}

	if rec := get("/api/v1/repo/status", windowName("repository")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("API while down: got %v", rec.Code)
	}
}

func TestStartupPage_escapesServerOutput(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStartupPage(rec, startupProblem{failed: true, reason: "<script>alert(1)</script>", logs: []string{`<img src=x onerror="x()">`}})

	body := rec.Body.String()
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") {
		t.Fatalf("server output reached the page unescaped: %s", body)
	}
}

// refuseElevation fails a test that would show an administrator prompt.
func refuseElevation(string, []string, time.Duration) (uint32, error) {
	return 0, errors.New("a test must not ask for administrator rights")
}

func TestServeShim_runImageTask(t *testing.T) {
	run := func(sh *shell) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, shimPrefix+"run-image-task", strings.NewReader(`{}`))
		r.Header.Set(windowNameHeader, windowName(defaultRepoID))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", "same-origin")

		rec := httptest.NewRecorder()
		(&proxyHandler{host: sh}).ServeHTTP(rec, r)

		return rec
	}

	var gotExe string

	var gotArgs []string

	sh := &shell{servers: map[string]*repoServer{}, kopiaPath: `C:\Program Files\KopiaUI\kopia.exe`}
	sh.runElevated = func(exe string, args []string, _ time.Duration) (uint32, error) {
		gotExe, gotArgs = exe, args
		return 0, nil
	}

	// the command is fixed, whatever the page sends
	if rec := run(sh); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"started"`) {
		t.Fatalf("started: got %v: %s", rec.Code, rec.Body)
	}

	if gotExe != sh.kopiaPath || strings.Join(gotArgs, " ") != "image schedule run" {
		t.Fatalf("ran %q %q", gotExe, gotArgs)
	}

	sh.runElevated = func(string, []string, time.Duration) (uint32, error) { return 0, errElevationCanceled }
	if rec := run(sh); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"canceled"`) {
		t.Fatalf("declined prompt: got %v: %s", rec.Code, rec.Body)
	}

	sh.runElevated = func(string, []string, time.Duration) (uint32, error) { return 1, nil }
	if rec := run(sh); rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "exit code 1") {
		t.Fatalf("failed run: got %v: %s", rec.Code, rec.Body)
	}

	// a second request while the prompt is open does not open another
	sh.elevationMu.Lock()

	rec := run(sh)

	sh.elevationMu.Unlock()

	if rec.Code != http.StatusConflict {
		t.Fatalf("prompt already open: got %v: %s", rec.Code, rec.Body)
	}
}
