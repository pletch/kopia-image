//go:build !windows

package recoverymedia

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pkg/errors"
)

// TrustCertificates makes this process trust certificate authorities in
// addition to the system's, by pointing SSL_CERT_DIR at a folder holding
// them. It must run before the first TLS connection.
func TrustCertificates(pemCerts []string) error {
	if len(pemCerts) == 0 {
		return nil
	}

	dir, err := os.MkdirTemp("", "kopia-recovery-ca-")
	if err != nil {
		return errors.Wrap(err, "unable to create a folder for the certificate authorities")
	}

	for i, p := range pemCerts {
		if _, err := ParseCertificates(p); err != nil {
			return err
		}

		if err := os.WriteFile(filepath.Join(dir, "kopia-"+strconv.Itoa(i)+".pem"), []byte(p), 0o600); err != nil { //nolint:mnd
			return errors.Wrap(err, "unable to write a certificate authority")
		}
	}

	dirs := []string{dir}
	if old := os.Getenv("SSL_CERT_DIR"); old != "" {
		dirs = append(dirs, filepath.SplitList(old)...)
	} else {
		dirs = append(dirs, "/etc/ssl/certs", "/etc/pki/tls/certs", "/system/etc/security/cacerts")
	}

	return errors.Wrap(os.Setenv("SSL_CERT_DIR", joinList(dirs)), "unable to set SSL_CERT_DIR")
}

func joinList(dirs []string) string {
	out := ""

	for i, d := range dirs {
		if i > 0 {
			out += string(os.PathListSeparator)
		}

		out += d
	}

	return out
}

// SetSystemClock is not supported here: set the clock with "date".
func SetSystemClock(time.Time) error {
	return errors.New("set the clock with 'date -s'")
}

// PowerAction is supported only in the Windows recovery environment.
func PowerAction(_ context.Context, action string) error {
	return errors.Errorf("%v the computer yourself", action)
}
