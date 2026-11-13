package hostinfo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"unicode"
)

// Identity of the running operating system as uname and the Windows version calls report it
type OS struct {
	System  string
	Release string
	Version string
	Machine string
	Node    string
	MacOS   string
	Windows *WindowsVersion
}

// Kernel version Windows reports to this process, Server set for server editions
type WindowsVersion struct {
	Major            uint32
	Minor            uint32
	Build            uint32
	ServicePackMajor uint16
	ServicePackMinor uint16
	Server           bool
}

const windows11Build = 22000

// System, release and machine joined by spaces, the os line of the dump command
func (o OS) String() string { return o.System + " " + o.Release + " " + o.Machine }

// Product release on macOS, else the kernel or Windows release, as the machine summary reports it
func (o OS) releaseName() string {
	if o.MacOS != "" {
		return o.MacOS
	}
	return o.Release
}

// Identity of the running operating system, read once
func (h *Host) OS() (OS, error) { return h.os.get(probeOS) }

// Names this host the way the system prompt introduces it, WSL first
func (h *Host) Label() (string, error) {
	if h.WSL() {
		return "WSL (Windows Subsystem for Linux)", nil
	}
	o, err := h.OS()
	if err != nil {
		return "", err
	}
	switch {
	case o.Windows != nil && o.Windows.Build >= windows11Build:
		return "Windows (11)", nil
	case o.Windows != nil:
		return "Windows (10)", nil
	case OSFamily == FamilyDarwin:
		return "macOS (" + o.releaseName() + ")", nil
	}
	return o.System + " (" + o.Release + ")", nil
}

// File name safe platform string the dashboard shows, such as Linux-6.8.0-x86_64-with-glibc2.39, read once
func (h *Host) Platform(ctx context.Context) (string, error) {
	return h.platform.get(ctx, func(ctx context.Context) (string, error) { return platformLabel(ctx, h) })
}

var platformReplacer = strings.NewReplacer(" ", "_", "/", "-", "\\", "-", ":", "-", ";", "-", "\"", "-", "(", "-", ")", "-")

// Non empty parts joined by dashes with unsafe characters and unknown words dropped and dashes folded
func joinPlatform(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, strings.TrimSpace(part))
		}
	}
	label := strings.ReplaceAll(platformReplacer.Replace(strings.Join(kept, "-")), "unknown", "")
	for strings.Contains(label, "--") {
		label = strings.ReplaceAll(label, "--", "-")
	}
	return strings.TrimRight(label, "-")
}

// Identity of a Linux distribution as its os-release file states it, linux for a file without an ID
type Distribution struct {
	ID     string
	IDLike []string
}

var osReleasePaths = []string{"/etc/os-release", "/usr/lib/os-release"}

// Linux distribution os-release names, read once, ErrAbsent when the host has no os-release file
func (h *Host) Distribution() (Distribution, error) { return h.distro.get(readDistribution) }

// First readable os-release file parsed, ErrAbsent when none exists
func readDistribution() (Distribution, error) {
	var failure error
	for _, path := range osReleasePaths {
		data, err := os.ReadFile(path)
		if err == nil {
			return parseOSRelease(string(data)), nil
		}
		if failure == nil && !errors.Is(err, fs.ErrNotExist) {
			failure = err
		}
	}
	if failure != nil {
		return Distribution{}, fmt.Errorf("os-release: %w", failure)
	}
	return Distribution{}, fmt.Errorf("os-release: %w", ErrAbsent)
}

var (
	osReleaseKey      = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	osReleaseUnescape = regexp.MustCompile("\\\\([\\\\$\"'`])")
	lineEndings       = strings.NewReplacer("\r\n", "\n", "\r", "\n")
)

// ID and ID_LIKE of os-release text unquoted and unescaped, ID_LIKE split at spaces and commas
func parseOSRelease(text string) Distribution {
	fields := map[string]string{"ID": "linux"}
	for _, line := range strings.Split(lineEndings.Replace(text), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found || !osReleaseKey.MatchString(key) {
			continue
		}
		if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		}
		fields[key] = osReleaseUnescape.ReplaceAllString(value, "$1")
	}
	like := strings.FieldsFunc(fields["ID_LIKE"], func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	return Distribution{ID: fields["ID"], IDLike: like}
}
