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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	fastPollInterval     = 3 * time.Second
	idlePollInterval     = 5 * time.Minute
	statusRequestTimeout = 3 * time.Second
	maxLogLines          = 200
	serverStartTimeout   = 30 * time.Second
	serverStopGrace      = 5 * time.Second
	serverKillWait       = 5 * time.Second
	errorPageLogLines    = 40
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

	// lifecycle serializes start and stop, which the tray, the error page and
	// shutdown can call at the same time. Without it two overlapping starts
	// would each launch a server and lose track of the first.
	lifecycle sync.Mutex

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
	startedAt       time.Time
	lastExit        string
	logLines        []string
	pollStop        chan struct{}
	pollKick        chan struct{}
}

func newRepoServer(id string, host *shell) *repoServer {
	return &repoServer{id: id, host: host, status: serverStatus{StartingUp: true}}
}

// start launches the server, after stopping the one that is running.
func (s *repoServer) start() error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()

	s.stopLocked()

	// the server uses a certificate made here; see serverCert
	cert, err := newServerCert()
	if err != nil {
		return err
	}

	started := false

	defer func() {
		if !started {
			cert.remove()
		}
	}()

	args := []string{
		"server", "start",
		"--ui",
		"--tls-cert-file", cert.certFile,
		"--tls-key-file", cert.keyFile,
		"--random-password",
		"--random-server-control-password",
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
		s.mu.Lock()
		s.lastExit = fmt.Sprintf("Unable to start %v: %v", s.host.kopiaPath, err)
		s.mu.Unlock()

		return fmt.Errorf("unable to start %v: %w", s.host.kopiaPath, err)
	}

	started = true

	s.appendLog("--- starting kopia server ---")

	stop := make(chan struct{})
	kick := make(chan struct{}, 1)
	exited := make(chan struct{})

	s.mu.Lock()
	s.cmd = cmd
	s.stdin = stdin
	s.exited = exited
	s.status = serverStatus{StartingUp: true}
	s.certSHA256 = cert.fingerprint
	s.transport = newPinnedTransport(cert.fingerprint)
	s.pollStop = stop
	s.pollKick = kick
	s.startedAt = time.Now()
	s.lastExit = ""
	s.mu.Unlock()

	s.host.statusChanged()

	go s.readLines(cmd, stdout, false)
	go s.readLines(cmd, stderr, true)
	go s.pollLoop(stop, kick)

	go func() {
		err := cmd.Wait()

		cert.remove()
		close(exited)
		s.appendLog(fmt.Sprintf("kopia server exited: %v", err))

		s.mu.Lock()
		if s.cmd == cmd {
			// exited on its own rather than through stop().
			s.resetLocked()
			s.lastExit = fmt.Sprintf("The Kopia server exited unexpectedly (%v).", err)
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
// and kills it if it is still running after a grace period. The server has
// exited when stop returns, unless it survives being killed.
func (s *repoServer) stop() {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()

	s.stopLocked()
}

// stopLocked is stop for callers that hold s.lifecycle.
func (s *repoServer) stopLocked() {
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
		return
	case <-time.After(serverStopGrace):
		slog.Warn("kopia server did not exit, killing", "repo", s.id)
		_ = cmd.Process.Kill()
	}

	// a killed server must be gone before another one starts on its repository
	select {
	case <-exited:
	case <-time.After(serverKillWait):
		slog.Error("kopia server is still running after being killed", "repo", s.id)
	}
}

// readLines reads the output of the server process cmd line by line. It never
// stops reading before the stream ends: the server blocks when nobody reads
// its output.
func (s *repoServer) readLines(cmd *exec.Cmd, r io.Reader, parse bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if parse && s.handleParam(cmd, line) {
			continue
		}

		s.appendLog(line)
	}

	if err := sc.Err(); err != nil {
		// a line longer than the buffer stops the scanner for good
		slog.Warn("no longer logging kopia server output", "repo", s.id, "err", err)
		_, _ = io.Copy(io.Discard, r)
	}
}

// isLoopbackServerAddress reports whether address is an https URL of a
// loopback IP address, which is the only kind of address the server started
// here listens on.
func isLoopbackServerAddress(address string) bool {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Port() == "" {
		return false
	}

	ip := net.ParseIP(u.Hostname())

	return ip != nil && ip.IsLoopback()
}

