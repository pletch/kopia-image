package recoverymedia

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
)

func storageConfigOf(typ string, cfg map[string]any) *repo.LocalConfig {
	return &repo.LocalConfig{Storage: &blob.ConnectionInfo{Type: typ, Config: cfg}}
}

func TestRepositoryEndpoint(t *testing.T) {
	cases := []struct {
		name string
		lc   *repo.LocalConfig
		want *Endpoint
	}{
		{"server pinned", &repo.LocalConfig{APIServer: &repo.APIServerInfo{BaseURL: "https://nas:51515", TrustedServerCertificateFingerprint: "ab"}}, &Endpoint{URL: "https://nas:51515", Pinned: true}},
		{"server with CA", &repo.LocalConfig{APIServer: &repo.APIServerInfo{BaseURL: "https://nas:51515", RootCA: []byte("pem")}}, &Endpoint{URL: "https://nas:51515", HasOwnCA: true}},
		{"s3 default", storageConfigOf("s3", map[string]any{"bucket": "b"}), &Endpoint{URL: "https://s3.amazonaws.com"}},
		{"s3 minio without TLS", storageConfigOf("s3", map[string]any{"endpoint": "minio:9000", "doNotUseTLS": true}), &Endpoint{URL: "http://minio:9000"}},
		{"s3 with CA", storageConfigOf("s3", map[string]any{"endpoint": "s3.lan", "rootCA": "cGVt"}), &Endpoint{URL: "https://s3.lan", HasOwnCA: true}},
		{"b2", storageConfigOf("b2", map[string]any{}), &Endpoint{URL: "https://api.backblazeb2.com"}},
		{"azure", storageConfigOf("azureBlob", map[string]any{"storageAccount": "acct"}), &Endpoint{URL: "https://acct.blob.core.windows.net"}},
		{"webdav", storageConfigOf("webdav", map[string]any{"url": "https://dav/x"}), &Endpoint{URL: "https://dav/x"}},
		{"filesystem", storageConfigOf("filesystem", map[string]any{"path": `D:\repo`}), nil},
		{"sftp", storageConfigOf("sftp", map[string]any{"host": "h"}), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RepositoryEndpoint(tc.lc)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestDescribeRepository(t *testing.T) {
	require.Equal(t, "folder D:\\repo", DescribeRepository(storageConfigOf("filesystem", map[string]any{"path": `D:\repo`})))
	require.Equal(t, "S3 bucket backups prefix pc/ at s3.lan", DescribeRepository(storageConfigOf("s3", map[string]any{"bucket": "backups", "prefix": "pc/", "endpoint": "s3.lan"})))
	require.Equal(t, "Kopia repository server at https://nas:51515", DescribeRepository(&repo.LocalConfig{APIServer: &repo.APIServerInfo{BaseURL: "https://nas:51515"}}))
	require.Equal(t, `D:\repo`, LocalRepositoryPath(storageConfigOf("filesystem", map[string]any{"path": `D:\repo`})))
	require.Empty(t, LocalRepositoryPath(storageConfigOf("s3", map[string]any{"bucket": "b"})))
}

func TestParseCertificates(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "Test Root"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(0, 0).Add(24 * time.Hour),
		IsCA:         true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	p := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	certs, err := ParseCertificates("junk\n" + p + p)
	require.NoError(t, err)
	require.Len(t, certs, 2)
	require.Equal(t, "Test Root", certs[0].Subject.CommonName)

	_, err = ParseCertificates("not a certificate")
	require.ErrorContains(t, err, "no PEM certificate")
}

func TestInfoRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), InfoFileName)
	require.NoError(t, WriteInfo(p, &Info{Computer: "desktop", Sources: []string{"tim@desktop:image:disk:x"}}))

	got, err := ReadInfo(p)
	require.NoError(t, err)
	require.Equal(t, currentFormatVersion, got.FormatVersion)
	require.Equal(t, "desktop", got.Computer)
}
