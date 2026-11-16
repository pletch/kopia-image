package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeServerEnv    = "KOPIAUI_TEST_FAKE_SERVER_DIR"
	testFingerprint  = "aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233"
	otherFingerprint = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

// TestMain lets the test binary stand in for kopia: started with
// fakeServerEnv set, it behaves like "kopia server start" as far as the shell
// can tell and never runs tests.
func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeServerEnv); dir != "" {
		runFakeServer(dir)
		return
	}

	os.Exit(m.Run())
}

// runFakeServer records that it runs with a file named after its process ID,
// prints the server parameters and exits when its stdin is closed, like kopia
// with --shutdown-on-stdin.
func runFakeServer(dir string) {
	marker := filepath.Join(dir, fmt.Sprintf("%d.running", os.Getpid()))

	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		os.Exit(2)
	}

	// a real server started with certificate files prints no fingerprint;
	// this one stands for a log line that looks like it
	fmt.Fprintln(os.Stderr, "SERVER CERT SHA256: "+otherFingerprint)
	fmt.Fprintln(os.Stderr, "SERVER PASSWORD: ui-secret")
	fmt.Fprintln(os.Stderr, "SERVER CONTROL PASSWORD: control-secret")
	fmt.Fprintln(os.Stderr, "SERVER ADDRESS: https://127.0.0.1:1")

	if os.Getenv(fakeServerEnv+"_LONG_LINE") != "" {
		// longer than the reader's line buffer, then far more than a pipe holds
		w := bufio.NewWriter(os.Stderr)
		fmt.Fprintln(w, strings.Repeat("x", 5<<20))

		for range 20000 {
			fmt.Fprintln(w, strings.Repeat("y", 100))
		}

		w.Flush()
		fmt.Fprintln(os.Stdout, "still writing")
	}

	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = os.Remove(marker)
}

// newFakeShell returns a shell whose kopia is this test binary, and the
// directory in which running fake servers leave their marker.
func newFakeShell(t *testing.T) (*shell, string) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Setenv(fakeServerEnv, dir)

	return &shell{
		kopiaPath: exe,
		configs:   &configStore{dir: t.TempDir(), repos: map[string]bool{}},
		servers:   map[string]*repoServer{},
	}, dir
}

func runningFakeServers(t *testing.T, dir string) int {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, "*.running"))
	if err != nil {
		t.Fatal(err)
	}

	return len(matches)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)

	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %v", what)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// serverCertDirs counts the folders of server certificates in tmp.
func serverCertDirs(t *testing.T, tmp string) int {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(tmp, "kopia-ui-tls-*"))
	if err != nil {
		t.Fatal(err)
	}

	return len(matches)
}

func TestServer_startAndStop(t *testing.T) {
	// where the server's certificate is written
	tmp := t.TempDir()
	for _, name := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(name, tmp)
	}

	sh, dir := newFakeShell(t)
	s := newRepoServer("repository", sh)

	if err := s.start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.stop)

	waitFor(t, "the connection parameters", func() bool {
		_, _, _, ok := s.connection()
		return ok
	})

	address, password, transport, _ := s.connection()
	if address != "https://127.0.0.1:1" || password != "ui-secret" || transport == nil {
		t.Fatalf("unexpected connection %q %q", address, password)
	}

	s.mu.Lock()
	logged := strings.Join(s.logLines, "\n")
	pinned := s.certSHA256
	s.mu.Unlock()

	if len(pinned) != len(otherFingerprint) || pinned == otherFingerprint {
		t.Fatalf("the pinned certificate %q is not the one the server was started with", pinned)
	}

	if n := serverCertDirs(t, tmp); n != 1 {
		t.Fatalf("%v certificate folders while the server runs, want 1", n)
	}

	if strings.Contains(logged, "ui-secret") || strings.Contains(logged, "control-secret") {
		t.Fatalf("passwords were logged: %s", logged)
	}

	if n := runningFakeServers(t, dir); n != 1 {
		t.Fatalf("%v servers running, want 1", n)
	}

	s.stop()

	// stop returns only after the server has exited
	if n := runningFakeServers(t, dir); n != 0 {
		t.Fatalf("%v servers running after stop", n)
	}

	if _, _, _, ok := s.connection(); ok {
		t.Fatal("a stopped server has no connection")
	}

	if n := serverCertDirs(t, tmp); n != 0 {
		t.Fatalf("%v certificate folders left after stop", n)
	}

	if p := s.startupProblem(); !p.failed {
		t.Fatal("a stopped server is reported as not available")
	}

	s.stop() // stopping twice is fine
}

