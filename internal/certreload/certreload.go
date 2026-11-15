// Package certreload serves a TLS certificate from PEM files and picks up
// changes to those files without restarting the server.
package certreload

import (
	"context"
	"crypto/tls"
	stderrors "errors"
	"os"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo/logging"
)

var log = logging.Module("certreload")

// DefaultCheckInterval is how often the cert/key files are checked for changes.
const DefaultCheckInterval = 10 * time.Second

// fileStamp identifies a version of a file well enough to notice replacements.
type fileStamp struct {
	modTime time.Time
	size    int64
}

// Reloader loads a certificate/key pair from disk and reloads it when either
// file changes. A failed reload keeps the previously loaded certificate.
type Reloader struct {
	certFile      string
	keyFile       string
	checkInterval time.Duration
	now           func() time.Time
	logger        logging.Logger

	mu          sync.Mutex
	cert        *tls.Certificate
	certStamp   fileStamp
	keyStamp    fileStamp
	lastChecked time.Time
}

// New loads the certificate/key pair and returns a Reloader serving it.
// An invalid pair at startup is an error.
func New(ctx context.Context, certFile, keyFile string) (*Reloader, error) {
	r := &Reloader{
		certFile:      certFile,
		keyFile:       keyFile,
		checkInterval: DefaultCheckInterval,
		now:           clock.Now,
		logger:        log(ctx),
	}

	if err := r.load(); err != nil {
		return nil, err
	}

	return r, nil
}

// GetCertificate implements tls.Config.GetCertificate.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if now := r.now(); now.Sub(r.lastChecked) >= r.checkInterval {
		r.lastChecked = now
		r.reloadIfChangedLocked()
	}

	return r.cert, nil
}

// Reload forces the certificate to be re-read from disk, keeping the current
// one if the files cannot be loaded.
func (r *Reloader) Reload() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lastChecked = r.now()

	if err := r.loadLocked(); err != nil {
		r.logger.Warnf("unable to reload TLS certificate, keeping the current one: %v", err)
	}
}

func (r *Reloader) reloadIfChangedLocked() {
	certStamp, err1 := stat(r.certFile)
	keyStamp, err2 := stat(r.keyFile)

	if err := stderrors.Join(err1, err2); err != nil {
		r.logger.Warnf("unable to check TLS certificate files, keeping the current certificate: %v", err)
		return
	}

	if certStamp == r.certStamp && keyStamp == r.keyStamp {
		return
	}

	if err := r.loadLocked(); err != nil {
		r.logger.Warnf("unable to reload TLS certificate, keeping the current one: %v", err)
	}
}

func (r *Reloader) load() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.loadLocked()
}

func (r *Reloader) loadLocked() error {
	// stat before reading so that a write racing with the read is seen as a
	// change on the next check rather than missed.
	certStamp, err := stat(r.certFile)
	if err != nil {
		return err
	}

	keyStamp, err := stat(r.keyFile)
	if err != nil {
		return err
	}

	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		// remember the stamps anyway so a broken pair is not re-parsed on every check;
		// the next write to either file will trigger another attempt.
		r.certStamp, r.keyStamp = certStamp, keyStamp

		return errors.Wrap(err, "error loading TLS certificate/key pair")
	}

	if r.cert != nil {
		r.logger.Infof("reloaded TLS certificate from %v", r.certFile)
	}

	r.cert = &cert
	r.certStamp, r.keyStamp = certStamp, keyStamp

	return nil
}

func stat(fname string) (fileStamp, error) {
	st, err := os.Stat(fname)
	if err != nil {
		return fileStamp{}, errors.Wrap(err, "unable to stat file")
	}

	return fileStamp{modTime: st.ModTime(), size: st.Size()}, nil
}
