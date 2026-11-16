//go:build debugproxy

package main

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

const (
	debugProxyAddrEnv    = "KOPIA_UI_DEBUG_PROXY_ADDR"
	debugProxyTokenBytes = 16
)

// startDebugProxy serves the proxy on a loopback address for troubleshooting
// when KOPIA_UI_DEBUG_PROXY_ADDR is set (e.g. 127.0.0.1:51515). It exists only
// in builds with the "debugproxy" tag, so a release build cannot be made to
// open it by an environment variable that was left set.
//
// Only this function is left out of other builds. The guard it puts in front
// of the proxy is in debug_proxy_guard.go, so that the usual test run covers
// it; a build without the tag has nothing that calls it.
//
// The proxy adds the UI password to every request it forwards, so it is
// guarded twice. Requests must name the proxy's own address in the Host
// header, which stops a web page that re-points its own host name at this
// computer (DNS rebinding). And they must carry the token that is generated
// at every start and written to the log: as the "token" query parameter, the
// X-Kopiaui-Debug-Token header, or the cookie that a request with the query
// parameter sets.
//
// Requests select the repository with the "repo" query parameter or the
// x-wails-window-name header.
func startDebugProxy(s *shell) {
	addr := os.Getenv(debugProxyAddrEnv)
	if addr == "" {
		return
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		slog.Error("debug proxy address must be a loopback address", "addr", addr)
		return
	}

	raw := make([]byte, debugProxyTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		slog.Error("unable to generate the debug proxy token", "err", err)
		return
	}

	token := hex.EncodeToString(raw)

	srv := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		Handler:           debugProxyGuard(&proxyHandler{host: s}, []string{addr, net.JoinHostPort("localhost", port)}, token),
	}

	slog.Warn("debug proxy enabled", "url", "http://"+addr+"/?repo="+defaultRepoID+"&"+debugProxyTokenParam+"="+token)

	go func() {
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("debug proxy stopped", "err", err)
		}
	}()
}
