package recoverymedia

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image"
)

// KitDisk is an imaged disk described in the recovery kit.
type KitDisk struct {
	// Source is the image source, user@host:image:disk:<GUID>.
	Source string

	// LatestImage describes the newest image, e.g. its ID and time.
	LatestImage string

	Layout *image.Layout
}

// Kit is the content of the recovery kit: what someone needs to restore
// this computer, with or without the recovery media.
type Kit struct {
	Computer     string
	BuiltAt      time.Time
	KopiaVersion string

	// Connection is the repository connection, without the password.
	Connection *repo.LocalConfig

	// UserHost is the user@host the images belong to.
	UserHost string

	Disks []KitDisk

	// CACerts are the certificate authorities on the media, as PEM.
	CACerts []string

	PasswordEmbedded bool
}

// RenderKit renders the recovery kit as text, printable and readable on any
// computer.
func RenderKit(k *Kit) string {
	var b strings.Builder

	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\r\n", args...)
	}

	line("KOPIA RECOVERY KIT for %v", k.Computer)
	line("Made %v with kopia %v.", k.BuiltAt.Format("2006-01-02 15:04 MST"), k.KopiaVersion)
	line("Keep it with the recovery media, and a printed copy somewhere else.")
	line("")
	line("REPOSITORY")
	line("  %v", DescribeRepository(k.Connection))
	line("  Images belong to %v.", k.UserHost)

	for _, t := range trustLines(k) {
		line("  %v", t)
	}

	line("  The repository password is kept at: ______________________________")

	if k.PasswordEmbedded {
		line("  The recovery media holds the password: keep it as safe as the password itself.")
	}

	line("")
	line("DISKS")

	for _, d := range k.Disks {
		line("  %v", d.Source)

		if d.LatestImage != "" {
			line("    newest image when this kit was made: %v", d.LatestImage)
		}

		if d.Layout == nil {
			continue
		}

		line("    %v, %v-byte sectors, disk GUID %v", units.BytesString(d.Layout.DiskSize), d.Layout.SectorSize, d.Layout.DiskGUID)

		for _, p := range d.Layout.Partitions {
			line("    partition %v: %v, %v", p.Number, units.BytesString(p.Length), partitionKind(p))
		}
	}

	line("")
	line("RESTORE WITH THE RECOVERY MEDIA")
	line("  1. Boot the computer from the recovery media (a boot menu key such as F12, Esc or F8).")
	line("     Secure Boot can stay on. A wired network connection is needed.")
	line("  2. kopia starts by itself, waits for the network and checks the clock.")
	line("  3. Enter the repository password.")
	line("  4. Choose the image: the newest image of this computer is the default.")
	line("  5. Choose the disk to restore to; the media and disks that are too small are not offered.")
	line("  6. Type the disk's number to erase it and restore.")
	line("  7. Remove the media and restart.")
	line("")
	line("RESTORE WITHOUT THE MEDIA")
	line("  Boot a Linux live system, download kopia %v or newer from https://kopia.io, then:", k.KopiaVersion)
	line("    %v", ConnectCommand(k.Connection))
	line("    kopia image recover")
	line("  or, to restore one image directly (find the disk with lsblk):")

	if len(k.Disks) > 0 {
		line("    kopia image restore %v --target /dev/sdX", k.Disks[0].Source)
	}

	line("")
	line("AFTER RESTORING")
	line("  - Volumes that used BitLocker come back decrypted: turn BitLocker on again.")
	line("  - If the computer was compromised, change the repository password.")
	line("  - Build new recovery media when kopia, Windows or the disks change:")
	line("    kopia image recovery-media status")

	return b.String()
}

func partitionKind(p image.Partition) string {
	switch strings.ToLower(p.TypeGUID) {
	case image.PartitionTypeEFISystem:
		return "EFI system"
	case image.PartitionTypeMSR:
		return "Microsoft reserved"
	case image.PartitionTypeWinRecovery:
		return "Windows recovery"
	case image.PartitionTypeBasicData:
		if p.Name != "" {
			return "basic data \"" + p.Name + "\""
		}

		return "basic data"
	default:
		return "type " + p.TypeGUID
	}
}

