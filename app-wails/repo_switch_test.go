package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func switchTestShell(t *testing.T, ids ...string) *shell {
	t.Helper()

	repos := map[string]bool{}
	for _, id := range ids {
		repos[id] = true
	}

	return &shell{
		servers:   map[string]*repoServer{},
		configs:   &configStore{dir: t.TempDir(), repos: repos},
		trayDirty: make(chan struct{}, 1),
	}
}

func mainWindowRequest(t *testing.T, sh *shell, action, body string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, shimPrefix+action, strings.NewReader(body))
	r.Header.Set(windowNameHeader, mainWindowName)
	r.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	(&proxyHandler{host: sh}).ServeHTTP(rec, r)

	return rec
}

func TestActiveRepository_isRememberedAndFallsBack(t *testing.T) {
	c := &configStore{dir: t.TempDir(), repos: map[string]bool{"repository": true, "repository-2": true}}

	if got := c.activeRepository(); got != "repository" {
		t.Fatalf("first start shows %q, want the default repository", got)
	}

	c.setActiveRepository("repository-2")

	// the next start reads the choice back
	c2 := &configStore{dir: c.dir, repos: map[string]bool{"repository": true, "repository-2": true}}
	c2.loadState()

	if got := c2.activeRepository(); got != "repository-2" {
		t.Fatalf("got %q after restart", got)
	}

	// a repository that is gone falls back to the first one
	delete(c2.repos, "repository-2")

	if got := c2.activeRepository(); got != "repository" {
		t.Fatalf("got %q for a removed repository", got)
	}
}

func TestShim_listsRepositoriesMarkingTheShownOne(t *testing.T) {
	sh := switchTestShell(t, "repository", "repository-2")
	sh.configs.setActiveRepository("repository-2")

	rec := mainWindowRequest(t, sh, "repositories", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %v: %v", rec.Code, rec.Body.String())
	}

	var got []repositoryInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0].ID != "repository" || got[0].Active || got[1].ID != "repository-2" || !got[1].Active {
		t.Fatalf("got %+v", got)
	}
}

func TestShim_switchesRepository(t *testing.T) {
	sh := switchTestShell(t, "repository", "repository-2")

	rec := mainWindowRequest(t, sh, "switch-repository", `{"id":"repository-2"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %v: %v", rec.Code, rec.Body.String())
	}

	if got := sh.configs.activeRepository(); got != "repository-2" {
		t.Fatalf("shows %q", got)
	}

	// the tray menu is rebuilt to check the shown repository
	select {
	case <-sh.trayDirty:
	default:
		t.Fatal("the tray menu was not refreshed")
	}

	if rec := mainWindowRequest(t, sh, "switch-repository", `{"id":"nope"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("switching to an unknown repository: got %v", rec.Code)
	}

	if got := sh.configs.activeRepository(); got != "repository-2" {
		t.Fatalf("an unknown repository changed the window to %q", got)
	}
}

func TestShim_switchRepositoryNeedsAPagePOST(t *testing.T) {
	sh := switchTestShell(t, "repository", "repository-2")

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, shimPrefix+"switch-repository", strings.NewReader(`{"id":"repository-2"}`))
	r.Header.Set(windowNameHeader, mainWindowName)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "cross-site")

	rec := httptest.NewRecorder()
	(&proxyHandler{host: sh}).ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden || sh.configs.activeRepository() != "repository" {
		t.Fatalf("a cross-site request switched the repository: %v", rec.Code)
	}
}

// Leaving a repository added with "Connect To Another Repository..." that
// was never connected forgets it, as closing its window did.
func TestSwitchRepository_forgetsAnUnconnectedRepository(t *testing.T) {
	sh := switchTestShell(t, "repository", "repository-9")

	if err := os.WriteFile(sh.configs.configFile("repository"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	sh.configs.setActiveRepository("repository-9")
	sh.switchRepository("repository")

	if sh.configs.has("repository-9") {
		t.Fatal("the unconnected repository is still listed")
	}

	if !sh.configs.has("repository") {
		t.Fatal("the connected repository was forgotten")
	}
}
