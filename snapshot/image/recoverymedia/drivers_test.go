package recoverymedia

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/snapshot/image"
)

const pnputilOutput = "Microsoft PnP Utility\r\n" +
	"\r\n" +
	"Published Name:     oem8.inf\r\n" +
	"Original Name:      alderlakedmasecextension.inf\r\n" +
	"Provider Name:      INTEL\r\n" +
	"Class Name:         Extension\r\n" +
	"Class GUID:         {e2f84ce7-8efa-411c-aa69-97454ca4cb57}\r\n" +
	"Extension ID:       {aaa2e5d0-bba2-4900-95b4-fee1e274e6fe}\r\n" +
	"Driver Version:     07/18/1968 10.1.45.9\r\n" +
	"Signer Name:        Microsoft Windows Hardware Compatibility Publisher\r\n" +
	"\r\n" +
	"Published Name:     oem40.inf\r\n" +
	"Original Name:      rt640x64.inf\r\n" +
	"Provider Name:      Realtek\r\n" +
	"Class Name:         Net\r\n" +
	"Class GUID:         {4d36e972-e325-11ce-bfc1-08002be10318}\r\n" +
	"Driver Version:     05/12/2023 10.65.512.2023\r\n" +
	"Signer Name:        Microsoft Windows Hardware Compatibility Publisher\r\n" +
	"\r\n" +
	// a second, older version of the same driver, with German labels
	"Veröffentlichter Name:     oem41.inf\r\n" +
	"Originalname:              rt640x64.inf\r\n" +
	"Anbietername:              Realtek\r\n" +
	"Klassenname:               Netzwerkadapter\r\n" +
	"Klassen-GUID:              {4d36e972-e325-11ce-bfc1-08002be10318}\r\n" +
	"Treiberversion:            01.02.2022 10.9.1.2022\r\n" +
	"\r\n" +
	"Published Name:     oem3.inf\r\n" +
	"Original Name:      iastorvd.inf\r\n" +
	"Provider Name:      Intel Corporation\r\n" +
	"Class Name:         Storage controllers\r\n" +
	"Class GUID:         {4D36E97B-E325-11CE-BFC1-08002BE10318}\r\n" +
	"Driver Version:     01/01/2024 19.5.1.1040\r\n"

func TestParseDriverList(t *testing.T) {
	got := ParseDriverList(pnputilOutput)
	require.Equal(t, []Driver{
		{Published: "oem8.inf", Original: "alderlakedmasecextension.inf", ClassGUID: "{e2f84ce7-8efa-411c-aa69-97454ca4cb57}", Version: "10.1.45.9"},
		{Published: "oem40.inf", Original: "rt640x64.inf", ClassGUID: "{4d36e972-e325-11ce-bfc1-08002be10318}", Version: "10.65.512.2023"},
		{Published: "oem41.inf", Original: "rt640x64.inf", ClassGUID: "{4d36e972-e325-11ce-bfc1-08002be10318}", Version: "10.9.1.2022"},
		{Published: "oem3.inf", Original: "iastorvd.inf", ClassGUID: "{4D36E97B-E325-11CE-BFC1-08002BE10318}", Version: "19.5.1.1040"},
	}, got)
}

func TestSelectDrivers(t *testing.T) {
	got := SelectDrivers(ParseDriverList(pnputilOutput))
	require.Len(t, got, 2)
	require.Equal(t, "oem40.inf", got[0].Published, "the newer Realtek driver")
	require.Equal(t, "Net", got[0].Class())
	require.Equal(t, "oem3.inf", got[1].Published)
	require.Equal(t, "SCSIAdapter", got[1].Class())
}

func TestDriverImage(t *testing.T) {
	d := SelectDrivers(ParseDriverList(pnputilOutput))[0]
	require.Equal(t, image.Driver{Class: "Net", INF: "rt640x64.inf", Version: "10.65.512.2023", Entry: "oem40"}, d.Image())
}

func TestNewestPackages(t *testing.T) {
	host := image.DriverSource{Class: "Net", INF: "rt640x64.inf", Version: "10.65", Dir: "host"}
	got := NewestPackages([]image.DriverSource{
		host,
		{Class: "Net", INF: "RT640X64.INF", Version: "10.65", Dir: "laptop"},
		{Class: "SCSIAdapter", INF: "iastorvd.inf", Version: "18.0", Dir: "nas"},
		{Class: "SCSIAdapter", INF: "iastorvd.inf", Version: "19.5", Dir: "laptop"},
	})
	require.Equal(t, []image.DriverSource{
		host,
		{Class: "SCSIAdapter", INF: "iastorvd.inf", Version: "19.5", Dir: "laptop"},
	}, got)
}

func TestMissingDrivers(t *testing.T) {
	media := []image.Driver{{Class: "Net", INF: "rt640x64.inf", Version: "10.1"}}
	need := []image.Driver{
		{Class: "Net", INF: "rt640x64.inf", Version: "10.65", Entry: "oem40"},
		{Class: "USB", INF: "asmtxhci.inf", Version: "1.16", Entry: "oem7"},
	}
	require.Equal(t, []image.Driver{{Class: "USB", INF: "asmtxhci.inf", Version: "1.16"}}, MissingDrivers(media, need))
	require.Empty(t, MissingDrivers(need, media))
}

func TestCompareVersions(t *testing.T) {
	require.Positive(t, compareVersions("10.65.512.2023", "10.9.1.2022"))
	require.Negative(t, compareVersions("1.2", "1.2.1"))
	require.Zero(t, compareVersions("1.2.0", "1.2"))
}

