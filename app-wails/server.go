package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	fastPollInterval     = 3 * time.Second
	idlePollInterval     = 5 * time.Minute
	statusRequestTimeout = 3 * time.Second
	maxLogLines          = 200
	serverControlUser    = "server-control"
	uiUser               = "kopia"
)

// serverStatus is the subset of /api/v1/control/status used by the tray menu.
type serverStatus struct {
	StartingUp  bool   `json:"-"`
	Connected   bool   `json:"connected"`
	InitTaskID  string `json:"initTaskID,omitempty"`
	Description string `json:"description,omitempty"`
}

// notificationMessage is printed by `kopia server start --kopiaui-notifications`
// as "NOTIFICATION: <json>" on stderr.
type notificationMessage struct {
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	Severity int    `json:"severity"`
}

// repoServer manages one `kopia server start` child process, the equivalent
// of newServerForRepo() in app/public/server.js.
type repoServer struct {
	id   string
	host *shell

	mu              sync.Mutex
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	exited          chan struct{}
	address         string
	password        string
	controlPassword string
	certSHA256      string
	transport       *http.Transport
	jar             map[string]*http.Cookie
	status          serverStatus
	logLines        []string
	pollStop        chan struct{}
	pollKick        chan struct{}
}

func newRepoServer(id string, host *shell) *repoServer {
	return &repoServer{id: id, host: host, status: serverStatus{StartingUp: true}}
}

func (s *repoServer) start() error {
	s.stop()

	args := []string{
		"server", "start",
		"--ui",
		"--tls-print-server-cert",
		"--tls-generate-cert-name=127.0.0.1",
		"--random-password",
		"--random-server-control-password",
		"--tls-generate-cert",
		"--async-repo-connect",
		"--error-notifications=always",
		"--kopiaui-notifications",
		"--shutdown-on-stdin",
		"--address=127.0.0.1:0",
		"--config-file", s.host.configs.configFile(s.id),
	}

	if s.host.configs.portable {
		args = append(args,
			"--cache-directory", filepath.Join(s.host.configs.dir, "cache", s.id),
			"--log-dir", filepath.Join(s.host.configs.dir, "logs", s.id),
		)
	}

	cmd := exec.Command(s.host.kopiaPath, args...) //nolint:gosec
	hideConsoleWindow(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	slog.Info("starting kopia server", "repo", s.id, "exe", s.host.kopiaPath)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("unable to start %v: %w", s.host.kopiaPath, err)
	}

	stop := make(chan struct{})
	kick := make(chan struct{}, 1)
	exited := make(chan struct{})

	s.mu.Lock()
	s.cmd = cmd
	s.stdin = stdin
	s.exited = exited
	s.status = serverStatus{StartingUp: true}
	s.pollStop = stop
	s.pollKick = kick
	s.mu.Unlock()

	s.host.statusChanged()

	go s.readLines(stdout, false)
	go s.readLines(stderr, true)
	go s.pollLoop(stop, kick)

	go func() {
		err := cmd.Wait()
		close(exited)
		s.appendLog(fmt.Sprintf("kopia server exited: %v", err))

		s.mu.Lock()
		if s.cmd == cmd {
			s.resetLocked()
		}
		s.mu.Unlock()

		s.host.statusChanged()
	}()

	return nil
}

// resetLocked clears connection details after the process exits or is stopped.
func (s *repoServer) resetLocked() {
	if s.pollStop != nil {
		close(s.pollStop)
		s.pollStop = nil
		s.pollKick = nil
	}

	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}

	s.cmd = nil
	s.stdin = nil
	s.exited = nil
	s.address = ""
	s.password = ""
	s.controlPassword = ""
	s.certSHA256 = ""
	s.transport = nil
	s.jar = nil
	s.status = serverStatus{}
}

// stop asks the server to shut down by closing its stdin (--shutdown-on-stdin)
// and kills it if it is still running after a grace period.
func (s *repoServer) stop() {
	s.mu.Lock()
	cmd, stdin, exited := s.cmd, s.stdin, s.exited
	s.resetLocked()
	s.mu.Unlock()

	if cmd == nil {
		return
	}

	if stdin != nil {
		_ = stdin.Close()
	}

	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		slog.Warn("kopia server did not exit, killing", "repo", s.id)
		_ = cmd.Process.Kill()
	}
}

func (s *repoServer) readLines(r io.Reader, parse bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if parse && s.handleParam(line) {
			continue
		}

		s.appendLog(line)
	}
}

// handleParam processes the "KEY: value" lines the server prints on stderr.
// It returns true for lines that must not be logged because they hold secrets.
func (s *repoServer) handleParam(line string) bool {
	key, value, ok := strings.Cut(line, ": ")
	if !ok {
		return false
	}

	s.mu.Lock()
	changed := true
	secret := false

	switch key {
	case "SERVER PASSWORD":
		s.password = value
		secret = true
	case "SERVER CONTROL PASSWORD":
		s.controlPassword = value
		secret = true
	case "SERVER CERT SHA256":
		s.certSHA256 = strings.ToLower(value)
		s.transport = newPinnedTransport(s.certSHA256)
	case "SERVER ADDRESS":
		s.address = value
	case "SERVER CERTIFICATE":
		// The fingerprint printed above is enough to pin the certificate.
		secret = true
	case "NOTIFICATION":
		changed = false

		var msg notificationMessage
		if err := json.Unmarshal([]byte(value), &msg); err != nil {
			slog.Warn("unable to parse notification", "err", err)
		} else {
			go s.host.notify(s.id, msg)
		}
	default:
		changed = false
	}

	kick := s.pollKick
	s.mu.Unlock()

	if changed {
		s.host.statusChanged()
		s.kickPoll(kick)
	}

	return secret
}

