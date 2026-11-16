package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

const (
	debugProxyTokenParam  = "token"
	debugProxyTokenHeader = "X-Kopiaui-Debug-Token"
	debugProxyTokenCookie = "kopiaui-debug-token"
)

// debugProxyGuard passes to next only requests whose Host header is one of
// hosts and that carry token, and maps the "repo" query parameter to the
// window name header.
func debugProxyGuard(next http.Handler, hosts []string, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isDebugProxyHost(r.Host, hosts) {
			http.Error(w, "unexpected Host header", http.StatusForbidden)
			return
		}

		fromQuery := r.URL.Query().Get(debugProxyTokenParam)

		if !hasDebugProxyToken(r, fromQuery, token) {
			http.Error(w, "missing or wrong debug proxy token; see the log for the URL", http.StatusForbidden)
			return
		}

		if fromQuery != "" {
			http.SetCookie(w, &http.Cookie{Name: debugProxyTokenCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		}

		// the window name selects whose password is added, so it only comes
		// from the query parameter or a header of an accepted request
		if repo := r.URL.Query().Get("repo"); repo != "" {
			r.Header.Set(windowNameHeader, windowName(repo))
		}

		next.ServeHTTP(w, r)
	})
}

func isDebugProxyHost(host string, hosts []string) bool {
	for _, h := range hosts {
		if strings.EqualFold(host, h) {
			return true
		}
	}

	return false
}

func hasDebugProxyToken(r *http.Request, fromQuery, token string) bool {
	candidates := []string{fromQuery, r.Header.Get(debugProxyTokenHeader)}

	if c, err := r.Cookie(debugProxyTokenCookie); err == nil {
		candidates = append(candidates, c.Value)
	}

	for _, c := range candidates {
		if subtle.ConstantTimeCompare([]byte(c), []byte(token)) == 1 {
			return true
		}
	}

	return false
}