func TestStartScript(t *testing.T) {
	s := StartScript()
	require.Contains(t, s, "wpeinit\r\n")
	require.Contains(t, s, `set LOCALAPPDATA=X:\kopia\appdata\local`)
	require.Contains(t, s, `X:\kopia\kopia.exe --config-file X:\kopia\repository.config --no-use-credential-manager --log-dir X:\kopia\logs image recover --recovery-file X:\kopia\kopia-recovery.json`)
	require.Equal(t, "[LaunchApps]\r\n%SYSTEMROOT%\\System32\\cmd.exe, /c X:\\kopia\\start.cmd\r\n", WinpeshlINI())
}

// pnputilDevicesExtension is from a laptop whose Wi-Fi adapter has an
// extension driver.
const pnputilDevicesExtension = "Microsoft PnP Utility\r\n" +
	"\r\n" +
	"Instance ID:                SWD\\Wintun\\{37217669-42DA-4657-A55B-0D995D328250}\r\n" +
	"Device Description:         Tailscale Tunnel\r\n" +
	"Class Name:                 Net\r\n" +
	"Class GUID:                 {4d36e972-e325-11ce-bfc1-08002be10318}\r\n" +
	"Manufacturer Name:          WireGuard LLC\r\n" +
	"Status:                     Started\r\n" +
	"Driver Name:                oem3.inf\r\n" +
	"\r\n" +
	"Instance ID:                PCI\\VEN_8086&DEV_51F0&SUBSYS_00948086&REV_01\\3&11583659&3&A3\r\n" +
	"Device Description:         Intel(R) Wi-Fi 6E AX211 160MHz\r\n" +
	"Class Name:                 Net\r\n" +
	"Class GUID:                 {4d36e972-e325-11ce-bfc1-08002be10318}\r\n" +
	"Manufacturer Name:          Intel Corporation\r\n" +
	"Status:                     Started\r\n" +
	"Driver Name:                oem100.inf\r\n" +
	"Extension Driver Names:     oem111.inf\r\n" +
	"\r\n" +
	"Instance ID:                BTH\\MS_BTHPAN\\6&2108ecab&0&2\r\n" +
	"Device Description:         Bluetooth Device (Personal Area Network)\r\n" +
	"Class Name:                 Net\r\n" +
	"Class GUID:                 {4d36e972-e325-11ce-bfc1-08002be10318}\r\n" +
	"Manufacturer Name:          Microsoft\r\n" +
	"Status:                     Started\r\n" +
	"Driver Name:                bthpan.inf\r\n"

const pnputilDevices = "Microsoft PnP Utility\r\n" +
	"\r\n" +
	"Instance ID:                PCI\\VEN_10EC&DEV_8125&SUBSYS_87D71043&REV_05\\01000000684CE00000\r\n" +
	"Device Description:         Realtek Gaming 2.5GbE Family Controller\r\n" +
	"Class Name:                 Net\r\n" +
	"Class GUID:                 {4d36e972-e325-11ce-bfc1-08002be10318}\r\n" +
	"Manufacturer Name:          Realtek\r\n" +
	"Status:                     Started\r\n" +
	"Driver Name:                OEM40.inf\r\n" +
	"\r\n" +
	// a virtual network adapter
	"Instance ID:                ROOT\\NET\\0000\r\n" +
	"Device Description:         VirtualBox Host-Only Ethernet Adapter\r\n" +
	"Driver Name:                oem77.inf\r\n" +
	"\r\n" +
	"Instance ID:                SWD\\XvddEnum\\XvddRootDevice_Instance\r\n" +
	"Driver Name:                oem78.inf\r\n" +
	"\r\n" +
	// a driver that comes with Windows, with German labels
	"Instanz-ID:                 USB\\ROOT_HUB30\\4&1d2f3e4&0&0\r\n" +
	"Gerätebeschreibung:         USB-Root-Hub (USB 3.0)\r\n" +
	"Treibername:                usbhub3.inf\r\n" +
	"\r\n" +
	"Instanz-ID:                 PCI\\VEN_8086&DEV_09AB\\3&11583659&0&B8\r\n" +
	"Treibername:                oem3.inf\r\n"

func TestParseDeviceDrivers(t *testing.T) {
	require.Equal(t, map[string]bool{"oem40.inf": true, "oem3.inf": true}, ParseDeviceDrivers(pnputilDevices))

	// a laptop's Wi-Fi adapter with an extension driver, next to a virtual
	// adapter: the adapter's driver is the first oemN.inf, not the extension
	require.Equal(t, map[string]bool{"oem100.inf": true}, ParseDeviceDrivers(pnputilDevicesExtension))

	// a computer whose hardware uses no third-party driver
	require.Equal(t, map[string]bool{}, ParseDeviceDrivers("Microsoft PnP Utility\r\n\r\nInstance ID: USB\\ROOT_HUB30\\4&1\r\nDriver Name: usbhub3.inf\r\n"))

	// pnputil without /enum-devices prints its usage
	require.Nil(t, ParseDeviceDrivers("PNPUTIL [/add-driver <...>]\r\n\r\nCommands:\r\n  Add driver package:\r\n    pnputil /add-driver x:\\driver.inf\r\n"))
}

func TestDriversInUse(t *testing.T) {
	// the Realtek driver in use is the older version of the two in the store
	got := SelectDrivers(DriversInUse(ParseDriverList(pnputilOutput), map[string]bool{"oem41.inf": true}))
	require.Len(t, got, 1)
	require.Equal(t, "oem41.inf", got[0].Published)
}
