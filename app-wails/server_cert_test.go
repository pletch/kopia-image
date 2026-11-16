package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// serveWith starts a TLS server that presents the certificate in the files of c.
func serveWith(t *testing.T, c *serverCert) *httptest.Server {
	t.Helper()

	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		t.Fatalf("the certificate files do not load: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv
}

func TestServerCert_pinsOnlyItsOwnServer(t *testing.T) {
	mine, err := newServerCert()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(mine.remove)

	other, err := newServerCert()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(other.remove)

	if mine.fingerprint == other.fingerprint {
		t.Fatal("two certificates share a fingerprint")
	}

	client := &http.Client{Transport: newPinnedTransport(mine.fingerprint)}

	resp, err := client.Get(serveWith(t, mine).URL)
	if err != nil {
		t.Fatalf("the server started with the certificate is refused: %v", err)
	}

	resp.Body.Close()

	// a server elsewhere on this computer, which a forged address would name
	if resp, err := client.Get(serveWith(t, other).URL); err == nil {
		resp.Body.Close()
		t.Fatal("a server with another certificate was accepted")
	}
}

func TestServerCert_remove(t *testing.T) {
	c, err := newServerCert()
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{c.certFile, c.keyFile} {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}

	c.remove()

	if _, err := os.Stat(c.dir); !os.IsNotExist(err) {
		t.Fatalf("the certificate folder is still there: %v", err)
	}
}

// TestServerCert_realKopia starts the kopia executable named by
// KOPIA_UI_TEST_KOPIA_EXE the way the shell does and talks to it through the
// pinned transport, to prove that kopia accepts the certificate made here.
func TestServerCert_realKopia(t *testing.T) {
	exe := os.Getenv("KOPIA_UI_TEST_KOPIA_EXE")
	if exe == "" {
		t.Skip("set KOPIA_UI_TEST_KOPIA_EXE to a kopia executable")
	}

	sh := &shell{
		kopiaPath: exe,
		configs:   &configStore{dir: t.TempDir(), repos: map[string]bool{}},
		servers:   map[string]*repoServer{},
	}

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

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address+"/", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	req.SetBasicAuth(uiUser, password)

	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()

		t.Fatalf("unable to reach the server: %v\n%v", err, s.logLines)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %v", resp.Status)
	}
}