// newPinnedTransport trusts exactly the certificate with the given SHA256
// fingerprint, which the server printed for us.
func newPinnedTransport(fingerprint string) *http.Transport {
	return &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConnsPerHost: 16,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Hostname verification is replaced by the fingerprint check below.
			InsecureSkipVerify: true, //nolint:gosec
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("server presented no certificate")
				}

				sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
				if hex.EncodeToString(sum[:]) != fingerprint {
					return errors.New("server certificate fingerprint mismatch")
				}

				return nil
			},
		},
	}
}

func (s *repoServer) appendLog(line string) {
	s.mu.Lock()
	s.logLines = append(s.logLines, line)
	if len(s.logLines) > maxLogLines {
		s.logLines = s.logLines[len(s.logLines)-maxLogLines:]
	}
	s.mu.Unlock()

	slog.Debug("kopia", "repo", s.id, "line", line)
}

// connection returns what is needed to proxy requests, or ok=false while the
// server is still starting.
func (s *repoServer) connection() (address, password string, transport *http.Transport, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ok = s.address != "" && s.password != "" && s.transport != nil

	return s.address, s.password, s.transport, ok
}

// storeCookies keeps cookies set by this server, dropping expired ones.
func (s *repoServer) storeCookies(cookies []*http.Cookie) {
	if len(cookies) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.jar == nil {
		s.jar = map[string]*http.Cookie{}
	}

	for _, c := range cookies {
		if c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(time.Now())) {
			delete(s.jar, c.Name)
			continue
		}

		s.jar[c.Name] = c
	}
}

// cookies returns the unexpired cookies to send to this server.
func (s *repoServer) cookies() []*http.Cookie {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]*http.Cookie, 0, len(s.jar))

	for name, c := range s.jar {
		if !c.Expires.IsZero() && c.Expires.Before(time.Now()) {
			delete(s.jar, name)
			continue
		}

		result = append(result, &http.Cookie{Name: c.Name, Value: c.Value})
	}

	return result
}

func (s *repoServer) statusSnapshot() serverStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.status
	if s.cmd == nil {
		st.StartingUp = false
	}

	return st
}

func (s *repoServer) describe() string {
	s.mu.Lock()
	running := s.cmd != nil
	s.mu.Unlock()

	st := s.statusSnapshot()

	switch {
	case !running:
		return "<server stopped>"
	case st.StartingUp:
		return "<starting up>"
	case !st.Connected && st.InitTaskID != "":
		return "<initializing>"
	case !st.Connected:
		return "<not connected>"
	case st.Description != "":
		return st.Description
	default:
		return s.id
	}
}

// pollNow triggers an immediate status poll, e.g. when a window gains focus.
func (s *repoServer) pollNow() {
	s.mu.Lock()
	kick := s.pollKick
	s.mu.Unlock()

	s.kickPoll(kick)
}

func (s *repoServer) kickPoll(kick chan struct{}) {
	if kick == nil {
		return
	}

	select {
	case kick <- struct{}{}:
	default:
	}
}

// pollLoop polls quickly while a window is visible or the repository is
// starting, and backs off otherwise (same policy as upstream PR #5735).
func (s *repoServer) pollLoop(stop, kick chan struct{}) {
	for {
		delay := idlePollInterval

		st := s.statusSnapshot()
		if s.host.anyWindowVisible() || st.StartingUp || st.InitTaskID != "" {
			delay = fastPollInterval
		}

		select {
		case <-stop:
			return
		case <-kick:
		case <-time.After(delay):
		}

		s.pollOnce()
	}
}

func (s *repoServer) pollOnce() {
	s.mu.Lock()
	address, controlPassword, transport := s.address, s.controlPassword, s.transport
	s.mu.Unlock()

	if address == "" || controlPassword == "" || transport == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), statusRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/api/v1/control/status", nil)
	if err != nil {
		return
	}

	req.SetBasicAuth(serverControlUser, controlPassword)

	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		slog.Debug("status poll failed", "repo", s.id, "err", err)
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return
	}

	var st serverStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return
	}

	s.mu.Lock()
	changed := s.status != st
	s.status = st
	s.mu.Unlock()

	if changed {
		s.host.statusChanged()
	}
}

// defaultKopiaPath finds kopia.exe: next to this executable (installed layout),
// in a "server" subdirectory (KopiaUI layout), or in the repository's dist
// directory when running from a source checkout.
func defaultKopiaPath() string {
	name := "kopia"
	if isWindows {
		name = "kopia.exe"
	}

	if p := os.Getenv("KOPIA_UI_SERVER_BINARY"); p != "" {
		return p
	}

	var candidates []string

	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, name),
			filepath.Join(dir, "server", name),
			filepath.Join(dir, "..", "dist", "kopia_windows_amd64", name),
			filepath.Join(dir, "..", "..", "dist", "kopia_windows_amd64", name),
		)
	}

	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return filepath.Clean(c)
		}
	}

	return name // rely on PATH
}
