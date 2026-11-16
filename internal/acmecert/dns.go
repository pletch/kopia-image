package acmecert

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/libdns/libdns"
	"github.com/pkg/errors"
)

const (
	// maxTokenFileSize is far above the size of any real API token and only
	// guards against reading an unrelated large file into memory.
	maxTokenFileSize = 4096

	cloudflareAPITimeout = 30 * time.Second
)

// DNSProvider publishes and removes the temporary TXT records that prove
// control of a domain in the DNS-01 challenge.
type DNSProvider = certmagic.DNSProvider

// TokenSource returns the API token of a DNS provider. It is called before
// every DNS change, so a token that was rotated while the server is running is
// picked up at the next certificate renewal.
//
// Errors returned by a TokenSource must not contain the token.
type TokenSource func() (string, error)

// StaticToken returns a TokenSource for a token that is already in memory.
func StaticToken(token string) TokenSource {
	return func() (string, error) {
		return cleanToken(token)
	}
}

// TokenFromFile returns a TokenSource that reads the token from a file, such as
// a systemd credential or a container secret. Surrounding whitespace is ignored.
func TokenFromFile(path string) TokenSource {
	return func() (string, error) {
		f, err := os.Open(path) //nolint:gosec
		if err != nil {
			return "", errors.Wrap(err, "unable to open API token file")
		}

		defer f.Close() //nolint:errcheck

		b, err := io.ReadAll(io.LimitReader(f, maxTokenFileSize+1))
		if err != nil {
			return "", errors.Wrapf(err, "unable to read API token file %v", path)
		}

		if len(b) > maxTokenFileSize {
			return "", errors.Errorf("API token file %v is too large", path)
		}

		token, err := cleanToken(string(b))

		return token, errors.Wrapf(err, "invalid API token file %v", path)
	}
}

// cleanToken trims token and rejects values that cannot be an API token. The
// returned errors deliberately do not include any part of the token.
func cleanToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("API token is empty")
	}

	for _, c := range token {
		// anything else, such as a line break, cannot be sent in an HTTP header.
		if c <= ' ' || c > '~' {
			return "", errors.New("API token contains whitespace or non-printable characters")
		}
	}

	return token, nil
}

// NewCloudflareDNSProvider returns a DNSProvider for zones hosted by Cloudflare.
//
// The token must be a scoped API token (not the global API key) with the
// Zone:Read and DNS:Edit permissions on the zones of the certificate's domains.
func NewCloudflareDNSProvider(token TokenSource) DNSProvider {
	return &cloudflareDNSProvider{
		token:      token,
		httpClient: &http.Client{Timeout: cloudflareAPITimeout},
	}
}

type cloudflareDNSProvider struct {
	token      TokenSource
	httpClient cloudflare.HTTPClient
}

func (p *cloudflareDNSProvider) provider() (*cloudflare.Provider, error) {
	token, err := p.token()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get the Cloudflare API token")
	}

	return &cloudflare.Provider{APIToken: token, HTTPClient: p.httpClient}, nil
}

func (p *cloudflareDNSProvider) AppendRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	cf, err := p.provider()
	if err != nil {
		return nil, err
	}

	//nolint:wrapcheck
	return cf.AppendRecords(ctx, zone, recs)
}

func (p *cloudflareDNSProvider) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	cf, err := p.provider()
	if err != nil {
		return nil, err
	}

	//nolint:wrapcheck
	return cf.DeleteRecords(ctx, zone, recs)
}
