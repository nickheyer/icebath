//go:build windows

package pyplatform

import (
	"os"

	"golang.org/x/sys/windows"
)

func system() string { return "Windows" }

// Native architecture the way CPython reads it, the WOW64 variable first
func machine() string {
	if arch := os.Getenv("PROCESSOR_ARCHITEW6432"); arch != "" {
		return arch
	}
	return os.Getenv("PROCESSOR_ARCHITECTURE")
}

// Client and server release names keyed by the lowest version each one starts at, newest first
var (
	windowsClientReleases = []windowsRelease{{10, 1, 0, "post11"}, {10, 0, 22000, "11"}, {6, 4, 0, "10"}, {6, 3, 0, "8.1"}, {6, 2, 0, "8"},
		{6, 1, 0, "7"}, {6, 0, 0, "Vista"}, {5, 2, 3790, "XP64"}, {5, 2, 0, "XPMedia"}, {5, 1, 0, "XP"}, {5, 0, 0, "2000"}}
	windowsServerReleases = []windowsRelease{{10, 1, 0, "post2022Server"}, {10, 0, 20348, "2022Server"}, {10, 0, 17763, "2019Server"},
		{6, 4, 0, "2016Server"}, {6, 3, 0, "2012ServerR2"}, {6, 2, 0, "2012Server"}, {6, 1, 0, "2008ServerR2"}, {6, 0, 0, "2008Server"},
		{5, 2, 0, "2003Server"}, {5, 0, 0, "2000Server"}}
)

type windowsRelease struct {
	major, minor, build uint32
	name                string
}

// Release name CPython platform.release derives from the version and product type
func release() string {
	info := windows.RtlGetVersion()
	table := windowsClientReleases
	if info.ProductType == 3 {
		table = windowsServerReleases
	}
	for _, r := range table {
		if info.MajorVersion > r.major || (info.MajorVersion == r.major && (info.MinorVersion > r.minor || (info.MinorVersion == r.minor && info.BuildNumber >= r.build))) {
			return r.name
		}
	}
	return ""
}
