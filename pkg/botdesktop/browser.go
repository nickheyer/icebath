package botdesktop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/tools"
)

// DiskCacheBytes caps the persistent profile HTTP cache
const DiskCacheBytes = 256 * 1024 * 1024

var systemBrowsers = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}

// Browser facts pkg/browser assigns at init, so this package never imports it
var (
	BrowserUserDataDir        func(env *tools.Env) string
	BrowserExecutablePath     func(env *tools.Env) string
	BrowserChromiumExecutable func(env *tools.Env, allowOverride bool) string
	BrowserAppArmorRestricts  func() bool
	BrowserNeedsSandboxBypass func() bool
	BrowserSandboxBypassArgs  func() []string
	geteuid                   = os.Geteuid
	accessX                   = accessExec
	whichBinary               = func(name string) string { p, _ := pyshutil.Which(name, nil); return p }
)

// SharedProfileArgs are the flags the agent launch and the dock icon share on the persistent profile
var SharedProfileArgs = []string{"--test-type", fmt.Sprintf("--disk-cache-size=%d", DiskCacheBytes)}

// User data dir of the bot browser, the pinned browser.user_data_dir or the screen state dir
func ProfileDir(env *tools.Env) string {
	if override := BrowserUserDataDir(env); override != "" {
		return override
	}
	return filepath.Join(StateDir(env), "browser-profile")
}

// Chromium the bot runs, pinned then packaged then system, the order flipped under the AppArmor userns rule
func Executable(env *tools.Env) string {
	explicit := BrowserExecutablePath(env)
	if explicit != "" && accessX(explicit) && !isHeadlessShell(explicit) {
		return explicit
	}
	finders := []func() string{func() string { return managedExecutable(env) }, systemExecutable}
	if !isRoot() && BrowserAppArmorRestricts() {
		finders[0], finders[1] = finders[1], finders[0]
	}
	for _, find := range finders {
		if exe := find(); exe != "" {
			return exe
		}
	}
	return ""
}

// Packaged Chromium when it is executable and can open a window
func managedExecutable(env *tools.Env) string {
	exe := BrowserChromiumExecutable(env, false)
	if exe != "" && accessX(exe) && !isHeadlessShell(exe) {
		return exe
	}
	return ""
}

// True for Playwright headless shell builds that cannot open a window
func isHeadlessShell(exe string) bool {
	return strings.Contains(strings.ToLower(filepath.Base(exe)), "headless") || strings.Contains(exe, "headless_shell")
}

// First system Chrome or Chromium on PATH
func systemExecutable() string {
	for _, name := range systemBrowsers {
		if p := whichBinary(name); p != "" {
			return whichBinary(name)
		}
	}
	return ""
}

// True when the process runs as root
func isRoot() bool { return geteuid() == 0 }

// Executable and user data dir for the dock Browser icon, ok false when no Chromium exists
func DockLaunch(env *tools.Env) (string, string, bool) {
	exe := Executable(env)
	if exe == "" {
		return "", "", false
	}
	return exe, ProfileDir(env), true
}

// Command the dock Browser icon runs, attachable through a debugging port, with the launcher sandbox policy
func DockArgv(exe, userDataDir string, sandboxBypass *bool) []string {
	argv := []string{exe, "--user-data-dir=" + userDataDir, "--remote-debugging-port=0", "--no-first-run", "--no-default-browser-check"}
	argv = append(argv, SharedProfileArgs...)
	bypass := false
	if sandboxBypass == nil {
		bypass = BrowserNeedsSandboxBypass()
	} else {
		bypass = *sandboxBypass
	}
	if bypass {
		argv = append(argv, BrowserSandboxBypassArgs()...)
	}
	return argv
}

// Exec line of the dock desktop entry with every argument quoted and escaped per the Desktop Entry spec
func DockExecLine(exe, userDataDir string, sandboxBypass *bool) string {
	quote := func(arg string) string {
		var b strings.Builder
		for _, ch := range arg {
			if strings.ContainsRune("\"`$\\", ch) {
				b.WriteByte('\\')
			}
			b.WriteRune(ch)
		}
		return `"` + strings.ReplaceAll(b.String(), `\`, `\\`) + `"`
	}
	parts := []string{}
	for _, arg := range DockArgv(exe, userDataDir, sandboxBypass) {
		parts = append(parts, quote(arg))
	}
	return "Exec=" + strings.Join(parts, " ")
}
