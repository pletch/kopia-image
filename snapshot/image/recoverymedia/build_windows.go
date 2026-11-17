package recoverymedia

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	pkgerrors "github.com/pkg/errors"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/kopia/kopia/snapshot/image"
)

// IsElevated reports whether the process runs as an administrator.
func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// Build makes the media's files from a copy of the WinRE image: it adds
// kopia, the repository connection, the start script, the time zone and
// drivers to the image, and lays the image out with its boot files the way
// UEFI firmware boots it from a USB stick. The original WinRE image is not
// changed.
func Build(ctx context.Context, opt *BuildOptions) (res *BuildResult, err error) {
	logf := opt.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	w := &workspace{dir: opt.WorkDir, logf: logf}
	w.cleanup(ctx)

	for _, d := range []string{w.mountDir(), w.mediaDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil { //nolint:mnd
			return nil, pkgerrors.Wrapf(err, "unable to create %v", d)
		}
	}

	res = &BuildResult{MediaDir: w.mediaDir(), WinRE: opt.WinRE, TimeZone: opt.TimeZone}

	if res.WinRE == "" {
		if res.WinRE, err = LocateWinRE(ctx); err != nil {
			return nil, err
		}
	}

	if res.TimeZone == "" {
		res.TimeZone = hostTimeZone()
	}

	logf("Copying %v\n", res.WinRE)

	if err := w.copyWinRE(res); err != nil {
		return nil, err
	}

	logf("Mounting the recovery image\n")

	if err := dism(ctx, "/Mount-Image", "/ImageFile:"+w.wimPath(), "/Index:1", "/MountDir:"+w.mountDir()); err != nil {
		return nil, err
	}

	committed := false

	defer func() {
		if !committed {
			err = errors.Join(err, dism(context.WithoutCancel(ctx), "/Unmount-Image", "/MountDir:"+w.mountDir(), "/Discard"))
		}
	}()

	if err := w.customize(ctx, opt, res); err != nil {
		return nil, err
	}

	logf("Saving the recovery image\n")

	if err := dism(ctx, "/Unmount-Image", "/MountDir:"+w.mountDir(), "/Commit"); err != nil {
		return nil, err
	}

	committed = true

	if err := w.finishMedia(opt, res); err != nil {
		return nil, err
	}

	return res, nil
}

// workspace is the folder a build works in.
type workspace struct {
	dir  string
	logf func(string, ...any)
}

func (w *workspace) mountDir() string { return filepath.Join(w.dir, "mount") }
func (w *workspace) mediaDir() string { return filepath.Join(w.dir, "media") }
func (w *workspace) wimPath() string  { return filepath.Join(w.dir, "boot.wim") }

// cleanup discards what a crashed build left behind.
func (w *workspace) cleanup(ctx context.Context) {
	if _, err := os.Stat(w.mountDir()); err == nil {
		// fails harmlessly when nothing is mounted
		_ = dism(ctx, "/Unmount-Image", "/MountDir:"+w.mountDir(), "/Discard")
	}

	for _, p := range []string{w.mountDir(), w.mediaDir(), w.wimPath(), filepath.Join(w.dir, "drivers")} {
		if err := os.RemoveAll(p); err != nil {
			w.logf("unable to remove %v: %v\n", p, err)
		}
	}
}

func (w *workspace) copyWinRE(res *BuildResult) error {
	if err := CopyFile(res.WinRE, w.wimPath()); err != nil {
		return err
	}

	digest, err := FileDigest(w.wimPath())
	if err != nil {
		return err
	}

	res.WinREDigest = hex.EncodeToString(digest)

	return nil
}

