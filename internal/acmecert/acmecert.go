// Package acmecert obtains and renews the server TLS certificate from an ACME
// certificate authority (such as Let's Encrypt or step-ca) using the TLS-ALPN-01
// challenge, and serves it without requiring a restart on renewal.
package acmecert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// DefaultDirectoryURL is the ACME directory used when none is specified.
const DefaultDirectoryURL = certmagic.LetsEncryptProductionCA

// Options configures certificate management.
type Options struct {
	// Domains are the DNS names the certificate is issued for. The first one is
	// also served to clients that do not send SNI. Must not be empty.
	Domains []string

	// DirectoryURL is the ACME directory of the CA. Defaults to DefaultDirectoryURL.
	DirectoryURL string

	// CARootFile is an optional PEM file with additional root certificates to
	// trust when connecting to the ACME server, in addition to the system roots.
	CARootFile string

	// Email is an optional contact address for the ACME account.
	Email string

	// StorageDir holds the ACME account, private keys and certificates. Required.
	StorageDir string

	// AltTLSALPNPort is the port on which to listen for TLS-ALPN-01 challenges
	// when it is not 443. The CA always validates on port 443, so a non-zero
	// value is only useful when 443 is forwarded to this port.
	AltTLSALPNPort int

	// renewCheckInterval and renewalWindowRatio override certmagic defaults in tests.
	renewCheckInterval time.Duration
	renewalWindowRatio float64
}

// Validate checks that the options are complete.
func (o Options) Validate() error {
	if len(o.Domains) == 0 {
		return errors.New("at least one domain is required")
	}

	for _, d := range o.Domains {
		if strings.TrimSpace(d) == "" {
			return errors.New("domain names must not be empty")
		}
	}

	if o.StorageDir == "" {
		return errors.New("storage directory is required")
	}

	if o.AltTLSALPNPort < 0 || o.AltTLSALPNPort > 65535 {
		return errors.Errorf("invalid TLS-ALPN challenge port: %v", o.AltTLSALPNPort)
	}

	return nil
}

// Manager keeps the server certificate issued and renewed in the background.
type Manager struct {
	cache     *certmagic.Cache
	cfg       *certmagic.Config
	closeOnce sync.Once
}

// New returns a Manager with a valid certificate for all of opts.Domains.
//
// It loads the certificate from opts.StorageDir or, when none is stored or it
// needs renewal, obtains one from the CA, blocking until that completes.
// Afterwards the certificate is renewed in the background until Close is called.
func New(ctx context.Context, opts Options, logger *zap.Logger) (*Manager, error) {
	if err := opts.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid ACME options")
	}

	roots, err := loadTrustedRoots(opts.CARootFile)
	if err != nil {
		return nil, err
	}

	directoryURL := opts.DirectoryURL
	if directoryURL == "" {
		directoryURL = DefaultDirectoryURL
	}

	var cfg *certmagic.Config

	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return cfg, nil
		},
		RenewCheckInterval: opts.renewCheckInterval,
		Logger:             logger,
	})

	cfg = certmagic.New(cache, certmagic.Config{
		Storage:            &certmagic.FileStorage{Path: opts.StorageDir},
		DefaultServerName:  opts.Domains[0],
		RenewalWindowRatio: opts.renewalWindowRatio,
		Logger:             logger,
		// private CAs such as step-ca usually do not run an OCSP responder.
		OCSP: certmagic.OCSPConfig{DisableStapling: true},
	})

	cfg.Issuers = []certmagic.Issuer{
		certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
			CA:     directoryURL,
			Email:  opts.Email,
			Agreed: true,
			// Only TLS-ALPN-01 is used: port 80 is commonly owned by another web server.
			DisableHTTPChallenge: true,
			AltTLSALPNPort:       opts.AltTLSALPNPort,
			TrustedRoots:         roots,
			Logger:               logger,
		}),
	}

	for _, domain := range opts.Domains {
		if err := loadOrObtain(ctx, cfg, domain, logger); err != nil {
			cache.Stop()

			return nil, err
		}
	}

	return &Manager{cache: cache, cfg: cfg}, nil
}

// loadOrObtain makes sure a valid certificate for domain is in the cache, where
// the cache's maintenance loop keeps renewing it.
//
// certmagic's ManageSync is not used because in synchronous mode it prompts on
// stdin for an ACME account email when none is configured. The *Async
// obtain/renew calls used here block, never prompt, and retry with backoff
// until they succeed or ctx is canceled.
func loadOrObtain(ctx context.Context, cfg *certmagic.Config, domain string, logger *zap.Logger) error {
	cert, err := cfg.CacheManagedCertificate(ctx, domain)
	if errors.Is(err, fs.ErrNotExist) {
		if err := cfg.ObtainCertAsync(ctx, domain); err != nil {
			return errors.Wrapf(err, "unable to obtain TLS certificate for %v", domain)
		}

		_, err = cfg.CacheManagedCertificate(ctx, domain)

		return errors.Wrapf(err, "unable to load TLS certificate for %v", domain)
	}

	if err != nil {
		return errors.Wrapf(err, "unable to load TLS certificate for %v", domain)
	}

	// a certificate that is due for renewal but still valid is renewed by the
	// cache's maintenance loop, so that an unreachable CA does not delay startup.
	if !cert.Expired() {
		return nil
	}

	logger.Warn("stored TLS certificate has expired, renewing", zap.String("domain", domain))

	if err := cfg.RenewCertAsync(ctx, domain, false); err != nil {
		return errors.Wrapf(err, "unable to renew expired TLS certificate for %v", domain)
	}

	_, err = cfg.CacheManagedCertificate(ctx, domain)

	return errors.Wrapf(err, "unable to load renewed TLS certificate for %v", domain)
}

// TLSConfig returns a server TLS configuration that serves the managed
// certificate and answers TLS-ALPN-01 challenges on the server's own listener.
func (m *Manager) TLSConfig() *tls.Config {
	c := m.cfg.TLSConfig()
	c.MinVersion = tls.VersionTLS12
	// h2 must come before http/1.1 so gRPC clients negotiate HTTP/2.
	c.NextProtos = append([]string{"h2", "http/1.1"}, c.NextProtos...)

	return c
}

// Close stops background renewal. It is safe to call more than once.
func (m *Manager) Close() {
	m.closeOnce.Do(m.cache.Stop)
}

func loadTrustedRoots(fname string) (*x509.CertPool, error) {
	if fname == "" {
		return nil, nil //nolint:nilnil
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}

	b, err := os.ReadFile(fname) //nolint:gosec
	if err != nil {
		return nil, errors.Wrap(err, "unable to read ACME CA root file")
	}

	if !pool.AppendCertsFromPEM(b) {
		return nil, errors.Errorf("no PEM certificates found in %v", fname)
	}

	return pool, nil
}
