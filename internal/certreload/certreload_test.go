package certreload

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/internal/tlsutil"
)

const testKeySize = 2048

type testCert struct {
	raw []byte
}

// writeTestCert generates a self-signed cert/key pair, writes it to certFile/keyFile
// and returns the DER bytes of the certificate.
func writeTestCert(t *testing.T, certFile, keyFile string) testCert {
	t.Helper()

	cert, key, err := tlsutil.GenerateServerCertificate(testlogging.Context(t), testKeySize, time.Hour, []string{"127.0.0.1"})
	require.NoError(t, err)

	require.NoError(t, tlsutil.WriteCertificateToFile(certFile, cert))
	require.NoError(t, tlsutil.WritePrivateKeyToFile(keyFile, key))

	return testCert{raw: cert.Raw}
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type fixture struct {
	r        *Reloader
	clock    *fakeClock
	certFile string
	keyFile  string
}

func newFixture(t *testing.T) (*fixture, testCert) {
	t.Helper()

	dir := t.TempDir()
	f := &fixture{
		clock:    &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		certFile: filepath.Join(dir, "server.crt"),
		keyFile:  filepath.Join(dir, "server.key"),
	}

	initial := writeTestCert(t, f.certFile, f.keyFile)

	r, err := New(testlogging.Context(t), f.certFile, f.keyFile)
	require.NoError(t, err)

	r.now = f.clock.now
	f.r = r

	return f, initial
}

// replaceCert writes a new cert/key pair and makes sure the file stamps differ
// from the previous ones, even on filesystems with coarse mtime resolution.
func (f *fixture) replaceCert(t *testing.T) testCert {
	t.Helper()

	c := writeTestCert(t, f.certFile, f.keyFile)
	f.bumpModTimes(t)

	return c
}

func (f *fixture) bumpModTimes(t *testing.T) {
	t.Helper()

	future := time.Now().Add(time.Hour) //nolint:forbidigo

	require.NoError(t, os.Chtimes(f.certFile, future, future))
	require.NoError(t, os.Chtimes(f.keyFile, future, future))
}

func (f *fixture) served(t *testing.T) []byte {
	t.Helper()

	c, err := f.r.GetCertificate(nil)
	require.NoError(t, err)
	require.NotNil(t, c)

	return c.Certificate[0]
}

func requireSameCert(t *testing.T, want testCert, got []byte) {
	t.Helper()

	require.True(t, bytes.Equal(want.raw, got), "served certificate does not match expected one")
}

func TestReloader_ReloadsChangedFilesAfterCheckInterval(t *testing.T) {
	f, initial := newFixture(t)

	requireSameCert(t, initial, f.served(t))

	replaced := f.replaceCert(t)

	// within the check interval the old cert keeps being served without touching disk.
	f.clock.advance(DefaultCheckInterval / 2)
	requireSameCert(t, initial, f.served(t))

	f.clock.advance(DefaultCheckInterval)
	requireSameCert(t, replaced, f.served(t))
}

func TestReloader_ReloadForcesImmediateReload(t *testing.T) {
	f, initial := newFixture(t)

	requireSameCert(t, initial, f.served(t))

	replaced := f.replaceCert(t)
	f.r.Reload()

	requireSameCert(t, replaced, f.served(t))
}

func TestReloader_KeepsCurrentCertWhenReloadFails(t *testing.T) {
	cases := []struct {
		name       string
		breakFiles func(t *testing.T, f *fixture)
	}{
		{
			name: "garbage cert file",
			breakFiles: func(t *testing.T, f *fixture) {
				t.Helper()
				require.NoError(t, os.WriteFile(f.certFile, []byte("not a certificate"), 0o600))
			},
		},
		{
			name: "key does not match cert",
			breakFiles: func(t *testing.T, f *fixture) {
				t.Helper()
				other := t.TempDir()
				writeTestCert(t, f.certFile, filepath.Join(other, "unused.key"))
			},
		},
		{
			name: "cert file removed",
			breakFiles: func(t *testing.T, f *fixture) {
				t.Helper()
				require.NoError(t, os.Remove(f.certFile))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, initial := newFixture(t)

			tc.breakFiles(t, f)

			if _, err := os.Stat(f.certFile); err == nil {
				f.bumpModTimes(t)
			}

			f.clock.advance(DefaultCheckInterval)
			requireSameCert(t, initial, f.served(t))

			f.r.Reload()
			requireSameCert(t, initial, f.served(t))
		})
	}
}

func TestReloader_RecoversAfterBrokenPairIsFixed(t *testing.T) {
	f, initial := newFixture(t)

	// simulates a renewer that has written the new cert but not yet the new key.
	writeTestCert(t, f.certFile, filepath.Join(t.TempDir(), "pending.key"))
	f.bumpModTimes(t)

	f.clock.advance(DefaultCheckInterval)
	requireSameCert(t, initial, f.served(t))

	fixed := writeTestCert(t, f.certFile, f.keyFile)
	future := time.Now().Add(2 * time.Hour) //nolint:forbidigo
	require.NoError(t, os.Chtimes(f.certFile, future, future))
	require.NoError(t, os.Chtimes(f.keyFile, future, future))

	f.clock.advance(DefaultCheckInterval)
	requireSameCert(t, fixed, f.served(t))
}

func TestNew_FailsOnInvalidPair(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")

	require.NoError(t, os.WriteFile(certFile, []byte("garbage"), 0o600))
	require.NoError(t, os.WriteFile(keyFile, []byte("garbage"), 0o600))

	_, err := New(testlogging.Context(t), certFile, keyFile)
	require.Error(t, err)

	_, err = New(testlogging.Context(t), filepath.Join(dir, "missing.crt"), keyFile)
	require.Error(t, err)
}
