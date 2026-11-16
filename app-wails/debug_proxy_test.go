package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	testProxyAddr  = "127.0.0.1:51515"
	testProxyToken = "0123456789abcdef0123456789abcdef"
)

func newGuardedDebugProxy(seenRepo *string) http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seenRepo = r.Header.Get(windowNameHeader)
		w.WriteHeader(http.StatusNoContent)
	})

	return debugProxyGuard(next, []string{testProxyAddr, "localhost:51515"}, testProxyToken)
}

func TestDebugProxyGuard_acceptsTokenFromQueryHeaderAndCookie(t *testing.T) {
	var repo string

	h := newGuardedDebugProxy(&repo)

	// the URL from the log: the token in the query sets the cookie
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testProxyAddr+"/?repo=repository&token="+testProxyToken, nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("query token: got status %v", rec.Code)
	}

	if repo != "repo:repository" {
		t.Fatalf("repo query parameter not mapped to the window name: %q", repo)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != testProxyToken || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected cookies: %+v", cookies)
	}

	// later requests of the page carry the cookie
	req := httptest.NewRequest(http.MethodGet, "http://localhost:51515/api/v1/repo/status", nil)
	req.AddCookie(cookies[0])

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("cookie token: got status %v", rec.Code)
	}

	// scripts send the header
	req = httptest.NewRequest(http.MethodGet, "http://"+testProxyAddr+"/", nil)
	req.Header.Set(debugProxyTokenHeader, testProxyToken)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("header token: got status %v", rec.Code)
	}
}

func TestDebugProxyGuard_rejectsOtherHostsAndMissingTokens(t *testing.T) {
	cases := map[string]func() *http.Request{
		"no token": func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "http://"+testProxyAddr+"/?repo=repository", nil)
		},
		"wrong token": func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "http://"+testProxyAddr+"/?token=0123456789abcdef0123456789abcdee", nil)
		},
		"empty token": func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "http://"+testProxyAddr+"/?token=", nil)
		},
		// DNS rebinding: the attacker's host name resolves to this computer
		"rebound host name with the right token": func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "http://attacker.example:51515/?token="+testProxyToken, nil)
		},
		"other port": func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/?token="+testProxyToken, nil)
		},
		"host without port": func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "http://127.0.0.1/?token="+testProxyToken, nil)
		},
	}

	for name, newRequest := range cases {
		t.Run(name, func(t *testing.T) {
			repo := "not called"

			rec := httptest.NewRecorder()
			newGuardedDebugProxy(&repo).ServeHTTP(rec, newRequest())

			if rec.Code != http.StatusForbidden {
				t.Fatalf("got status %v, want 403", rec.Code)
			}

			if repo != "not called" {
				t.Fatal("the request reached the proxy")
			}

			if len(rec.Result().Cookies()) != 0 {
				t.Fatal("a rejected request must not get the token cookie")
			}
		})
	}
}
