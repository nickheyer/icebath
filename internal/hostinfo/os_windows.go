package hostinfo

import (
	"context"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Family of the operating system this binary was built for
const OSFamily = FamilyWindows

var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procIsWow64Process2          = kernel32.NewProc("IsWow64Process2")
	procGetMachineTypeAttributes = kernel32.NewProc("GetMachineTypeAttributes")
)

const machineAttributeUserEnabled = 0x1

var runnableMachineOrder = []uint16{0xAA64, 0x8664, 0x014C}

var (
	windowsMachineNames  = map[uint16]string{0x8664: "AMD64", 0xAA64: "ARM64", 0x014C: "x86"}
	windowsMachineArches = map[uint16]Arch{0x8664: ArchAMD64, 0xAA64: ArchARM64, 0x014C: ArchX86}
)

// Release name and the lowest version it starts at, newest first
type windowsRelease struct {
	major, minor, build uint32
	name                string
}

var (
	windowsClientReleases = []windowsRelease{{10, 1, 0, "post11"}, {10, 0, 22000, "11"}, {6, 4, 0, "10"}, {6, 3, 0, "8.1"},
		{6, 2, 0, "8"}, {6, 1, 0, "7"}, {6, 0, 0, "Vista"}, {5, 2, 3790, "XP64"}, {5, 2, 0, "XPMedia"}, {5, 1, 0, "XP"}, {5, 0, 0, "2000"}}
	windowsServerReleases = []windowsRelease{{10, 1, 0, "post2025Server"}, {10, 0, 26100, "2025Server"}, {10, 0, 20348, "2022Server"},
		{10, 0, 17763, "2019Server"}, {6, 4, 0, "2016Server"}, {6, 3, 0, "2012ServerR2"}, {6, 2, 0, "2012Server"},
		{6, 1, 0, "2008ServerR2"}, {6, 0, 0, "2008Server"}, {5, 2, 0, "2003Server"}, {5, 0, 0, "2000Server"}}
)

// Identity of the running Windows from the kernel version and native machine, Node empty without a host name
func probeOS() (OS, error) {
	info := windows.RtlGetVersion()
	version := &WindowsVersion{Major: info.MajorVersion, Minor: info.MinorVersion, Build: info.BuildNumber,
		ServicePackMajor: info.ServicePackMajor, ServicePackMinor: info.ServicePackMinor, Server: info.ProductType != 1}
	node, err := os.Hostname()
	if err != nil {
		node = ""
	}
	return OS{System: "Windows", Release: windowsReleaseName(version),
		Version: fmt.Sprintf("%d.%d.%d", version.Major, version.Minor, version.Build),
		Machine: windowsMachine(), Node: node, Windows: version}, nil
}

// Client or server release name of a Windows version such as 11 or 2022Server
func windowsReleaseName(v *WindowsVersion) string {
	table := windowsClientReleases
	if v.Server {
		table = windowsServerReleases
	}
	for _, r := range table {
		if v.Major > r.major || (v.Major == r.major && (v.Minor > r.minor || (v.Minor == r.minor && v.Build >= r.build))) {
			return r.name
		}
	}
	return ""
}

// Native machine IsWow64Process2 reports, false when the call is missing or names none
func windowsNativeMachine() (uint16, bool) {
	if procIsWow64Process2.Find() != nil {
		return 0, false
	}
	var process, native uint16
	r, _, _ := procIsWow64Process2.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&process)), uintptr(unsafe.Pointer(&native)))
	if r == 0 || native == 0 {
		return 0, false
	}
	return native, true
}

// Machine name the processor environment variables give, the WOW64 one first
func windowsMachineVariable() string {
	if arch := os.Getenv("PROCESSOR_ARCHITEW6432"); arch != "" {
		return arch
	}
	return os.Getenv("PROCESSOR_ARCHITECTURE")
}

// Native machine name in the AMD64 ARM64 x86 vocabulary
func windowsMachine() string {
	if native, ok := windowsNativeMachine(); ok {
		if name, known := windowsMachineNames[native]; known {
			return name
		}
	}
	return windowsMachineVariable()
}

// Native machine architecture even for an x64 build emulated on arm64
func nativeArch() Arch {
	if native, ok := windowsNativeMachine(); ok {
		if arch, known := windowsMachineArches[native]; known {
			return arch
		}
	}
	return parseArch(windowsMachineVariable())
}

// Architectures this host runs in user mode natively or emulated, ErrAbsent before Windows 11 reports them
func RunnableArches() ([]Arch, error) {
	if procGetMachineTypeAttributes.Find() != nil {
		return nil, fmt.Errorf("runnable architectures: %w", ErrAbsent)
	}
	var arches []Arch
	for _, machine := range runnableMachineOrder {
		var attributes int32
		hresult, _, _ := procGetMachineTypeAttributes.Call(uintptr(machine), uintptr(unsafe.Pointer(&attributes)))
		if int32(hresult) == 0 && attributes&machineAttributeUserEnabled != 0 {
			arches = append(arches, windowsMachineArches[machine])
		}
	}
	if len(arches) == 0 {
		return nil, fmt.Errorf("runnable architectures: %w", ErrAbsent)
	}
	return arches, nil
}

// Release, version and service pack joined the way the dashboard shows them
func platformLabel(_ context.Context, h *Host) (string, error) {
	o, err := h.OS()
	if err != nil {
		return "", err
	}
	servicePack := fmt.Sprintf("SP%d", o.Windows.ServicePackMajor)
	if o.Windows.ServicePackMinor != 0 {
		servicePack = fmt.Sprintf("SP%d.%d", o.Windows.ServicePackMajor, o.Windows.ServicePackMinor)
	}
	return joinPlatform(o.System, o.Release, o.Version, servicePack), nil
}
