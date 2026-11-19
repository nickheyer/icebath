//go:build unix

package pyplatform

import (
	"os"
	"os/exec"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyplist"
)

// Processor name uname prints with the p flag, empty for an unknown one or a failed command
func processor() string {
	cmd := exec.Command("uname", "-p")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	if name := strings.TrimSpace(string(out)); name != "unknown" {
		return name
	}
	return ""
}

// Product version of macOS from the system version property list, empty when unreadable
func macRelease() string {
	raw, err := os.ReadFile("/System/Library/CoreServices/SystemVersion.plist")
	if err != nil {
		return ""
	}
	doc, err := pyplist.Load(raw)
	if err != nil {
		return ""
	}
	dict, ok := doc.(*pyplist.Dict)
	if !ok {
		return ""
	}
	release, _ := dict.Get("ProductVersion")
	text, _ := release.(string)
	return text
}

// Platform string of a unix host, the libc of the executable on Linux and its bits and linkage elsewhere
func platformLabel() string {
	sys, rel, mach := system(), release(), machine()
	proc := processor()
	if mach == proc {
		proc = ""
	}
	if sys == "Darwin" {
		if macos := macRelease(); macos != "" {
			sys, rel = "macOS", macos
		}
	}
	if sys == "Linux" {
		lib, ver, ok := confstrLibcVer()
		if !ok {
			if executable, err := os.Executable(); err == nil {
				lib, ver = libcVer(executable)
			}
		}
		return joinPlatform(sys, rel, mach, proc, "with", lib+ver)
	}
	bits, linkage := architecture()
	return joinPlatform(sys, rel, mach, proc, bits, linkage)
}