func trustLines(k *Kit) []string {
	var out []string

	if s := k.Connection.APIServer; s != nil && s.TrustedServerCertificateFingerprint != "" {
		out = append(out, "Server certificate SHA-256: "+s.TrustedServerCertificateFingerprint)
	}

	for _, p := range k.CACerts {
		certs, err := ParseCertificates(p)
		if err != nil {
			continue
		}

		for _, c := range certs {
			sum := sha256.Sum256(c.Raw)
			out = append(out, fmt.Sprintf("Trusted CA %q, SHA-256 %v, valid until %v", c.Subject.CommonName, hex.EncodeToString(sum[:]), c.NotAfter.Format("2006-01-02")))
		}
	}

	return out
}

// ConnectCommand returns the "kopia repository connect" command for a
// connection, with placeholders in place of secrets.
func ConnectCommand(lc *repo.LocalConfig) string {
	if s := lc.APIServer; s != nil {
		cmd := "kopia repository connect server --url " + quoteArg(s.BaseURL)

		switch {
		case s.TrustedServerCertificateFingerprint != "":
			cmd += " --server-cert-fingerprint " + s.TrustedServerCertificateFingerprint
		case len(s.RootCA) > 0:
			cmd += " --server-ca-pem-path <CA FILE>"
		}

		return cmd + " --readonly"
	}

	if lc.Storage == nil {
		return "kopia repository connect ..."
	}

	spec, ok := connectSpecs[lc.Storage.Type]
	if !ok {
		return "kopia repository connect " + lc.Storage.Type + " ... --readonly"
	}

	cfg, err := storageConfig(lc)
	if err != nil {
		return "kopia repository connect " + spec.command + " ... --readonly"
	}

	args := []string{"kopia", "repository", "connect", spec.command}

	for _, f := range spec.flags {
		args = append(args, connectFlag(f[0], cfg[f[1]])...)
	}

	for _, s := range spec.secrets {
		args = append(args, "--"+s[0], "<"+s[1]+">")
	}

	return strings.Join(append(args, "--readonly"), " ")
}

// connectSpec says how a storage type's options map to connect flags.
type connectSpec struct {
	command string

	// flags are pairs of a flag name and its storage option key.
	flags [][2]string

	// secrets are pairs of a flag name and the placeholder shown for it.
	secrets [][2]string
}

//nolint:goconst // flag names and storage option keys that happen to match
var connectSpecs = map[string]connectSpec{
	storageFilesystem: {command: "filesystem", flags: [][2]string{{"path", "path"}}},
	storageS3: {
		command: "s3",
		flags:   [][2]string{{"bucket", "bucket"}, {"prefix", "prefix"}, {"endpoint", "endpoint"}, {"region", "region"}, {"disable-tls", "doNotUseTLS"}},
		secrets: [][2]string{{"access-key", "ACCESS KEY"}, {"secret-access-key", "SECRET KEY"}},
	},
	storageB2: {
		command: "b2",
		flags:   [][2]string{{"bucket", "bucket"}, {"prefix", "prefix"}},
		secrets: [][2]string{{"key-id", "KEY ID"}, {"key", "KEY"}},
	},
	storageGCS: {
		command: "gcs",
		flags:   [][2]string{{"bucket", "bucket"}, {"prefix", "prefix"}},
		secrets: [][2]string{{"credentials-file", "CREDENTIALS FILE"}},
	},
	storageAzure: {
		command: "azure",
		flags:   [][2]string{{"container", "container"}, {"prefix", "prefix"}, {"storage-account", "storageAccount"}, {"storage-domain", "storageDomain"}},
		secrets: [][2]string{{"storage-key", "STORAGE KEY"}},
	},
	storageWebDAV: {
		command: "webdav",
		flags:   [][2]string{{"url", "url"}, {"webdav-username", "username"}},
		secrets: [][2]string{{"webdav-password", "PASSWORD"}},
	},
	storageSFTP: {
		command: "sftp",
		flags:   [][2]string{{"host", "host"}, {"port", "port"}, {"username", "username"}, {"path", "path"}},
		secrets: [][2]string{{"keyfile", "KEY FILE"}},
	},
}

// connectFlag returns the arguments for one storage option, none when unset.
func connectFlag(name string, v any) []string {
	switch v := v.(type) {
	case nil:
		return nil
	case bool:
		if v {
			return []string{"--" + name}
		}

		return nil
	case float64:
		return []string{"--" + name, strconv.FormatFloat(v, 'f', -1, 64)}
	case string:
		if v == "" {
			return nil
		}

		return []string{"--" + name, quoteArg(v)}
	default:
		return []string{"--" + name, quoteArg(fmt.Sprint(v))}
	}
}

func quoteArg(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\"'&|<>()^%!") {
		return strconv.Quote(s)
	}

	return s
}
