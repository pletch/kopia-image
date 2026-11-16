package recoverymedia

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image"
)

const (
	// RecordFileName is the record of the last media built on a computer,
	// kept in the image state folder.
	RecordFileName = "recovery-media.json"

	// MaxMediaAge is the age after which media is reported as old.
	MaxMediaAge = 180 * 24 * time.Hour
)

// Record describes the last recovery media built on a computer, to tell
// when the media no longer matches the computer.
type Record struct {
	BuiltAt      time.Time `json:"builtAt"`
	KopiaVersion string    `json:"kopiaVersion"`

	// Target is where the media was written.
	Target string `json:"target"`

	WinRE       string `json:"winre"`
	WinREDigest string `json:"winreDigest"`

	// ConnectFrom is the repository connection file the media embeds, when
	// it is not the one kopia uses.
	ConnectFrom string `json:"connectFrom,omitempty"`

	// ConnectionDigest identifies the embedded repository connection.
	ConnectionDigest string `json:"connectionDigest"`

	CACerts []string `json:"caCerts,omitempty"`

	// Disks maps the image sources in the kit to a digest of their layout.
	Disks map[string]string `json:"disks"`

	PasswordEmbedded bool `json:"passwordEmbedded,omitempty"`
}

// ReadRecord reads the record of the last media built. It returns an error
// wrapping os.ErrNotExist when no media was built.
func ReadRecord(path string) (*Record, error) {
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, errors.Wrap(err, "unable to read the recovery media record")
	}

	var r Record

	return &r, errors.Wrapf(json.Unmarshal(b, &r), "invalid recovery media record %v", path)
}

// WriteRecord replaces the record of the last media built.
func WriteRecord(path string, r *Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errors.Wrap(err, "unable to encode the recovery media record")
	}

	tmp := path + ".tmp"

	if err := os.WriteFile(tmp, b, 0o600); err != nil { //nolint:mnd
		return errors.Wrap(err, "unable to write the recovery media record")
	}

	return errors.Wrap(os.Rename(tmp, filepath.Clean(path)), "unable to save the recovery media record")
}

// ConnectionDigest identifies where a connection leads and how it trusts
// the server, ignoring caching and client options.
func ConnectionDigest(lc *repo.LocalConfig) string {
	b, err := json.Marshal(struct {
		APIServer *repo.APIServerInfo `json:"apiServer,omitempty"`
		Storage   any                 `json:"storage,omitempty"`
	}{lc.APIServer, lc.Storage})
	if err != nil {
		return ""
	}

	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// LayoutDigest identifies a disk's partition table.
func LayoutDigest(l *image.Layout) string {
	b, err := json.Marshal(l)
	if err != nil {
		return ""
	}

	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// Current is the state of the computer that media is compared with.
type Current struct {
	Now          time.Time
	KopiaVersion string

	// WinREDigest is the SHA-256 of the computer's Winre.wim, or "" when
	// not known.
	WinREDigest string

	// Connection is the repository connection the media should embed.
	Connection *repo.LocalConfig

	// Layouts are the disks being imaged, by image source.
	Layouts map[string]*image.Layout
}

// Stale returns why media built as recorded no longer matches the
// computer, or nothing when it does. Server checks need the network.
func (r *Record) Stale(ctx context.Context, cur *Current) []string {
	var out []string

	if cur.KopiaVersion != r.KopiaVersion {
		out = append(out, fmt.Sprintf("kopia is now %v; the media has %v", cur.KopiaVersion, r.KopiaVersion))
	}

	if cur.WinREDigest != "" && cur.WinREDigest != r.WinREDigest {
		out = append(out, "Windows updated its recovery environment, which the media is built from; old media may stop booting with Secure Boot on")
	}

	if cur.Connection != nil {
		if ConnectionDigest(cur.Connection) != r.ConnectionDigest {
			out = append(out, "the repository connection changed")
		}

		if err := CheckServerTrust(ctx, cur.Connection, r.CACerts); err != nil {
			out = append(out, err.Error())
		}
	}

	sources := make([]string, 0, len(cur.Layouts))
	for s := range cur.Layouts {
		sources = append(sources, s)
	}

	slices.Sort(sources)

	for _, s := range sources {
		switch digest, ok := r.Disks[s]; {
		case !ok:
			out = append(out, "the recovery kit does not describe the disk "+s)
		case digest != LayoutDigest(cur.Layouts[s]):
			out = append(out, "the partitions of "+s+" changed")
		}
	}

	if age := cur.Now.Sub(r.BuiltAt); age > MaxMediaAge {
		out = append(out, fmt.Sprintf("the media is %v days old", int(age.Hours()/24))) //nolint:mnd
	}

	return out
}

// CheckServerTrust checks that a connection's server is still trusted the
// way the media trusts it: by its pinned certificate fingerprint, or by
// the media's certificate authorities. Endpoints that the media trusts with
// the system's authorities, or that are not reached over TLS, pass.
func CheckServerTrust(ctx context.Context, lc *repo.LocalConfig, caCerts []string) error {
	ep, err := RepositoryEndpoint(lc)
	if err != nil || ep == nil || ep.HasOwnCA || !strings.HasPrefix(ep.URL, "https://") {
		return nil //nolint:nilerr
	}

	if !ep.Pinned && len(caCerts) == 0 {
		return nil
	}

	chain, err := peerCertificates(ctx, ep.URL)
	if err != nil {
		return errors.Wrap(err, "unable to check the server certificate")
	}

	if ep.Pinned {
		want := ""
		if lc.APIServer != nil {
			want = lc.APIServer.TrustedServerCertificateFingerprint
		}

		sum := sha256.Sum256(chain[0].Raw)
		if want != "" && !strings.EqualFold(hex.EncodeToString(sum[:]), want) {
			return errors.Errorf("the server certificate of %v changed; the media pins the old one", ep.URL)
		}

		return nil
	}

	roots := x509.NewCertPool()

	for _, p := range caCerts {
		roots.AppendCertsFromPEM([]byte(p))
	}

	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}

	u, _ := url.Parse(ep.URL)

	if _, err := chain[0].Verify(x509.VerifyOptions{DNSName: u.Hostname(), Roots: roots, Intermediates: inter}); err != nil {
		return errors.Errorf("the certificate authorities on the media no longer validate %v: %v", ep.URL, err)
	}

	return nil
}

// peerCertificates returns the certificates an https server presents,
// without verifying them.
func peerCertificates(ctx context.Context, endpoint string) ([]*x509.Certificate, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid endpoint %v", endpoint)
	}

	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}

	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout},
		Config:    &tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // verified by the caller
	}

	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to connect to %v", u.Host)
	}
	defer conn.Close() //nolint:errcheck

	tc, ok := conn.(*tls.Conn)
	if !ok {
		return nil, errors.New("not a TLS connection")
	}

	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.Errorf("%v presented no certificate", u.Host)
	}

	return certs, nil
}
