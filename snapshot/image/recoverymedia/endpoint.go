package recoverymedia

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo"
)

// Endpoint is the network address a repository connection uses.
type Endpoint struct {
	// URL is an http or https URL whose server answers for the repository.
	URL string

	// Pinned is set when the connection trusts one certificate by its
	// fingerprint rather than certificate authorities.
	Pinned bool

	// HasOwnCA is set when the connection carries its own certificate
	// authority, as S3 connections made with --root-ca-pem-path do.
	HasOwnCA bool
}

// RepositoryEndpoint returns the endpoint of a repository connection, or nil
// for storage that is not reached over HTTP, such as a local folder or SFTP.
func RepositoryEndpoint(lc *repo.LocalConfig) (*Endpoint, error) {
	if lc.APIServer != nil {
		return &Endpoint{URL: lc.APIServer.BaseURL, Pinned: lc.APIServer.TrustedServerCertificateFingerprint != "", HasOwnCA: len(lc.APIServer.RootCA) > 0}, nil
	}

	if lc.Storage == nil {
		return nil, errors.New("the repository connection has neither storage nor a server")
	}

	cfg, err := storageConfig(lc)
	if err != nil {
		return nil, err
	}

	str := func(key string) string {
		s, _ := cfg[key].(string)
		return s
	}

	switch lc.Storage.Type {
	case "s3":
		host := str("endpoint")
		if host == "" {
			host = "s3.amazonaws.com"
		}

		scheme := "https"
		if b, _ := cfg["doNotUseTLS"].(bool); b {
			scheme = "http"
		}

		return &Endpoint{URL: scheme + "://" + host, HasOwnCA: str("rootCA") != ""}, nil
	case "gcs":
		return &Endpoint{URL: "https://storage.googleapis.com"}, nil
	case "b2":
		return &Endpoint{URL: "https://api.backblazeb2.com"}, nil
	case "azureBlob":
		domain := str("storageDomain")
		if domain == "" {
			domain = "blob.core.windows.net"
		}

		return &Endpoint{URL: "https://" + str("storageAccount") + "." + domain}, nil
	case "webdav":
		return &Endpoint{URL: str("url"), Pinned: str("trustedServerCertificateFingerprint") != ""}, nil
	default:
		return nil, nil
	}
}

// storageConfig returns the storage options of a connection as JSON values.
func storageConfig(lc *repo.LocalConfig) (map[string]any, error) {
	b, err := json.Marshal(lc.Storage.Config)
	if err != nil {
		return nil, errors.Wrap(err, "unable to encode the storage options")
	}

	cfg := map[string]any{}

	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, errors.Wrap(err, "unable to decode the storage options")
	}

	return cfg, nil
}

// LocalRepositoryPath returns the folder of a repository on a local or
// attached disk, or "" for other storage.
func LocalRepositoryPath(lc *repo.LocalConfig) string {
	if lc.Storage == nil || lc.Storage.Type != "filesystem" {
		return ""
	}

	cfg, err := storageConfig(lc)
	if err != nil {
		return ""
	}

	p, _ := cfg["path"].(string)

	return p
}

// DescribeRepository describes a repository connection for people.
func DescribeRepository(lc *repo.LocalConfig) string {
	if lc.APIServer != nil {
		return "Kopia repository server at " + lc.APIServer.BaseURL
	}

	if lc.Storage == nil {
		return "unknown repository"
	}

	cfg, err := storageConfig(lc)
	if err != nil {
		return lc.Storage.Type + " repository"
	}

	str := func(key string) string {
		s, _ := cfg[key].(string)
		return s
	}

	join := func(parts ...string) string {
		var out []string

		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}

		return strings.Join(out, " ")
	}

	switch lc.Storage.Type {
	case "filesystem":
		return "folder " + str("path")
	case "s3":
		return join("S3 bucket", str("bucket"), "prefix", str("prefix"), "at", str("endpoint"))
	case "gcs", "b2":
		return join(strings.ToUpper(lc.Storage.Type), "bucket", str("bucket"), "prefix", str("prefix"))
	case "azureBlob":
		return join("Azure container", str("container"), "in account", str("storageAccount"))
	case "webdav":
		return "WebDAV server " + str("url")
	case "sftp":
		return join("SFTP", str("username")+"@"+str("host"), str("path"))
	default:
		return lc.Storage.Type + " repository"
	}
}

const dialTimeout = 20 * time.Second

// ServerRoot connects to an https endpoint, verifies its certificate with
// this computer's trusted certificate authorities and returns the root of the
// verified chain as PEM.
func ServerRoot(ctx context.Context, endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.Wrapf(err, "invalid endpoint %v", endpoint)
	}

	if u.Scheme != "https" {
		return "", errors.Errorf("%v does not use TLS", endpoint)
	}

	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}

	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout},
		Config:    &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12},
	}

	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return "", errors.Wrapf(err, "unable to verify the certificate of %v", u.Host)
	}
	defer conn.Close() //nolint:errcheck

	tc, ok := conn.(*tls.Conn)
	if !ok {
		return "", errors.New("not a TLS connection")
	}

	chains := tc.ConnectionState().VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return "", errors.Errorf("%v presented no verified certificate chain", u.Host)
	}

	root := chains[0][len(chains[0])-1]

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})), nil
}

// ParseCertificates parses PEM certificates.
func ParseCertificates(pemData string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate

	rest := []byte(pemData)

	for {
		var block *pem.Block

		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}

		if block.Type != "CERTIFICATE" {
			continue
		}

		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.Wrap(err, "invalid certificate")
		}

		out = append(out, c)
	}

	if len(out) == 0 {
		return nil, errors.New("no PEM certificate found")
	}

	return out, nil
}
