package hostinfo

import (
	"runtime"
	"strings"
)

// Platform identifier plugin catalogs, skills and user agents key on
type Family string

const (
	FamilyLinux   Family = "linux"
	FamilyDarwin  Family = "darwin"
	FamilyWindows Family = "win32"
)

// Canonical processor architecture name
type Arch string

const (
	ArchAMD64   Arch = "amd64"
	ArchARM64   Arch = "arm64"
	ArchX86     Arch = "x86"
	ArchUnknown Arch = "unknown"
)

var archNames = map[string]Arch{
	"amd64": ArchAMD64, "x86_64": ArchAMD64, "x64": ArchAMD64,
	"arm64": ArchARM64, "aarch64": ArchARM64,
	"x86": ArchX86, "i386": ArchX86, "i686": ArchX86,
}

// Canonical architecture of a machine name in any common spelling, unknown when unrecognized
func parseArch(raw string) Arch {
	if arch, ok := archNames[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return arch
	}
	return ArchUnknown
}

// Architecture this binary was built for
func processArch() Arch {
	if runtime.GOARCH == "386" {
		return ArchX86
	}
	return parseArch(runtime.GOARCH)
}

// Machine architecture even when this process runs under Rosetta or x64 emulation
func (h *Host) NativeArch() Arch { return h.nativeArch.value(nativeArch) }