// customize changes the mounted image.
func (w *workspace) customize(ctx context.Context, opt *BuildOptions, res *BuildResult) error {
	mnt := w.mountDir()
	kdir := filepath.Join(mnt, "kopia")

	if err := os.MkdirAll(kdir, 0o700); err != nil { //nolint:mnd
		return pkgerrors.Wrap(err, "unable to create the kopia folder in the image")
	}

	if err := CopyFile(opt.KopiaExe, filepath.Join(kdir, "kopia.exe")); err != nil {
		return err
	}

	files := map[string][]byte{
		filepath.Join(kdir, "start.cmd"):                          []byte(StartScript()),
		filepath.Join(kdir, ConfigFileName):                       opt.Config,
		filepath.Join(mnt, "Windows", "System32", "winpeshl.ini"): []byte(WinpeshlINI()),
	}

	for p, b := range files {
		if err := os.WriteFile(p, b, 0o600); err != nil { //nolint:mnd
			return pkgerrors.Wrapf(err, "unable to write %v", p)
		}
	}

	if err := WriteInfo(filepath.Join(kdir, InfoFileName), opt.Info); err != nil {
		return err
	}

	w.logf("Setting the time zone to %v\n", res.TimeZone)

	if err := dism(ctx, "/Image:"+mnt, "/Set-TimeZone:"+res.TimeZone); err != nil {
		return err
	}

	if err := w.addDrivers(ctx, opt, res); err != nil {
		return err
	}

	return w.copyBootFiles(res)
}

func (w *workspace) addDrivers(ctx context.Context, opt *BuildOptions, res *BuildResult) error {
	var pkgs []image.DriverSource

	if opt.HostDrivers {
		exported, failures, err := ExportHostDrivers(ctx, filepath.Join(w.dir, "drivers"))
		if err != nil {
			return err
		}

		pkgs = exported

		res.DriverFailures = append(res.DriverFailures, failures...)
	}

	pkgs = NewestPackages(append(pkgs, opt.RepositoryDrivers...))
	total := len(pkgs) + len(opt.ExtraDrivers)

	add := func(i int, name, path string) bool {
		w.logf("Adding driver %v/%v: %v\n", i+1, total, name)

		if err := dism(ctx, "/Image:"+w.mountDir(), "/Add-Driver", "/Driver:"+path, "/Recurse"); err != nil {
			res.DriverFailures = append(res.DriverFailures, name+": "+err.Error())
			return false
		}

		res.Drivers = append(res.Drivers, name)

		return true
	}

	res.DriverPackages = []image.Driver{}

	for i, p := range pkgs {
		if add(i, p.String(), p.Dir) {
			d := p.Driver
			d.Entry = ""
			res.DriverPackages = append(res.DriverPackages, d)
		}
	}

	for i, p := range opt.ExtraDrivers {
		add(len(pkgs)+i, p, p)
	}

	return nil
}

func (w *workspace) copyBootFiles(res *BuildResult) error {
	mnt, media := w.mountDir(), w.mediaDir()

	for _, f := range BootFiles() {
		if err := CopyTree(filepath.Join(mnt, f.From), filepath.Join(media, f.To)); err != nil {
			return pkgerrors.Wrap(err, "the recovery image lacks boot files")
		}
	}

	// the RAM disk template next to Winre.wim matches it; the image has one too
	sdi := filepath.Join(filepath.Dir(res.WinRE), "boot.sdi")
	if _, err := os.Stat(sdi); err != nil {
		sdi = filepath.Join(mnt, `Windows\Boot\DVD\EFI\boot.sdi`)
	}

	return CopyFile(sdi, filepath.Join(media, MediaBootSDI))
}

func (w *workspace) finishMedia(opt *BuildOptions, res *BuildResult) error {
	media := w.mediaDir()

	wim := filepath.Join(media, MediaBootWIM)
	if err := os.MkdirAll(filepath.Dir(wim), 0o700); err != nil { //nolint:mnd
		return pkgerrors.Wrap(err, "unable to create the sources folder")
	}

	if err := os.Rename(w.wimPath(), wim); err != nil {
		return pkgerrors.Wrap(err, "unable to move the recovery image")
	}

	if err := os.WriteFile(filepath.Join(media, KitFileName), []byte(opt.Kit), 0o600); err != nil { //nolint:mnd
		return pkgerrors.Wrap(err, "unable to write the recovery kit")
	}

	if opt.Answers != nil {
		if err := os.WriteFile(filepath.Join(media, AnswersFileName), opt.Answers, 0o600); err != nil { //nolint:mnd
			return pkgerrors.Wrap(err, "unable to write the answers")
		}
	}

	size, err := TreeSize(media)
	res.Size = size

	return err
}