// TestServer_concurrentStartsLeaveOneServer is the double-clicked "Restart
// Server" button: every start must stop the server the previous one launched.
func TestServer_concurrentStartsLeaveOneServer(t *testing.T) {
	sh, dir := newFakeShell(t)
	s := newRepoServer("repository", sh)

	t.Cleanup(s.stop)

	const starts = 6

	var wg sync.WaitGroup

	errs := make(chan error, starts)

	for range starts {
		wg.Add(1)

		go func() {
			defer wg.Done()

			errs <- s.start()
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "the last server to start", func() bool {
		_, _, _, ok := s.connection()
		return ok
	})

	// every server but the last was stopped before its successor started
	if n := runningFakeServers(t, dir); n != 1 {
		t.Fatalf("%v servers running after %v concurrent starts, want 1", n, starts)
	}

	s.stop()

	if n := runningFakeServers(t, dir); n != 0 {
		t.Fatalf("%v servers left running", n)
	}
}

func TestServer_survivesVeryLongOutputLine(t *testing.T) {
	sh, _ := newFakeShell(t)
	t.Setenv(fakeServerEnv+"_LONG_LINE", "1")

	s := newRepoServer("repository", sh)

	if err := s.start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.stop)

	// the server gets past the long line only if its stderr is still being read
	waitFor(t, "the server to finish writing", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()

		for _, l := range s.logLines {
			if l == "still writing" {
				return true
			}
		}

		return false
	})
}

// paramServer returns a server with a current process, as after start.
func paramServer() (*repoServer, *exec.Cmd) {
	s := newRepoServer("repository", &shell{})
	cmd := &exec.Cmd{}
	s.cmd = cmd

	// as start does, with the certificate it made
	s.certSHA256 = testFingerprint
	s.transport = newPinnedTransport(testFingerprint)

	return s, cmd
}

func TestHandleParam_acceptsStartupParameters(t *testing.T) {
	s, cmd := paramServer()

	for line, secret := range map[string]bool{
		"SERVER PASSWORD: ui-secret":              true,
		"SERVER CONTROL PASSWORD: control-secret": true,
		"SERVER ADDRESS: https://127.0.0.1:51234": false,
		"SERVER CERTIFICATE: MIIB...":             true,
		"some log line":                           false,
		"ERROR something: failed":                 false,
	} {
		if got := s.handleParam(cmd, line); got != secret {
			t.Errorf("handleParam(%q) = %v, want %v", line, got, secret)
		}
	}

	address, password, transport, ok := s.connection()
	if !ok || address != "https://127.0.0.1:51234" || password != "ui-secret" || transport == nil {
		t.Fatalf("unexpected connection %q %q %v", address, password, ok)
	}

	if s.certSHA256 != testFingerprint || s.controlPassword != "control-secret" {
		t.Fatalf("unexpected fingerprint %q or control password", s.certSHA256)
	}
}

