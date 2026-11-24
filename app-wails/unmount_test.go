package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeControlServer is a server whose control API unmounts only mountedAt.
func fakeControlServer(t *testing.T, host *shell, id, mountedAt, failWith string) *repoServer {
	t.Helper()

	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if r.URL.Path != "/api/v1/control/unmount" || user != serverControlUser || pass != "secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		var req struct {
			Path string `json:"path"`
		}

		_ = json.NewDecoder(r.Body).Decode(&req)

		switch {
		case failWith != "":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"INTERNAL","error":"` + failWith + `"}`))
		case strings.EqualFold(req.Path, mountedAt):
			_, _ = w.Write([]byte(`{"path":"` + mountedAt + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"NOT_FOUND","error":"nothing is mounted at x"}`))
		}
	}))
	t.Cleanup(hs.Close)

	srv := newRepoServer(id, host)
	srv.address = hs.URL
	srv.controlPassword = "secret"
	srv.transport = &http.Transport{}

	return srv
}

func TestUnmountPath(t *testing.T) {
	s := &shell{servers: map[string]*repoServer{}}

	s.servers["a"] = fakeControlServer(t, s, "a", "X:", "")
	s.servers["b"] = fakeControlServer(t, s, "b", "Y:", "")
	s.servers["c"] = newRepoServer("c", s) // still starting: skipped

	if err := s.unmountPath("y:"); err != nil {
		t.Fatalf("unmount of a drive another server mounted: %v", err)
	}

	if err := s.unmountPath("Z:"); !errors.Is(err, errNotMountedHere) {
		t.Fatalf("unmount of a drive no server mounted: %v", err)
	}

	s.servers["d"] = fakeControlServer(t, s, "d", "", "drive is busy")

	if err := s.unmountPath("Z:"); err == nil || !strings.Contains(err.Error(), "drive is busy") {
		t.Fatalf("a server's error must be returned, got %v", err)
	}
}

func TestArgValue(t *testing.T) {
	args := []string{`C:\KopiaUI.exe`, unmountArg, `X:\`}

	if v, ok := argValue(args, unmountArg); !ok || v != `X:\` {
		t.Fatalf("got %q %v", v, ok)
	}

	if _, ok := argValue([]string{unmountArg}, unmountArg); ok {
		t.Fatal("a missing value must not be found")
	}

	// "%1" quoted by a context menu command reaches the program as X:"
	if v, ok := unmountArgPath([]string{unmountArg, `X:"`}); !ok || v != "X:" {
		t.Fatalf("got %q %v", v, ok)
	}

	if _, ok := unmountArgPath([]string{unmountArg, `"`}); ok {
		t.Fatal("an empty drive must not be found")
	}
}