var winreLocationRE = regexp.MustCompile(`(?i)\\\\\?\\GLOBALROOT\\device\\harddisk\d+\\partition\d+\\\S+`)

// LocateWinRE returns the path of this computer's Winre.wim: on the recovery
// partition that "reagentc /info" names, or in System32\Recovery when WinRE
// is disabled.
func LocateWinRE(ctx context.Context) (string, error) {
	out, err := systemTool(ctx, "reagentc.exe", "/info")
	if err == nil {
		if loc := winreLocationRE.FindString(string(out)); loc != "" {
			p := filepath.Join(loc, "Winre.wim")
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}

	windir, werr := windows.GetWindowsDirectory()
	if werr != nil {
		return "", pkgerrors.Wrap(werr, "unable to find the Windows folder")
	}

	p := filepath.Join(windir, "System32", "Recovery", "Winre.wim")
	if _, serr := os.Stat(p); serr == nil {
		return p, nil
	}

	return "", pkgerrors.Errorf("this computer has no Windows Recovery Environment image: 'reagentc /info' names none and %v does not exist; enable it with 'reagentc /enable' or pass --winre", p)
}

func hostTimeZone() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\TimeZoneInformation`, registry.QUERY_VALUE)
	if err != nil {
		return "UTC"
	}
	defer k.Close() //nolint:errcheck

	tz, _, err := k.GetStringValue("TimeZoneKeyName")
	if err != nil || tz == "" {
		return "UTC"
	}

	return tz
}

// systemTool runs a program from the Windows system folder: the caller is an
// administrator, so a program of that name found elsewhere on PATH must not run.
func systemTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, pkgerrors.Wrap(err, "unable to find the Windows system folder")
	}

	out, err := exec.CommandContext(ctx, filepath.Join(system32, name), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return out, pkgerrors.Errorf("%v %v failed: %v: %s", name, strings.Join(args, " "), err, lastLines(out))
	}

	return out, nil
}

// dism runs DISM in English. Its options take values as /Name:value, where a
// value with spaces must be quoted after the colon, which Go's quoting of
// whole arguments does not do, so the command line is written here.
func dism(ctx context.Context, args ...string) error {
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return pkgerrors.Wrap(err, "unable to find the Windows system folder")
	}

	exe := filepath.Join(system32, "dism.exe")
	line := []string{`"` + exe + `"`, "/English", "/NoRestart"}

	for _, a := range args {
		if name, value, ok := strings.Cut(a, ":"); ok && strings.HasPrefix(name, "/") && strings.ContainsAny(value, " ") {
			a = name + `:"` + value + `"`
		}

		line = append(line, a)
	}

	cmd := exec.CommandContext(ctx, exe) //nolint:gosec // DISM from the Windows system folder
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: strings.Join(line, " ")}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return pkgerrors.Errorf("dism %v failed: %v: %s", strings.Join(args, " "), err, lastLines(out))
	}

	return nil
}

// lastLines returns the end of a tool's output, where the error is.
func lastLines(out []byte) string {
	const maxLines = 6

	lines := strings.Split(strings.TrimSpace(string(bytes.ReplaceAll(out, []byte("\r"), nil))), "\n")

	var kept []string

	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, strings.TrimSpace(l))
		}
	}

	if len(kept) > maxLines {
		kept = kept[len(kept)-maxLines:]
	}

	return strings.Join(kept, " | ")
}