func TestHandleParam_ignoresLaterAndForeignParameters(t *testing.T) {
	s, cmd := paramServer()

	for _, line := range []string{
		"SERVER PASSWORD: ui-secret",
		"SERVER CONTROL PASSWORD: control-secret",
		"SERVER ADDRESS: https://127.0.0.1:51234",
	} {
		s.handleParam(cmd, line)
	}

	_, _, transport, _ := s.connection()

	// log output that looks like parameters, e.g. from a file name with a newline
	for _, line := range []string{
		"SERVER ADDRESS: https://attacker.example:443",
		"SERVER ADDRESS: https://127.0.0.1:6666",
		"SERVER CERT SHA256: " + otherFingerprint,
		"SERVER PASSWORD: other",
		"SERVER CONTROL PASSWORD: other",
	} {
		s.handleParam(cmd, line)
	}

	address, password, after, _ := s.connection()
	if address != "https://127.0.0.1:51234" || password != "ui-secret" || after != transport {
		t.Fatalf("a later line changed the connection: %q %q", address, password)
	}

	if s.certSHA256 != testFingerprint || s.controlPassword != "control-secret" {
		t.Fatal("a later line changed the pinned certificate or the control password")
	}

	// output of a process that is no longer the current one changes nothing,
	// and its secrets still stay out of the log
	fresh, _ := paramServer()

	if !fresh.handleParam(&exec.Cmd{}, "SERVER PASSWORD: stale") {
		t.Fatal("a stale password line must not be logged")
	}

	fresh.handleParam(&exec.Cmd{}, "SERVER ADDRESS: https://127.0.0.1:1")

	if fresh.password != "" || fresh.address != "" {
		t.Fatal("output of a stopped server was accepted")
	}
}

func TestHandleParam_refusesAddressesElsewhere(t *testing.T) {
	for _, address := range []string{
		"https://attacker.example:443",
		"https://192.168.1.10:51515",
		"https://127.0.0.1.attacker.example:443",
		"https://localhost:51515", // a name can be made to resolve elsewhere
		"http://127.0.0.1:51515",
		"https://127.0.0.1",
		"unix+https:///tmp/kopia.sock",
		"https://user@attacker.example:443",
		"",
		"not a url",
	} {
		s, cmd := paramServer()
		s.handleParam(cmd, "SERVER ADDRESS: "+address)

		if s.address != "" {
			t.Errorf("address %q was accepted", address)
		}

		// the real address is still accepted afterwards
		s.handleParam(cmd, "SERVER ADDRESS: https://[::1]:51515")

		if s.address != "https://[::1]:51515" {
			t.Errorf("after refusing %q the loopback address was not accepted", address)
		}
	}
}

// TestHandleParam_neverTakesTheCertificateFromOutput is the forged line that
// comes first: before any real parameter, a log line names a certificate and
// an address. The address is believed, but only the certificate the server
// was started with is trusted there, so no password can be sent to it.
func TestHandleParam_neverTakesTheCertificateFromOutput(t *testing.T) {
	s, cmd := paramServer()

	_, _, transport, _ := s.connection()

	for _, line := range []string{
		"SERVER CERT SHA256: " + otherFingerprint,
		"SERVER CERT SHA256: " + strings.ToUpper(otherFingerprint),
		"SERVER ADDRESS: https://127.0.0.1:6666",
		"SERVER PASSWORD: ui-secret",
	} {
		s.handleParam(cmd, line)
	}

	_, _, after, ok := s.connection()
	if !ok || after != transport || s.certSHA256 != testFingerprint {
		t.Fatalf("output changed the pinned certificate to %q", s.certSHA256)
	}
}

func TestStartupProblem_showsTheLastLogLines(t *testing.T) {
	s := newRepoServer("repository", &shell{})

	if p := s.startupProblem(); !p.failed || p.reason == "" {
		t.Fatalf("a server that never started: %+v", p)
	}

	s.cmd = &exec.Cmd{}
	s.startedAt = time.Now()

	if p := s.startupProblem(); p.failed {
		t.Fatalf("a server that just started is not a failure: %+v", p)
	}

	s.startedAt = time.Now().Add(-2 * serverStartTimeout)

	for i := range 2 * errorPageLogLines {
		s.appendLog(fmt.Sprintf("line %d", i))
	}

	p := s.startupProblem()
	if !p.failed || len(p.logs) != errorPageLogLines || p.logs[len(p.logs)-1] != fmt.Sprintf("line %d", 2*errorPageLogLines-1) {
		t.Fatalf("a server that does not finish starting: %+v", p)
	}
}

func TestAppendLog_keepsTheLastLines(t *testing.T) {
	s := newRepoServer("repository", &shell{})

	for i := range maxLogLines + 50 {
		s.appendLog(fmt.Sprintf("line %d", i))
	}

	if len(s.logLines) != maxLogLines || s.logLines[0] != "line 50" {
		t.Fatalf("got %v lines starting with %q", len(s.logLines), s.logLines[0])
	}
}
