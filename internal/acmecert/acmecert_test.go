package acmecert

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/kopia/kopia/internal/acmecert/acmetest"
	"github.com/kopia/kopia/internal/testlogging"
)

func optionsForCA(t *testing.T, testCA *acmetest.CA) Options {
	t.Helper()

	return Options{
		Domains:        []string{acmetest.Domain},
		DirectoryURL:   testCA.DirectoryURL,
		CARootFile:     testCA.RootFile,
		StorageDir:     t.TempDir(),
		AltTLSALPNPort: testCA.TLSALPNPort,
	}
}

func newTestManager(t *testing.T, opts Options) *Manager {
	t.Helper()

	m, err := New(testlogging.Context(t), opts, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(m.Close)

	return m
}

// servedLeaf performs a TLS handshake against cfg and returns the leaf certificate.
func servedLeaf(t *testing.T, cfg *tls.Config, serverName string, roots *x509.CertPool) *x509.Certificate {
	t.Helper()

	l, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	require.NoError(t, err)

	defer l.Close()

	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}

		defer c.Close()

		if tc, ok := c.(*tls.Conn); ok {
			tc.HandshakeContext(t.Context()) //nolint:errcheck
		}
	}()

	d := &tls.Dialer{Config: &tls.Config{
		ServerName: serverName,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
		// without a server name there is nothing to verify against; callers compare the raw certificate.
		InsecureSkipVerify: serverName == "", //nolint:gosec
	}}

	conn, err := d.DialContext(t.Context(), "tcp", l.Addr().String())
	require.NoError(t, err)

	defer conn.Close()

	tc, ok := conn.(*tls.Conn)
	require.True(t, ok)

	return tc.ConnectionState().PeerCertificates[0]
}

func TestNew_ObtainsCertificateAndReusesStoredOne(t *testing.T) {
	testCA := acmetest.Start(t)
	opts := optionsForCA(t, testCA)

	m := newTestManager(t, opts)
	require.Equal(t, 1, testCA.Orders())

	roots := testCA.RootPool(t)

	leaf := servedLeaf(t, m.TLSConfig(), acmetest.Domain, roots)
	require.Equal(t, []string{acmetest.Domain}, leaf.DNSNames)

	// clients without SNI (e.g. connecting by IP address) get the same certificate.
	noSNI := servedLeaf(t, m.TLSConfig(), "", nil)
	require.Equal(t, leaf.Raw, noSNI.Raw)

	require.Equal(t, []string{"h2", "http/1.1"}, m.TLSConfig().NextProtos[:2])

	m.Close()

	// a second manager using the same storage loads the certificate without a new order.
	m2 := newTestManager(t, opts)
	require.Equal(t, 1, testCA.Orders())
	require.Equal(t, leaf.Raw, servedLeaf(t, m2.TLSConfig(), acmetest.Domain, roots).Raw)
}

func TestManager_RenewsInBackground(t *testing.T) {
	testCA := acmetest.Start(t)

	opts := optionsForCA(t, testCA)
	// treat every certificate as due for renewal and check frequently.
	opts.renewCheckInterval = 200 * time.Millisecond
	opts.renewalWindowRatio = 1.0

	m := newTestManager(t, opts)

	roots := testCA.RootPool(t)
	initial := servedLeaf(t, m.TLSConfig(), acmetest.Domain, roots)

	// the same tls.Config keeps working and starts serving the renewed certificate.
	cfg := m.TLSConfig()

	require.Eventually(t, func() bool {
		return !bytes.Equal(initial.Raw, servedLeaf(t, cfg, acmetest.Domain, roots).Raw)
	}, 30*time.Second, 100*time.Millisecond)

	require.Greater(t, testCA.Orders(), 1)
}

func TestNew_FailsWhenCAIsUnreachable(t *testing.T) {
	opts := Options{
		Domains:      []string{acmetest.Domain},
		DirectoryURL: "https://127.0.0.1:" + strconv.Itoa(acmetest.FreePort(t)) + "/dir",
		StorageDir:   t.TempDir(),
	}

	// issuance is retried until the context is done.
	ctx, cancel := context.WithTimeout(testlogging.Context(t), 2*time.Second)
	defer cancel()

	_, err := New(ctx, opts, zap.NewNop())
	require.Error(t, err)
}

func TestNew_FailsOnInvalidCARootFile(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.pem")
	require.NoError(t, os.WriteFile(bad, []byte("not pem"), 0o600))

	for _, fname := range []string{bad, filepath.Join(t.TempDir(), "missing.pem")} {
		_, err := New(testlogging.Context(t), Options{
			Domains:    []string{acmetest.Domain},
			CARootFile: fname,
			StorageDir: t.TempDir(),
		}, zap.NewNop())
		require.Error(t, err)
	}
}

func TestOptionsValidate_Errors(t *testing.T) {
	cases := map[string]Options{
		"no domains":        {StorageDir: "/tmp/x"},
		"empty domain":      {Domains: []string{" "}, StorageDir: "/tmp/x"},
		"no storage dir":    {Domains: []string{"a.example"}},
		"negative port":     {Domains: []string{"a.example"}, StorageDir: "/tmp/x", AltTLSALPNPort: -1},
		"port out of range": {Domains: []string{"a.example"}, StorageDir: "/tmp/x", AltTLSALPNPort: 70000},
	}

	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, opts.Validate())
		})
	}
}

func TestOptionsValidate_Success(t *testing.T) {
	opts := Options{Domains: []string{"kopia.home", "mint-box.home"}, StorageDir: "/tmp/x", AltTLSALPNPort: 8443}
	require.NoError(t, opts.Validate())
}
