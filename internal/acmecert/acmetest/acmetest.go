// Package acmetest runs an in-process ACME certificate authority for tests.
package acmetest

import (
	"crypto/x509"
	"encoding/pem"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/letsencrypt/pebble/v2/ca"
	"github.com/letsencrypt/pebble/v2/db"
	"github.com/letsencrypt/pebble/v2/va"
	"github.com/letsencrypt/pebble/v2/wfe"
	"github.com/stretchr/testify/require"
)

// Domain resolves to the loopback address, so the CA can validate TLS-ALPN-01
// challenges served by the code under test.
const Domain = "localhost"

// pebbleNewOrderPath is Pebble's (unexported) new-order endpoint.
const pebbleNewOrderPath = "/order-plz"

// CA is a running test certificate authority.
type CA struct {
	// DirectoryURL is the ACME directory URL.
	DirectoryURL string

	// RootFile is a PEM file with the roots for both the ACME API endpoint and
	// the certificates the CA issues.
	RootFile string

	// TLSALPNPort is the port on which the CA validates TLS-ALPN-01 challenges.
	TLSALPNPort int

	orders atomic.Int32
}

// Orders returns the number of new-order requests, which tells issuance apart
// from loading a certificate from storage.
func (c *CA) Orders() int {
	return int(c.orders.Load())
}

// RootPool returns a pool with the CA's roots.
func (c *CA) RootPool(tb testing.TB) *x509.CertPool {
	tb.Helper()

	b, err := os.ReadFile(c.RootFile)
	require.NoError(tb, err)

	pool := x509.NewCertPool()
	require.True(tb, pool.AppendCertsFromPEM(b))

	return pool
}

// Start runs an in-process Pebble ACME server whose validator performs real
// TLS-ALPN-01 checks against a free local port, returned in CA.TLSALPNPort.
// The server is stopped when the test ends.
func Start(t *testing.T) *CA {
	t.Helper()

	return StartWithResolver(t, "")
}

// StartWithResolver is like Start, but the CA looks up domains and DNS-01
// challenge records using the DNS server at resolverAddr (host:port) instead of
// the system resolver.
func StartWithResolver(t *testing.T, resolverAddr string) *CA {
	t.Helper()

	t.Setenv("PEBBLE_VA_NOSLEEP", "1")
	t.Setenv("PEBBLE_WFE_NONCEREJECT", "0")
	t.Setenv("PEBBLE_AUTHZREUSE", "0")

	c := &CA{TLSALPNPort: FreePort(t)}

	logger := stdlog.New(io.Discard, "", 0)
	store := db.NewMemoryStore()
	authority := ca.New(logger, store, "", "ecdsa", 0, 1, map[string]ca.Profile{
		"default": {Description: "default"},
	})
	validator := va.New(logger, 0, c.TLSALPNPort, false, resolverAddr, store)
	frontEnd := wfe.New(logger, store, validator, authority, nil, false, false, 0, 0)
	handler := frontEnd.Handler()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pebbleNewOrderPath {
			c.orders.Add(1)
		}

		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	c.DirectoryURL = srv.URL + wfe.DirectoryPath
	c.RootFile = filepath.Join(t.TempDir(), "roots.pem")

	f, err := os.Create(c.RootFile)
	require.NoError(t, err)

	require.NoError(t, pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	require.NoError(t, pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: authority.GetRootCert(0).DER}))
	require.NoError(t, f.Close())

	return c
}

// FreePort returns a currently unused local TCP port.
func FreePort(tb testing.TB) int {
	tb.Helper()

	l, err := (&net.ListenConfig{}).Listen(tb.Context(), "tcp", "127.0.0.1:0")
	require.NoError(tb, err)

	defer l.Close() //nolint:errcheck

	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(tb, ok)

	return addr.Port
}
