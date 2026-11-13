package hostinfo

import (
	"context"
	"os"
	"regexp"
	"strconv"

	"golang.org/x/sys/unix"
)

// Family of the operating system this binary was built for
const OSFamily = FamilyDarwin

const (
	systemVersionPlist = "/System/Library/CoreServices/SystemVersion.plist"
	machOLinkage       = "Mach-O"
)

var productVersionPattern = regexp.MustCompile(`<key>ProductVersion</key>\s*<string>([^<]*)</string>`)

// Identity of the running Darwin kernel with the macOS product version, MacOS empty when neither source names one
func probeOS() (OS, error) {
	o, err := unameOS()
	if err != nil {
		return OS{}, err
	}
	o.MacOS = macProductVersion()
	return o, nil
}

// Product version from the system version property list, else from the kernel product version sysctl
func macProductVersion() string {
	if data, err := os.ReadFile(systemVersionPlist); err == nil {
		if m := productVersionPattern.FindSubmatch(data); m != nil && len(m[1]) > 0 {
			return string(m[1])
		}
	}
	version, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return version
}

// Apple silicon when an x86_64 build of this binary runs under Rosetta
func nativeArch() Arch {
	if processArch() == ArchAMD64 {
		if translated, err := unix.SysctlUint32("sysctl.proc_translated"); err == nil && translated == 1 {
			return ArchARM64
		}
	}
	return processArch()
}

// macOS product or Darwin kernel, machine, processor family, pointer width and linkage joined the way the dashboard shows them
func platformLabel(_ context.Context, h *Host) (string, error) {
	o, err := h.OS()
	if err != nil {
		return "", err
	}
	system, release := o.System, o.Release
	if o.MacOS != "" {
		system, release = "macOS", o.MacOS
	}
	processor := darwinProcessor()
	if processor == o.Machine {
		processor = ""
	}
	return joinPlatform(system, release, o.Machine, processor, strconv.Itoa(strconv.IntSize)+"bit", machOLinkage), nil
}

// Processor family uname prints for its p flag, from the CPU type the kernel reports, empty when it reports none
func darwinProcessor() string {
	cpuType, err := unix.SysctlUint32("hw.cputype")
	if err != nil {
		return ""
	}
	switch cpuType & 0x00FFFFFF {
	case 7:
		return "i386"
	case 12:
		return "arm"
	case 18:
		return "powerpc"
	}
	return ""
}
