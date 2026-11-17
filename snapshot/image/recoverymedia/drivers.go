package recoverymedia

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kopia/kopia/snapshot/image"
)

// DriverClasses are the device setup classes whose drivers the recovery
// system needs to reach disks and the network, by class GUID.
var DriverClasses = map[string]string{
	"{4d36e972-e325-11ce-bfc1-08002be10318}": "Net",
	"{4d36e97b-e325-11ce-bfc1-08002be10318}": "SCSIAdapter",
	"{4d36e96a-e325-11ce-bfc1-08002be10318}": "HDC",
	"{36fc9e60-c465-11cf-8056-444553540000}": "USB",
}

// Driver is a third-party driver package in the Windows driver store.
type Driver struct {
	// Published is the store's name for it, e.g. "oem12.inf".
	Published string

	// Original is the vendor's INF name, e.g. "rt640x64.inf".
	Original string

	ClassGUID string
	Version   string
}

// Class returns the short name of the driver's class, or "".
func (d *Driver) Class() string {
	return DriverClasses[strings.ToLower(d.ClassGUID)]
}

// Image returns how images and media records identify the driver: its
// folder in an image is named after its published name.
func (d *Driver) Image() image.Driver {
	return image.Driver{Class: d.Class(), INF: d.Original, Version: d.Version, Entry: strings.TrimSuffix(strings.ToLower(d.Published), ".inf")}
}

var (
	publishedNameRE = regexp.MustCompile(`(?i)^oem\d+\.inf$`)
	classGUIDRE     = regexp.MustCompile(`^\{[0-9a-fA-F-]{36}\}$`)
	driverVersionRE = regexp.MustCompile(`^\S+\s+(\d+(?:\.\d+)+)$`)
)

// ParseDriverList parses the output of "pnputil /enum-drivers". The labels
// are translated on non-English Windows, so fields are recognized by their
// values: the published name oemN.inf, the original INF name, the first
// braced GUID (the class; an extension ID follows it) and the date and
// version.
func ParseDriverList(out string) []Driver {
	var (
		drivers []Driver
		cur     Driver
	)

	flush := func() {
		if cur.Published != "" {
			drivers = append(drivers, cur)
		}

		cur = Driver{}
	}

	for line := range strings.SplitSeq(strings.ReplaceAll(out, "\r", ""), "\n") {
		_, value, ok := strings.Cut(line, ":")
		if !ok {
			if strings.TrimSpace(line) == "" {
				flush()
			}

			continue
		}

		value = strings.TrimSpace(value)

		switch {
		case publishedNameRE.MatchString(value):
			flush()

			cur.Published = value
		case strings.HasSuffix(strings.ToLower(value), ".inf") && cur.Original == "":
			cur.Original = value
		case classGUIDRE.MatchString(value) && cur.ClassGUID == "":
			cur.ClassGUID = value
		case cur.Version == "":
			if m := driverVersionRE.FindStringSubmatch(value); m != nil {
				cur.Version = m[1]
			}
		}
	}

	flush()

	return drivers
}

// SelectDrivers returns the storage, network and USB drivers, the newest
// version of each original INF only, ordered by class and name.
func SelectDrivers(all []Driver) []Driver {
	newest := map[string]Driver{}

	for _, d := range all {
		if d.Class() == "" {
			continue
		}

		key := strings.ToLower(d.Original)
		if prev, ok := newest[key]; !ok || compareVersions(d.Version, prev.Version) > 0 {
			newest[key] = d
		}
	}

	out := make([]Driver, 0, len(newest))
	for _, d := range newest {
		out = append(out, d)
	}

	slices.SortFunc(out, func(a, b Driver) int {
		if c := strings.Compare(a.Class(), b.Class()); c != 0 {
			return c
		}

		return strings.Compare(strings.ToLower(a.Original), strings.ToLower(b.Original))
	})

	return out
}

// compareVersions compares dotted numeric versions.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")

	for i := range max(len(pa), len(pb)) {
		var x, y int

		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}

		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}

		if x != y {
			return x - y
		}
	}

	return 0
}

// NewestPackages keeps the newest version of each driver, the first one
// given among equal versions, in the order given.
func NewestPackages(pkgs []image.DriverSource) []image.DriverSource {
	var out []image.DriverSource

	for _, p := range pkgs {
		i := slices.IndexFunc(out, func(o image.DriverSource) bool { return image.SameDriver(o.Driver, p.Driver) })

		switch {
		case i < 0:
			out = append(out, p)
		case compareVersions(p.Version, out[i].Version) > 0:
			out[i] = p
		}
	}

	return out
}

// MissingDrivers returns the drivers of need of which media holds no version.
// Any version of a driver usually supports the devices of the computer.
func MissingDrivers(media, need []image.Driver) []image.Driver {
	var out []image.Driver

	for _, d := range need {
		if !slices.ContainsFunc(media, func(m image.Driver) bool { return image.SameDriver(m, d) }) {
			d.Entry = ""
			out = append(out, d)
		}
	}

	return out
}
