package main

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

// startDebugProxy serves the proxy on a loopback address for troubleshooting
// when KOPIA_UI_DEBUG_PROXY_ADDR is set (e.g. 127.0.0.1:51515). Requests select
// the repository with the "repo" query parameter or the x-wails-window-name
// header. It grants unauthenticated access to the UI, so it is never enabled
// by default.
func startDebugProxy(s *shell) {
	addr := os.Getenv("KOPIA_UI_DEBUG_PROXY_ADDR")
	if addr == "" {
		return
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		slog.Error("debug proxy address must be a loopback address", "addr", addr)
		return
	}

	h := &proxyHandler{host: s}

	srv := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if repo := r.URL.Query().Get("repo"); repo != "" {
				r.Header.Set(windowNameHeader, windowName(repo))
			}

			h.ServeHTTP(w, r)
		}),
	}

	slog.Warn("debug proxy enabled", "addr", addr)

	go func() {
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("debug proxy stopped", "err", err)
		}
	}()
}
