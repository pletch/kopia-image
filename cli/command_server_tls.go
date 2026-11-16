package cli

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/acmecert"
	"github.com/kopia/kopia/internal/certreload"
	"github.com/kopia/kopia/internal/insecureserverbind"
	"github.com/kopia/kopia/internal/tlsutil"
)

const oneDay = 24 * time.Hour

const (
	acmeDNSProviderCloudflare = "cloudflare"

	// acmeCloudflareTokenEnv deliberately has no command-line flag equivalent:
	// arguments are visible to every user of the machine in the process list.
	acmeCloudflareTokenEnv = "KOPIA_ACME_CLOUDFLARE_API_TOKEN" //nolint:gosec // the name of the variable, not a credential

	// permission bits that give users other than the owner access to a file.
	groupAndOthersPermissions = 0o077
)

func (c *commandServerStart) generateServerCertificate(ctx context.Context) (*x509.Certificate, *rsa.PrivateKey, error) {
	cert, key, err := tlsutil.GenerateServerCertificate(
		ctx,
		c.serverStartTLSGenerateRSAKeySize,
		time.Duration(c.serverStartTLSGenerateCertValidDays)*oneDay,
		c.serverStartTLSGenerateCertNames,
	)

	return cert, key, errors.Wrap(err, "error generating server certificate")
}

func (c *commandServerStart) startServerWithOptionalTLS(ctx context.Context, httpServer *http.Server) error {
	var l net.Listener

	var err error

	listeners, err := activation.Listeners()
	if err != nil {
		return errors.Wrap(err, "socket-activation error")
	}

	switch len(listeners) {
	case 0:
		if after, ok := strings.CutPrefix(httpServer.Addr, "unix:"); ok {
			l, err = (&net.ListenConfig{}).Listen(ctx, "unix", after)
		} else {
			l, err = (&net.ListenConfig{}).Listen(ctx, "tcp", httpServer.Addr)
		}

		if err != nil {
			return errors.Wrap(err, "listen error")
		}
	case 1:
		l = listeners[0]
	default:
		return errors.Errorf("Too many activated sockets found.  Expected 1, got %v", len(listeners))
	}

	if err := insecureserverbind.ValidateListenerAddrIfRestricted(
		c.serverStartInsecure,
		c.serverStartWithoutPassword,
		c.serverStartAllowDangerousUnauthenticatedNetwork,
		l.Addr(),
	); err != nil {
		l.Close() //nolint:errcheck

		return errors.Wrap(err, "insecure server bind validation")
	}

	defer l.Close() //nolint:errcheck

	httpServer.Addr = l.Addr().String()

	return c.startServerWithOptionalTLSAndListener(ctx, httpServer, l)
}

func (c *commandServerStart) maybeGenerateTLS(ctx context.Context) error {
	if !c.serverStartTLSGenerateCert || c.serverStartTLSCertFile == "" || c.serverStartTLSKeyFile == "" {
		return nil
	}

	if _, err := os.Stat(c.serverStartTLSCertFile); err == nil {
		return errors.Errorf("TLS cert file already exists: %q", c.serverStartTLSCertFile)
	}

	if _, err := os.Stat(c.serverStartTLSKeyFile); err == nil {
		return errors.Errorf("TLS key file already exists: %q", c.serverStartTLSKeyFile)
	}

	cert, key, err := c.generateServerCertificate(ctx)
	if err != nil {
		return errors.Wrap(err, "unable to generate server cert")
	}

	fingerprint := sha256.Sum256(cert.Raw)
	fmt.Fprintf(c.out.stderr(), "SERVER CERT SHA256: %v\n", hex.EncodeToString(fingerprint[:])) //nolint:errcheck

	log(ctx).Infof("writing TLS certificate to %v", c.serverStartTLSCertFile)

	if err := tlsutil.WriteCertificateToFile(c.serverStartTLSCertFile, cert); err != nil {
		return errors.Wrap(err, "unable to write private key")
	}

	log(ctx).Infof("writing TLS private key to %v", c.serverStartTLSKeyFile)

	if err := tlsutil.WritePrivateKeyToFile(c.serverStartTLSKeyFile, key); err != nil {
		return errors.Wrap(err, "unable to write private key")
	}

	return nil
}

