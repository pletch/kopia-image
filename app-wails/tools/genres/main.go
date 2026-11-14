// Command genres generates rsrc_windows_amd64.syso, which the Go linker embeds
// in KopiaUI-Wails.exe. It contains:
//
//   - the application icon as resource ID 3, which Wails uses for the window
//     icon and Explorer uses for the executable;
//   - an application manifest enabling per-monitor DPI awareness and Common
//     Controls v6 (needed by Wails' task dialogs).
//
// Run with `go generate` from the app-wails directory.
package main

import (
	"log"
	"os"

	"github.com/tc-hib/winres"
)

// wailsAppIconID is the icon resource ID Wails loads for windows.
const wailsAppIconID = 3

func main() {
	f, err := os.Open("icons/app.ico")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close() //nolint:errcheck

	icon, err := winres.LoadICO(f)
	if err != nil {
		log.Fatalf("unable to load icon: %v", err)
	}

	rs := &winres.ResourceSet{}

	if err := rs.SetIcon(winres.ID(wailsAppIconID), icon); err != nil {
		log.Fatalf("unable to set icon: %v", err)
	}

	rs.SetManifest(winres.AppManifest{
		Identity:            winres.AssemblyIdentity{Name: "io.kopia.kopiaui-wails", Version: [4]uint16{0, 1, 0, 0}},
		Description:         "KopiaUI-Wails",
		Compatibility:       winres.Win10AndAbove,
		DPIAwareness:        winres.DPIPerMonitorV2,
		UseCommonControlsV6: true,
		LongPathAware:       true,
	})

	out, err := os.Create("rsrc_windows_amd64.syso")
	if err != nil {
		log.Fatal(err)
	}

	if err := rs.WriteObject(out, winres.ArchAMD64); err != nil {
		log.Fatalf("unable to write resources: %v", err)
	}

	if err := out.Close(); err != nil {
		log.Fatal(err)
	}
}
