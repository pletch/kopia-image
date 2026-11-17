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
	"github.com/kopia/kopia/snapshot/image/schedule"
)

const (
	// RecordFileName is the record of the last media built on a computer,
	// kept in the image state folder.
	RecordFileName = "recovery-media.json"

	// MediaRecordFile is the copy of the record at the root of the media,
	// which "kopia image recovery-media register" reads to use the media for
	// other computers. It holds no secrets.
	MediaRecordFile = "kopia-recovery-media.json"

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

	// BuiltOn is the computer that built the media. It is set in the copy on
	// the media and when a computer registers media built on another one;
	// the builder's own record leaves it empty.
	BuiltOn string `json:"builtOn,omitempty"`

	// Repository describes the repository the media leads to (see
	// DescribeRepository); for registered media, as this computer reached it
	// when registering.
	Repository string `json:"repository,omitempty"`

	// Drivers are the driver packages the media holds, besides the ones
	// added from folders. Nil in records of media built before kopia
	// recorded them.
	Drivers []image.Driver `json:"drivers"`
}

// Shared tells whether the record is of media built on another computer.
func (r *Record) Shared() bool {
	return r.BuiltOn != ""
}

// ForMedia returns the copy of the record to put on the media built on
// computer: without the builder's local paths and disks.
func (r *Record) ForMedia(computer string) *Record {
	c := *r
	c.BuiltOn = computer
	c.Target = ""
	c.WinRE = ""
	c.ConnectFrom = ""
	c.Disks = nil

	return &c
}

// DefaultRecordPath returns where "kopia image recovery-media create"
// records the media: in ProgramData\kopia, which every user can read, so
// that the web UI, whose server runs as the signed-in user, can show it.
func DefaultRecordPath() (string, error) {
	dir, err := schedule.StateDir()
	if err != nil {
		return "", err //nolint:wrapcheck
	}

	return filepath.Join(dir, RecordFileName), nil
}

// legacyRecordPath is where kopia recorded media before: in
// ProgramData\kopia\image, which only administrators can open.
func legacyRecordPath() (string, error) {
	dir, err := schedule.DefaultConfigDir()
	if err != nil {
		return "", err //nolint:wrapcheck
	}

	return filepath.Join(dir, RecordFileName), nil
}

// ReadDefaultRecord reads the record at DefaultRecordPath, or, when there is
// none, the one an earlier kopia left in ProgramData\kopia\image. It returns
// an error wrapping os.ErrNotExist when no media was built.
func ReadDefaultRecord() (*Record, error) {
	p, err := DefaultRecordPath()
	if err != nil {
		return nil, err
	}

	r, err := ReadRecord(p)
	if !errors.Is(err, os.ErrNotExist) {
		return r, err
	}

	legacy, lerr := legacyRecordPath()
	if lerr != nil {
		return nil, err
	}

	return ReadRecord(legacy)
}

// RemoveLegacyRecord removes the record an earlier kopia left in
// ProgramData\kopia\image, once a new one is written at DefaultRecordPath.
func RemoveLegacyRecord() error {
	legacy, err := legacyRecordPath()
	if err != nil {
		return err
	}

	if err := os.Remove(legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Wrap(err, "unable to remove the previous recovery media record")
	}

	return nil
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

	// Drivers are the computer's storage, network and USB drivers, or nil
	// when not known.
	Drivers []image.Driver
}

// Stale returns why media built as recorded no longer matches the
// computer, or nothing when it does. Server checks need the network.
//
// Media built on another computer is not checked against this computer's
// recovery environment, which it is not built from, nor against its disks,
// which its recovery kit does not describe; its repository is compared by
// where it leads rather than by the exact connection, which differs from
// computer to computer.
func (r *Record) Stale(ctx context.Context, cur *Current) []string {
	var out []string

	if cur.KopiaVersion != r.KopiaVersion {
		out = append(out, fmt.Sprintf("kopia is now %v; the media has %v", cur.KopiaVersion, r.KopiaVersion))
	}

	if !r.Shared() && cur.WinREDigest != "" && cur.WinREDigest != r.WinREDigest {
		out = append(out, "Windows updated its recovery environment, which the media is built from; old media may stop booting with Secure Boot on")
	}

	if cur.Connection != nil {
		if r.Shared() {
			if r.Repository != "" && DescribeRepository(cur.Connection) != r.Repository {
				out = append(out, "the repository connection changed")
			}
		} else if ConnectionDigest(cur.Connection) != r.ConnectionDigest {
			out = append(out, "the repository connection changed")
		}

		if err := CheckServerTrust(ctx, cur.Connection, r.CACerts); err != nil {
			out = append(out, err.Error())
		}
	}

	out = append(out, r.driverFindings(cur.Drivers)...)

	if r.Shared() {
		return append(out, r.ageFinding(cur.Now)...)
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

	return append(out, r.ageFinding(cur.Now)...)
}

// driverFindings reports the computer's drivers that the media lacks, when
// both are known.
func (r *Record) driverFindings(have []image.Driver) []string {
	if r.Drivers == nil || have == nil {
		return nil
	}

	missing := MissingDrivers(r.Drivers, have)
	if len(missing) == 0 {
		return nil
	}

	names := make([]string, 0, len(missing))
	for _, d := range missing {
		names = append(names, d.String())
	}

	return []string{"the media lacks drivers of this computer, which it may need to reach the disks or the network: " + strings.Join(names, ", ")}
}

func (r *Record) ageFinding(now time.Time) []string {
	if age := now.Sub(r.BuiltAt); age > MaxMediaAge {
		return []string{fmt.Sprintf("the media is %v days old", int(age.Hours()/24))} //nolint:mnd
	}

	return nil
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