func (c *commandServerStart) startServerWithOptionalTLSAndListener(ctx context.Context, httpServer *http.Server, listener net.Listener) error {
	if err := c.maybeGenerateTLS(ctx); err != nil {
		return err
	}

	udsPfx := ""
	if listener.Addr().Network() == "unix" {
		udsPfx = "unix+"
	}

	switch {
	case len(c.serverStartTLSACMEDomains) > 0:
		// ctx is canceled on termination, which interrupts waiting for the CA.
		m, err := acmecert.New(ctx, c.acmeOptions(), log(ctx).Desugar())
		if err != nil {
			if ctx.Err() != nil {
				log(ctx).Info("terminated while waiting for the TLS certificate")
				return nil
			}

			return errors.Wrap(err, "unable to set up ACME certificate management")
		}

		defer m.Close()

		httpServer.TLSConfig = m.TLSConfig()

		fmt.Fprintf(c.out.stderr(), "SERVER ADDRESS: %shttps://%v\n", udsPfx, httpServer.Addr) //nolint:errcheck
		c.showServerUIPrompt(ctx)

		return checkErrServerClosed(ctx, httpServer.ServeTLS(listener, "", ""), "error starting TLS server")

	case c.serverStartTLSCertFile != "" && c.serverStartTLSKeyFile != "":
		// PEM files provided, reloaded when they change on disk or on SIGHUP.
		reloader, err := certreload.New(ctx, c.serverStartTLSCertFile, c.serverStartTLSKeyFile)
		if err != nil {
			return errors.Wrap(err, "unable to load TLS certificate")
		}

		onExternalConfigReloadRequest(reloader.Reload)

		httpServer.TLSConfig = &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: reloader.GetCertificate,
		}

		fmt.Fprintf(c.out.stderr(), "SERVER ADDRESS: %shttps://%v\n", udsPfx, httpServer.Addr) //nolint:errcheck
		c.showServerUIPrompt(ctx)

		return checkErrServerClosed(ctx, httpServer.ServeTLS(listener, "", ""), "error starting TLS server")

	case c.serverStartTLSGenerateCert:
		// PEM files not provided, generate in-memory TLS cert/key but don't persist.
		cert, key, err := c.generateServerCertificate(ctx)
		if err != nil {
			return errors.Wrap(err, "unable to generate server cert")
		}

		httpServer.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{
				{
					Certificate: [][]byte{cert.Raw},
					PrivateKey:  key,
				},
			},
		}

		fingerprint := sha256.Sum256(cert.Raw)
		fmt.Fprintf(c.out.stderr(), "SERVER CERT SHA256: %v\n", hex.EncodeToString(fingerprint[:])) //nolint:errcheck

		if c.serverStartTLSPrintFullServerCert {
			// dump PEM-encoded server cert, only used by KopiaUI to securely connect.
			var b bytes.Buffer

			if err := pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
				return errors.Wrap(err, "Failed to write data")
			}

			fmt.Fprintf(c.out.stderr(), "SERVER CERTIFICATE: %v\n", base64.StdEncoding.EncodeToString(b.Bytes())) //nolint:errcheck
		}

		fmt.Fprintf(c.out.stderr(), "SERVER ADDRESS: %shttps://%v\n", udsPfx, httpServer.Addr) //nolint:errcheck
		c.showServerUIPrompt(ctx)

		return checkErrServerClosed(ctx, httpServer.ServeTLS(listener, "", ""), "error starting TLS server")

	default:
		if !c.serverStartInsecure {
			return errors.New("TLS not configured. To start server without encryption pass --insecure")
		}

		fmt.Fprintf(c.out.stderr(), "SERVER ADDRESS: %shttp://%v\n", udsPfx, httpServer.Addr) //nolint:errcheck
		c.showServerUIPrompt(ctx)

		return checkErrServerClosed(ctx, httpServer.Serve(listener), "error starting server")
	}
}

// validateTLSFlags rejects combinations of TLS flags that select more than one certificate source.
func (c *commandServerStart) validateTLSFlags(ctx context.Context) error {
	dnsProvider, err := c.acmeDNSProviderFromFlags(ctx)
	if err != nil {
		return err
	}

	c.acmeDNSProvider = dnsProvider

	if len(c.serverStartTLSACMEDomains) == 0 {
		if dnsProvider != nil {
			return errors.New("--tls-acme-dns-provider requires --tls-acme-domain")
		}

		return nil
	}

	switch {
	case c.serverStartTLSCertFile != "" || c.serverStartTLSKeyFile != "":
		return errors.New("--tls-acme-domain cannot be combined with --tls-cert-file or --tls-key-file")
	case c.serverStartTLSGenerateCert:
		return errors.New("--tls-acme-domain cannot be combined with --tls-generate-cert")
	case c.serverStartInsecure:
		return errors.New("--tls-acme-domain cannot be combined with --insecure")
	}

	return errors.Wrap(c.acmeOptions().Validate(), "invalid ACME flags")
}