// handleParam processes the "KEY: value" lines the server process cmd prints
// on stderr. It returns true for lines that must not be logged because they
// hold secrets.
//
// The connection parameters decide where the proxy sends the UI password, and
// stderr also carries log messages, whose text may come from elsewhere, such
// as file names. So each parameter is accepted once, from the process that is
// current, and only an address on this computer: a later line that looks like
// a parameter is just a log line.
//
// A line that looks like a parameter and comes before the real one is still
// accepted. That cannot send a password anywhere else: the certificate the
// proxy insists on is the one start made, never one named on stderr, and its
// key is the server's alone. The worst such a line does is make the server
// unreachable until it is restarted.
func (s *repoServer) handleParam(cmd *exec.Cmd, line string) bool {
	key, value, ok := strings.Cut(line, ": ")
	if !ok {
		return false
	}

	s.mu.Lock()

	if s.cmd != cmd {
		// output of a server that was stopped
		s.mu.Unlock()

		return key == "SERVER PASSWORD" || key == "SERVER CONTROL PASSWORD" || key == "SERVER CERTIFICATE"
	}

	changed := false
	secret := false

	switch key {
	case "SERVER PASSWORD":
		secret = true

		if s.password == "" {
			s.password = value
			changed = true
		}
	case "SERVER CONTROL PASSWORD":
		secret = true

		if s.controlPassword == "" {
			s.controlPassword = value
			changed = true
		}
	case "SERVER ADDRESS":
		switch {
		case s.address != "":
		case !isLoopbackServerAddress(value):
			slog.Warn("ignoring kopia server address that is not on this computer", "repo", s.id, "address", value)
		default:
			s.address = value
			changed = true
		}
	case "SERVER CERTIFICATE":
		// not printed for a certificate from files, and never needed
		secret = true
	case "NOTIFICATION":
		var msg notificationMessage
		if err := json.Unmarshal([]byte(value), &msg); err != nil {
			slog.Warn("unable to parse notification", "err", err)
		} else {
			go s.host.notify(s.id, msg)
		}
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
// fingerprint, which is the one the server was started with.
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

// startupProblem describes why the server cannot be reached yet.
type startupProblem struct {
	failed  bool
	reason  string
	elapsed time.Duration
	logs    []string
}

// startupProblem reports whether a server that is not reachable is still
// starting or has failed: it exited, never started, or did not print its
// address within serverStartTimeout.
func (s *repoServer) startupProblem() startupProblem {
	s.mu.Lock()
	defer s.mu.Unlock()

	var p startupProblem

	switch {
	case s.cmd == nil:
		p.failed = true

		p.reason = s.lastExit
		if p.reason == "" {
			p.reason = "The Kopia server is not running."
		}

	default:
		p.elapsed = time.Since(s.startedAt)

		if p.elapsed > serverStartTimeout {
			p.failed = true
			p.reason = fmt.Sprintf("The Kopia server did not finish starting within %v.", serverStartTimeout)
		}
	}

	if p.failed {
		n := min(len(s.logLines), errorPageLogLines)
		p.logs = append([]string(nil), s.logLines[len(s.logLines)-n:]...)
	}

	return p
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

// defaultKopiaPath finds the kopia executable: next to this executable (installed
// layout, including /usr/bin on Linux), in a "server" subdirectory (KopiaUI layout),
// in the repository's dist directory when running from a source checkout, and
// finally on PATH.
func defaultKopiaPath() string {
	name := "kopia"
	if isWindows {
		name = "kopia.exe"
	}

	if p := os.Getenv("KOPIA_UI_SERVER_BINARY"); p != "" {
		return p
	}

	// the directory the Makefile and release workflow build kopia into, e.g. kopia_linux_amd64
	distDir := "kopia_" + runtime.GOOS + "_" + runtime.GOARCH

	var candidates []string

	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, name),
			filepath.Join(dir, "server", name),
			filepath.Join(dir, "..", "dist", distDir, name),
			filepath.Join(dir, "..", "..", "dist", distDir, name),
		)
	}

	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return filepath.Clean(c)
		}
	}

	return name // rely on PATH
}
