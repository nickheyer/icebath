package botdesktop

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Most bytes the persistent profile keeps as its HTTP cache
const diskCache = 256 << 20

// Chrome or Chromium of the system, in the order they are looked for
var systemBrowsers = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}

// Flags the agent's launch on the screen and the dock icon share: on the screen's X display even under a Wayland session, maximized
var SharedArgs = []string{"--test-type", "--ozone-platform=x11", "--start-maximized", fmt.Sprintf("--disk-cache-size=%d", diskCache)}

// Profile folder the screen's browser keeps: browser.user_data_dir, else the screen's own
func ProfileDir(cfg *config.Config) string {
	if pinned := browser.ProfileDir(cfg); pinned != "" {
		return pinned
	}
	return filepath.Join(StateDir(cfg), "browser-profile")
}

func windowed(exe string) bool { return fsx.Executable(exe) && !browser.Headless(exe) }

// Chromium the screen runs: the pinned one, then the packaged one, then the system's, the last two swapped under the AppArmor user namespace
// rule when not root; empty when there is none
func (d *Desktop) Executable(cfg *config.Config) string {
	if pinned := browser.Pinned(cfg); pinned != "" && windowed(pinned) {
		return pinned
	}
	managed := func() string {
		if exe := d.browser.Executable(cfg, false); exe != "" && windowed(exe) {
			return exe
		}
		return ""
	}
	finders := []func() string{managed, systemBrowser}
	if !root() && browser.AppArmor() {
		slices.Reverse(finders)
	}
	for _, find := range finders {
		if exe := find(); exe != "" {
			return exe
		}
	}
	return ""
}

func systemBrowser() string {
	for _, name := range systemBrowsers {
		if found, err := exec.LookPath(name); err == nil {
			return found
		}
	}
	return ""
}

// Program and profile folder of the dock's Browser icon, false when there is no Chromium
func (d *Desktop) dock(cfg *config.Config) (string, string, bool) {
	exe := d.Executable(cfg)
	if exe == "" {
		return "", "", false
	}
	return exe, ProfileDir(cfg), true
}

// Command of the dock's Browser icon, reachable through a debugging port, with the sandbox flags a host that needs them takes
func dockArgv(exe, profile string, bypass bool) []string {
	argv := append([]string{exe, "--user-data-dir=" + profile, "--remote-debugging-port=0"}, browser.QuietArgs()...)
	argv = append(argv, SharedArgs...)
	if bypass {
		argv = append(argv, browser.BypassArgs...)
	}
	return argv
}

// Exec line of the dock's desktop entry, every argument quoted and escaped as the Desktop Entry spec wants
func execLine(exe, profile string, bypass bool) string {
	var parts []string
	for _, arg := range dockArgv(exe, profile, bypass) {
		var b strings.Builder
		for _, ch := range arg {
			if strings.ContainsRune("\"`$\\", ch) {
				b.WriteByte('\\')
			}
			b.WriteRune(ch)
		}
		parts = append(parts, `"`+strings.ReplaceAll(b.String(), `\`, `\\`)+`"`)
	}
	return "Exec=" + strings.Join(parts, " ")
}
