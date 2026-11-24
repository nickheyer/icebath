package hostutil

import (
	"os"
	"runtime"
	"strconv"

	"golang.org/x/sys/windows"
)

func readOSName() string { return "Windows" }

// Architecture Windows reports for this process, the native one first when it runs under WOW64
func readMachine() string {
	if arch := os.Getenv("PROCESSOR_ARCHITEW6432"); arch != "" {
		return arch
	}
	if arch := os.Getenv("PROCESSOR_ARCHITECTURE"); arch != "" {
		return arch
	}
	switch runtime.GOARCH {
	case "amd64":
		return "AMD64"
	case "arm64":
		return "ARM64"
	case "386":
		return "x86"
	}
	return runtime.GOARCH
}

type windowsRelease struct {
	major, minor, build uint32
	name                string
}

// Client and server release names keyed by the lowest version each starts at, newest first
var (
	clientReleases = []windowsRelease{{10, 1, 0, "post11"}, {10, 0, 22000, "11"}, {6, 4, 0, "10"}, {6, 3, 0, "8.1"}, {6, 2, 0, "8"},
		{6, 1, 0, "7"}, {6, 0, 0, "Vista"}, {5, 2, 3790, "XP64"}, {5, 2, 0, "XPMedia"}, {5, 1, 0, "XP"}, {5, 0, 0, "2000"}}
	serverReleases = []windowsRelease{{10, 1, 0, "post2025Server"}, {10, 0, 26100, "2025Server"}, {10, 0, 20348, "2022Server"}, {10, 0, 17763, "2019Server"},
		{6, 4, 0, "2016Server"}, {6, 3, 0, "2012ServerR2"}, {6, 2, 0, "2012Server"}, {6, 1, 0, "2008ServerR2"}, {6, 0, 0, "2008Server"},
		{5, 2, 0, "2003Server"}, {5, 0, 0, "2000Server"}}
)

// Product type RtlGetVersion reports for a client edition (VER_NT_WORKSTATION); domain controllers and other servers report another
const productTypeWorkstation = 1

// Product release named from the version and edition, empty for a version older than every known one
func readRelease() string {
	info := windows.RtlGetVersion()
	table := clientReleases
	if info.ProductType != productTypeWorkstation {
		table = serverReleases
	}
	for _, r := range table {
		if info.MajorVersion != r.major {
			if info.MajorVersion > r.major {
				return r.name
			}
			continue
		}
		if info.MinorVersion > r.minor || (info.MinorVersion == r.minor && info.BuildNumber >= r.build) {
			return r.name
		}
	}
	return ""
}

// Label of a Windows host: release name, major.minor.build version and service pack
func readPlatform() string {
	info := windows.RtlGetVersion()
	version := strconv.FormatUint(uint64(info.MajorVersion), 10) + "." + strconv.FormatUint(uint64(info.MinorVersion), 10) + "." +
		strconv.FormatUint(uint64(info.BuildNumber), 10)
	servicePack := "SP" + strconv.FormatUint(uint64(info.ServicePackMajor), 10)
	if info.ServicePackMinor != 0 {
		servicePack += "." + strconv.FormatUint(uint64(info.ServicePackMinor), 10)
	}
	return joinLabel(OSName(), Release(), version, servicePack)
}
