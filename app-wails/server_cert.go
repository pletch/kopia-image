package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	serverCertFileName = "server.crt"
	serverKeyFileName  = "server.key"
	serverCertValidity = 10 * 365 * 24 * time.Hour
	serverCertSerial   = 128 // bits
)

// serverCert is the TLS certificate one kopia server process is started with.
//
// The shell makes the certificate, rather than letting the server generate
// one and print its fingerprint. What the server prints arrives on stderr
// between log messages, and a log message can carry text from elsewhere, such
// as a file name. A fingerprint read from there could be one that someone
// else chose, and the proxy would then send them the UI password. A
// fingerprint the shell computed itself cannot be forged that way.
type serverCert struct {
	dir         string // holds the two files below and nothing else
	certFile    string
	keyFile     string
	fingerprint string // SHA256 of the certificate, in lower-case hex
}

// newServerCert generates a certificate for 127.0.0.1 and writes it and its
// key to a new directory that only the current user can open.
func newServerCert() (c *serverCert, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("unable to generate a TLS key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serverCertSerial))
	if err != nil {
		return nil, fmt.Errorf("unable to generate a certificate serial number: %w", err)
	}

	now := time.Now()

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "KopiaUI"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(serverCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("unable to generate a TLS certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("unable to encode the TLS key: %w", err)
	}

	// private to the user on every platform: 0700 on Unix, and on Windows the
	// user's own temporary folder
	dir, err := os.MkdirTemp("", "kopia-ui-tls-")
	if err != nil {
		return nil, fmt.Errorf("unable to create a folder for the TLS certificate: %w", err)
	}

	sum := sha256.Sum256(der)

	c = &serverCert{
		dir:         dir,
		certFile:    filepath.Join(dir, serverCertFileName),
		keyFile:     filepath.Join(dir, serverKeyFileName),
		fingerprint: hex.EncodeToString(sum[:]),
	}

	defer func() {
		if err != nil {
			c.remove()
		}
	}()

	if err := writePEM(c.certFile, "CERTIFICATE", der); err != nil {
		return nil, err
	}

	if err := writePEM(c.keyFile, "PRIVATE KEY", keyDER); err != nil {
		return nil, err
	}

	return c, nil
}

func writePEM(path, blockType string, der []byte) error {
	b := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})

	if err := os.WriteFile(path, b, 0o600); err != nil { //nolint:mnd
		return fmt.Errorf("unable to write %v: %w", path, err)
	}

	return nil
}

// remove deletes the certificate and its key.
func (c *serverCert) remove() {
	_ = os.RemoveAll(c.dir)
}
