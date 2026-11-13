package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

const (
	windowNameHeader = "x-wails-window-name"
	windowNamePrefix = "repo:"
	shimPrefix       = "/__kopiaui/"
)

//go:embed shim.js
var shimJS []byte

// The page shown while the repository server starts. It reloads itself until
// the proxy can reach the server.
const startingPage = `<!doctype html><html><head><meta charset="utf-8">
<meta http-equiv="refresh" content="1"><title>KopiaUI is Loading...</title>
<style>body{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;color:#555}
@media (prefers-color-scheme:dark){body{background:#1e1e1e;color:#bbb}}</style></head>
<body><div>Starting Kopia server&hellip;</div></body></html>`

// proxyHandler is the Wails asset handler. Every request from a repository
// window carries that window's name, which identifies the repository server
// to forward to.
//
// Cookies are kept by the proxy, per repository server, rather than by the
// webview: WebView2 does not store cookies set by responses that Wails serves
// through request interception, and all windows share one origin anyway.
// There is one window per repository, so a per-server jar is equivalent to
// the browser's cookie store in Electron KopiaUI.
type proxyHandler struct {
	host *shell
}

func repoIDFromRequest(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get(windowNameHeader), windowNamePrefix)
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	repoID := repoIDFromRequest(r)

	if strings.HasPrefix(r.URL.Path, shimPrefix) {
		h.serveShim(w, r, repoID)
		return
	}

	srv := h.host.server(repoID)
	if srv == nil {
		http.Error(w, "unknown repository window", http.StatusNotFound)
		return
	}

	address, password, transport, ok := srv.connection()
	if !ok {
		if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(w, startingPage)

			return
		}

		http.Error(w, "kopia server is starting", http.StatusServiceUnavailable)

		return
	}

	target, err := url.Parse(address)
	if err != nil {
		http.Error(w, "invalid server address", http.StatusBadGateway)
		return
	}

	rp := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host

			pr.Out.SetBasicAuth(uiUser, password)

			// Let the transport negotiate compression so it transparently
			// decompresses HTML that needs the shim injected.
			pr.Out.Header.Del("Accept-Encoding")
			pr.Out.Header.Del(windowNameHeader)
			pr.Out.Header.Del("x-wails-window-id")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")

			pr.Out.Header.Del("Cookie")
			for _, c := range srv.cookies() {
				pr.Out.AddCookie(c)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			srv.storeCookies(resp.Cookies())
			resp.Header.Del("Set-Cookie")

			return injectShim(resp)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("proxy error", "repo", repoID, "path", r.URL.Path, "err", err)
			http.Error(w, "unable to reach kopia server: "+err.Error(), http.StatusBadGateway)
		},
	}

	rp.ServeHTTP(w, r)
}

// injectShim adds the window.kopiaUI shim to HTML pages so the web UI shows the
// directory picker and "Browse" buttons it normally only shows in KopiaUI.
func injectShim(resp *http.Response) error {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		return err
	}

	tag := []byte(`<script src="` + shimPrefix + `shim.js"></script></head>`)
	body = bytes.Replace(body, []byte("</head>"), tag, 1)

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("Content-Encoding")

	return nil
}

func (h *proxyHandler) serveShim(w http.ResponseWriter, r *http.Request, repoID string) {
	switch strings.TrimPrefix(r.URL.Path, shimPrefix) {
	case "shim.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(shimJS)

	case "select-dir":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path, err := h.host.selectDirectory(repoID)
		if err != nil {
			slog.Warn("select directory failed", "err", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"path": path})

	case "browse-dir":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Path string `json:"path"`
		}

		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil || req.Path == "" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		h.host.browseDirectory(req.Path)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.NotFound(w, r)
	}
}