func (c *commandServerStart) acmeOptions() acmecert.Options {
	storageDir := c.serverStartTLSACMEStorageDir
	if storageDir == "" {
		storageDir = filepath.Join(filepath.Dir(c.svc.repositoryConfigFileName()), "acme")
	}

	return acmecert.Options{
		Domains:        c.serverStartTLSACMEDomains,
		DirectoryURL:   c.serverStartTLSACMEDirectory,
		CARootFile:     c.serverStartTLSACMECARoot,
		Email:          c.serverStartTLSACMEEmail,
		StorageDir:     storageDir,
		AltTLSALPNPort: c.serverStartTLSACMEALPNPort,
		DNSProvider:    c.acmeDNSProvider,
		DNSResolvers:   c.serverStartTLSACMEDNSResolvers,
	}
}

// acmeDNSProviderFromFlags returns the DNS provider for the DNS-01 challenge,
// or nil when the challenge is not enabled.
//
// The API token is accepted from a file or from the environment. It is removed
// from the environment, so this must be called only once.
func (c *commandServerStart) acmeDNSProviderFromFlags(ctx context.Context) (acmecert.DNSProvider, error) {
	envName := c.svc.EnvName(acmeCloudflareTokenEnv)
	envToken := takeSecretFromEnv(envName)
	tokenFile := c.serverStartTLSACMECloudflareTokenFile

	if c.serverStartTLSACMEDNSProvider == "" {
		if tokenFile != "" {
			return nil, errors.New("--tls-acme-cloudflare-api-token-file requires --tls-acme-dns-provider=cloudflare")
		}

		if len(c.serverStartTLSACMEDNSResolvers) > 0 {
			return nil, errors.New("--tls-acme-dns-resolver requires --tls-acme-dns-provider")
		}

		return nil, nil //nolint:nilnil
	}

	var token acmecert.TokenSource

	switch {
	case tokenFile != "" && envToken != "":
		return nil, errors.Errorf("--tls-acme-cloudflare-api-token-file and %v are both set, use only one", envName)
	case tokenFile != "":
		warnIfReadableByOthers(ctx, tokenFile)

		token = acmecert.TokenFromFile(tokenFile)
	case envToken != "":
		token = acmecert.StaticToken(envToken)
	default:
		return nil, errors.Errorf("--tls-acme-dns-provider=cloudflare requires an API token in --tls-acme-cloudflare-api-token-file or %v", envName)
	}

	// report an unusable token now instead of at the first issuance or renewal.
	if _, err := token(); err != nil {
		return nil, errors.Wrap(err, "invalid ACME DNS provider API token")
	}

	return acmecert.NewCloudflareDNSProvider(token), nil
}

// takeSecretFromEnv returns the value of an environment variable and removes
// it from the environment of this process, because snapshot actions and other
// child processes inherit that environment.
func takeSecretFromEnv(name string) string {
	v := os.Getenv(name)

	// Unsetenv only fails for malformed names.
	os.Unsetenv(name) //nolint:errcheck

	return v
}

func warnIfReadableByOthers(ctx context.Context, secretFile string) {
	// Windows permissions are not expressed in the file mode.
	if isWindows() {
		return
	}

	st, err := os.Stat(secretFile)
	if err != nil {
		// reported with more context when the file is read.
		return
	}

	if st.Mode().Perm()&groupAndOthersPermissions != 0 {
		log(ctx).Warnf("%v is accessible by other users (mode %04o), restrict it to its owner, e.g. chmod 600", secretFile, st.Mode().Perm())
	}
}

func (c *commandServerStart) showServerUIPrompt(ctx context.Context) {
	if c.serverStartUI {
		log(ctx).Info("Open the address above in a web browser to use the UI.")
	}
}

func checkErrServerClosed(ctx context.Context, err error, msg string) error {
	if errors.Is(err, http.ErrServerClosed) {
		log(ctx).Debug("HTTP server closed:", err)

		return nil
	}

	return errors.Wrap(err, msg)
}
