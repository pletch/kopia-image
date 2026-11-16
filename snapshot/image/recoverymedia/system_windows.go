package recoverymedia

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"time"
	"unsafe"

	pkgerrors "github.com/pkg/errors"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/snapshot/image/imagerestore"
)

var procSetSystemTime = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetSystemTime")

// TrustCertificates adds certificate authorities to the trusted roots of the
// recovery system, where they last until it restarts. On an installed
// Windows, whose root store would keep them, it refuses.
func TrustCertificates(pemCerts []string) error {
	if len(pemCerts) == 0 {
		return nil
	}

	if !imagerestore.InRecoveryEnvironment() {
		return pkgerrors.New("certificate authorities are installed only in the recovery environment; add them to this computer's trusted roots yourself")
	}

	root, err := windows.UTF16PtrFromString("ROOT")
	if err != nil {
		return pkgerrors.Wrap(err, "invalid store name")
	}

	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, uintptr(unsafe.Pointer(root))) //nolint:gosec // Windows API argument
	if err != nil {
		return pkgerrors.Wrap(err, "unable to open the trusted root store")
	}
	defer windows.CertCloseStore(store, 0) //nolint:errcheck

	var errs error

	for _, p := range pemCerts {
		certs, err := ParseCertificates(p)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}

		for _, c := range certs {
			errs = errors.Join(errs, addCertificate(store, c.Raw))
		}
	}

	return errs
}

func addCertificate(store windows.Handle, der []byte) error {
	ctx, err := windows.CertCreateCertificateContext(windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, &der[0], uint32(len(der))) //nolint:gosec
	if err != nil {
		return pkgerrors.Wrap(err, "unable to decode a certificate")
	}
	defer windows.CertFreeCertificateContext(ctx) //nolint:errcheck

	return pkgerrors.Wrap(windows.CertAddCertificateContextToStore(store, ctx, windows.CERT_STORE_ADD_REPLACE_EXISTING, nil), "unable to add a certificate to the trusted roots")
}

// SetSystemClock sets the computer's clock.
func SetSystemClock(t time.Time) error {
	u := t.UTC()
	st := windows.Systemtime{
		Year:         uint16(u.Year()),                               //nolint:gosec
		Month:        uint16(u.Month()),                              //nolint:gosec
		DayOfWeek:    uint16(u.Weekday()),                            //nolint:gosec
		Day:          uint16(u.Day()),                                //nolint:gosec
		Hour:         uint16(u.Hour()),                               //nolint:gosec
		Minute:       uint16(u.Minute()),                             //nolint:gosec
		Second:       uint16(u.Second()),                             //nolint:gosec
		Milliseconds: uint16(u.Nanosecond() / int(time.Millisecond)), //nolint:gosec
	}

	if r, _, err := procSetSystemTime.Call(uintptr(unsafe.Pointer(&st))); r == 0 { //nolint:gosec // Windows API argument
		return pkgerrors.Wrap(err, "unable to set the clock")
	}

	return nil
}

// PowerAction restarts ("reboot") or turns off ("shutdown") the recovery
// system.
func PowerAction(ctx context.Context, action string) error {
	if !imagerestore.InRecoveryEnvironment() {
		return pkgerrors.Errorf("%v is done only in the recovery environment", action)
	}

	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return pkgerrors.Wrap(err, "unable to find the Windows system folder")
	}

	return pkgerrors.Wrapf(exec.CommandContext(ctx, filepath.Join(system32, "wpeutil.exe"), action).Run(), "wpeutil %v", action) //nolint:gosec
}
