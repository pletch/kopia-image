package cli_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/internal/testutil"
	"github.com/kopia/kopia/internal/tlsutil"
	"github.com/kopia/kopia/tests/testenv"
)

// startTLSServer starts "kopia server start" with the given extra flags and
// returns the host:port it listens on.
func startTLSServer(t *testing.T, env *testenv.CLITest, flags ...string) string {
	t.Helper()

	var sp testutil.ServerParameters

	args := append([]string{
		"server", "start",
		"--address=127.0.0.1:0",
		"--random-password",
		"--random-server-control-password",
	}, flags...)

	wait, kill := env.RunAndProcessStderr(t, sp.ProcessOutput, args...)

	t.Cleanup(func() {
		kill()
		wait() //nolint:errcheck
	})

	u, err := url.Parse(sp.BaseURL)
	require.NoError(t, err)
	require.Equal(t, "https", u.Scheme)

	return u.Host
}

func dialServer(t *testing.T, addr, serverName string, roots *x509.CertPool) tls.ConnectionState {
	t.Helper()

	d := &tls.Dialer{Config: &tls.Config{
		ServerName: serverName,
		RootCAs:    roots,
		NextProtos: []string{"h2", "http/1.1"},
		MinVersion: tls.VersionTLS12,
		// self-signed test certificates are compared by their raw bytes instead.
		InsecureSkipVerify: roots == nil, //nolint:gosec
	}}

	conn, err := d.DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)

	defer conn.Close()

	tc, ok := conn.(*tls.Conn)
	require.True(t, ok)

	return tc.ConnectionState()
}

func TestServerStart_ReloadsChangedCertFiles(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	dir := testutil.TempDirectory(t)
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")

	initial := writeSelfSignedCert(t, certFile, keyFile)

	addr := startTLSServer(t, env, "--tls-cert-file="+certFile, "--tls-key-file="+keyFile)

	require.Equal(t, initial.Raw, dialServer(t, addr, "", nil).PeerCertificates[0].Raw)

	replaced := writeSelfSignedCert(t, certFile, keyFile)

	// files are checked for changes every 10 seconds.
	require.Eventually(t, func() bool {
		return bytes.Equal(dialServer(t, addr, "", nil).PeerCertificates[0].Raw, replaced.Raw)
	}, 30*time.Second, 500*time.Millisecond)
}

func writeSelfSignedCert(t *testing.T, certFile, keyFile string) *x509.Certificate {
	t.Helper()

	cert, key, err := tlsutil.GenerateServerCertificate(testlogging.Context(t), 2048, time.Hour, []string{"127.0.0.1"})
	require.NoError(t, err)

	require.NoError(t, tlsutil.WriteCertificateToFile(certFile, cert))
	require.NoError(t, tlsutil.WritePrivateKeyToFile(keyFile, key))

	return cert
}
