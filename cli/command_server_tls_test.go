package cli_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/acmecert/acmetest"
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

func TestServerStart_ACMEFlagConflicts(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))

	cases := map[string][]string{
		"cert file":     {"--tls-cert-file=a.crt"},
		"key file":      {"--tls-key-file=a.key"},
		"generate cert": {"--tls-generate-cert"},
		"insecure":      {"--insecure"},
		"bad alpn port": {"--tls-acme-alpn-port=70000"},
	}

	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"server", "start", "--address=127.0.0.1:0", "--tls-acme-domain=kopia.example"}, flags...)

			_, stderr := env.RunAndExpectFailure(t, args...)
			require.Contains(t, strings.ToLower(strings.Join(stderr, "\n")), "acme")
		})
	}
}

func TestServerStart_ACMECertificate(t *testing.T) {
	testCA := acmetest.Start(t)

	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	storageDir := testutil.TempDirectory(t)

	addr := startTLSServer(t, env,
		"--tls-acme-domain="+acmetest.Domain,
		"--tls-acme-directory="+testCA.DirectoryURL,
		"--tls-acme-ca-root="+testCA.RootFile,
		"--tls-acme-storage-dir="+storageDir,
		"--tls-acme-alpn-port="+strconv.Itoa(testCA.TLSALPNPort),
	)

	state := dialServer(t, addr, acmetest.Domain, testCA.RootPool(t))
	require.Equal(t, []string{acmetest.Domain}, state.PeerCertificates[0].DNSNames)
	require.Equal(t, "h2", state.NegotiatedProtocol, "gRPC clients need HTTP/2")

	entries, err := os.ReadDir(storageDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "certificate should be persisted in the storage directory")
}

func TestServerStart_ACMEInterruptibleWhileObtainingCertificate(t *testing.T) {
	env := testenv.NewCLITest(t, testenv.RepoFormatNotImportant, testenv.NewInProcRunner(t))
	env.RunAndExpectSuccess(t, "repo", "create", "filesystem", "--path", env.RepoDir)

	// nothing listens on this port, so issuance keeps being retried.
	unreachable := "https://127.0.0.1:" + strconv.Itoa(acmetest.FreePort(t)) + "/dir"

	wait, kill := env.RunAndProcessStderr(t, func(line string) bool {
		return !strings.Contains(line, "will retry")
	},
		"server", "start",
		"--address=127.0.0.1:0",
		"--random-password",
		"--random-server-control-password",
		"--tls-acme-domain="+acmetest.Domain,
		"--tls-acme-directory="+unreachable,
		"--tls-acme-storage-dir="+testutil.TempDirectory(t),
	)

	kill()

	done := make(chan error, 1)

	go func() { done <- wait() }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("server did not stop after being interrupted while obtaining a certificate")
	}
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
