package toolchain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/hostinfo"
)

// Platform an artifact is built for, written as system, architecture and an optional Linux C library
type Target string

// Every target the lock may pin an artifact for
const (
	WindowsX64       Target = "win32-x64"
	WindowsARM64     Target = "win32-arm64"
	LinuxX64         Target = "linux-x64"
	LinuxARM64       Target = "linux-arm64"
	LinuxX64Musl     Target = "linux-x64-musl"
	LinuxARM64Musl   Target = "linux-arm64-musl"
	LinuxARM64Bionic Target = "linux-arm64-bionic"
	DarwinX64        Target = "darwin-x64"
	DarwinARM64      Target = "darwin-arm64"
)

// Lock key of an artifact that serves every target
const anyTarget = "any"

// Targets in the order status listings and gap tables walk them
var allTargets = []Target{WindowsX64, WindowsARM64, LinuxX64, LinuxARM64, LinuxX64Musl, LinuxARM64Musl, LinuxARM64Bionic, DarwinX64, DarwinARM64}

// Reports whether t is a target the lock may pin
func (t Target) known() bool {
	for _, k := range allTargets {
		if k == t {
			return true
		}
	}
	return false
}

// First part, one of win32, linux and darwin
func (t Target) system() string {
	s, _, _ := strings.Cut(string(t), "-")
	return s
}

// Architecture part, x64 or arm64
func (t Target) arch() string {
	parts := strings.Split(string(t), "-")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// Reports a Windows target
func (t Target) windows() bool { return t.system() == "win32" }

// Reports a Linux target built for the alternative small C library
func (t Target) musl() bool { return strings.HasSuffix(string(t), "-musl") }

// Reports an Android target
func (t Target) bionic() bool { return strings.HasSuffix(string(t), "-bionic") }

// Group key a per target table falls back to, win32 or posix
func (t Target) group() string {
	if t.windows() {
		return "win32"
	}
	return "posix"
}

// Value of table for t, by exact target, then its group, then the empty key
func lookupTarget(table map[string]string, t Target) (string, bool) {
	for _, key := range []string{string(t), t.group(), ""} {
		if v, ok := table[key]; ok {
			return v, true
		}
	}
	return "", false
}

// Target and C library of the machine a store installs for
type hostTarget struct {
	target  Target
	glibc   [2]int
	hasLib  bool
	machine Target
}

func (h hostTarget) runs() Target {
	if h.machine != "" {
		return h.machine
	}
	return h.target
}

// Target of this machine from its native architecture, operating system and Linux C library
func HostTarget(ctx context.Context, host *hostinfo.Host) (Target, error) {
	ht, err := detectTarget(ctx, host)
	return ht.target, err
}

// Target with the glibc release the artifact floors compare against
func detectTarget(ctx context.Context, host *hostinfo.Host) (hostTarget, error) {
	var arch string
	switch native := host.NativeArch(); native {
	case hostinfo.ArchAMD64:
		arch = "x64"
	case hostinfo.ArchARM64:
		arch = "arm64"
	default:
		return hostTarget{}, fmt.Errorf("unsupported architecture %s: %w", native, ErrUnavailable)
	}
	switch hostinfo.OSFamily {
	case hostinfo.FamilyWindows:
		return hostTarget{target: Target("win32-" + arch)}, nil
	case hostinfo.FamilyDarwin:
		return hostTarget{target: Target("darwin-" + arch)}, nil
	}
	libc, err := host.LibC(ctx)
	if err != nil && !errors.Is(err, hostinfo.ErrAbsent) {
		return hostTarget{}, fmt.Errorf("detecting the C library: %w", err)
	}
	ht := hostTarget{target: Target("linux-" + arch)}
	switch libc.Flavor {
	case hostinfo.Bionic:
		ht.target += "-bionic"
	case hostinfo.Musl:
		ht.target += "-musl"
	default:
		ht.glibc, ht.hasLib = parseRelease(libc.Version)
	}
	return ht, nil
}

// Major and minor of a dotted release such as 2.39, false when it does not parse
func parseRelease(text string) ([2]int, bool) {
	major, minor, ok := strings.Cut(strings.TrimSpace(text), ".")
	if !ok {
		return [2]int{}, false
	}
	minor, _, _ = strings.Cut(minor, ".")
	a, errA := strconv.Atoi(major)
	b, errB := strconv.Atoi(minor)
	if errA != nil || errB != nil || a < 0 || b < 0 {
		return [2]int{}, false
	}
	return [2]int{a, b}, true
}

// Reports whether release a is at least release b
func releaseAtLeast(a, b [2]int) bool {
	return a[0] > b[0] || (a[0] == b[0] && a[1] >= b[1])
}
