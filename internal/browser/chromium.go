package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Name of the package of the Chromium icebath ships
const Chromium = "chromium"

// File the icebath image writes the path of the Chromium it staged into, for its architecture
const Baked = "/etc/icebath/chromium-executable-path"

// Distribution commands looked for only where the package manager builds no Chromium for the machine
var systemCommands = []string{"chromium", "chromium-browser"}

// Flags that start Chromium where its sandbox cannot work, shared with the Bot Desktop dock
var BypassArgs = []string{"--no-sandbox", "--disable-dev-shm-usage"}

// Program browser.executable_path pins, ~ expanded, empty for none
func Pinned(cfg *config.Config) string {
	return terminal.ExpandUser(strings.TrimSpace(cfg.GetString(browserconfig.ExecutablePath)))
}

// Profile folder browser.user_data_dir keeps for every session, ~ expanded and a relative one under the profile folder; empty for none
func ProfileDir(cfg *config.Config) string {
	raw := strings.TrimSpace(cfg.GetString(browserconfig.UserDataDir))
	if raw == "" {
		return ""
	}
	raw = terminal.ExpandUser(raw)
	if filepath.IsAbs(raw) {
		return raw
	}
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), raw)
}

// Chromium the package manager installed, empty when it has not
func (b *Browser) Managed() string {
	installed, err := b.packages.Installed(Chromium, false)
	if err != nil || installed == nil {
		return ""
	}
	return installed.Binary
}

// Whether the package manager builds no Chromium for this machine
func (b *Browser) Unbuilt() bool {
	reason, err := b.packages.Missing(Chromium)
	return err == nil && reason != ""
}

// Chromium the image of icebath staged, empty outside it
func baked() string {
	raw, err := os.ReadFile(Baked)
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	if value == "" || !fsx.IsFile(value) {
		return ""
	}
	return value
}

// Program a name is: an explicit path, ~ and variables expanded, when it is absolute and runs; else the one found on PATH; empty for none
func Locate(name string) string {
	name = strings.TrimSpace(name)
	if strings.ContainsRune(name, os.PathSeparator) || runtime.GOOS == "windows" && strings.Contains(name, "/") {
		expanded := os.ExpandEnv(terminal.ExpandUser(name))
		if filepath.IsAbs(expanded) && fsx.Executable(expanded) {
			return expanded
		}
		return ""
	}
	found, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return found
}

// Chromium a distribution installed, for a machine the package manager builds none for
func system() string {
	for _, name := range systemCommands {
		if found := Locate(name); found != "" {
			return found
		}
	}
	return ""
}

// Chromium to run: the pin when it may override, then the package manager's, then the image's, then a distribution's where the package
// manager builds none; empty when there is none
func (b *Browser) Executable(cfg *config.Config, override bool) string {
	if override {
		if pinned := Pinned(cfg); pinned != "" {
			return pinned
		}
	}
	if found := b.Managed(); found != "" {
		return found
	}
	if found := baked(); found != "" {
		return found
	}
	if b.Unbuilt() {
		return system()
	}
	return ""
}

// Chromium that runs on disk, the pin found on PATH when it is a bare name; empty when there is none
func (b *Browser) Resolved(cfg *config.Config) string {
	exe := b.Executable(cfg, true)
	if exe == "" {
		return ""
	}
	if fsx.IsFile(exe) {
		return exe
	}
	return Locate(exe)
}

// One line saying what to do on a machine without Chromium, never suggesting npm
func (b *Browser) MissingHint(cfg *config.Config) string {
	if pinned := Pinned(cfg); pinned != "" {
		return "browser.executable_path is set to " + pinned + ", which is not a browser on this machine: fix the path, or clear it to use " +
			"Icebath's Chromium."
	}
	if packages.CurrentTarget() == packages.Bionic {
		return "No Chromium found: pm builds none for Termux. Install one with `pkg install x11-repo && pkg install chromium`, or set " +
			"browser.executable_path."
	}
	if _, err := os.Stat(Baked); err == nil {
		return "The image's Chromium is missing: pull the current Icebath image and recreate the container (a volume mounted over the image's " +
			"tool store hides it)."
	}
	if b.Unbuilt() {
		return "No Chromium found: pm builds none for this platform. Install your distribution's chromium package, or set " +
			"browser.executable_path."
	}
	return "Chromium is not installed: run `icebath pm install chromium`."
}

// Whether Ubuntu's default denies unprivileged user namespaces, so a Chromium sandbox that is not setuid dies
func AppArmor() bool {
	data, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	return err == nil && strings.TrimSpace(string(data)) == "1"
}

// Whether icebath runs inside a Docker container
func Docker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	data, err := os.ReadFile("/proc/1/cgroup")
	return err == nil && strings.Contains(string(data), "docker")
}

// Whether Chromium needs its sandbox off to start here: as root, in Docker, or under the AppArmor user namespace rule
func Bypass() bool {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		return true
	}
	return Docker() || AppArmor()
}

// Whether a build cannot open a window, as Playwright's headless shell
func Headless(exe string) bool {
	return strings.Contains(strings.ToLower(filepath.Base(exe)), "headless") || strings.Contains(exe, "headless_shell")
}

// Path a profile folder resolves to, as spelled when it can't be resolved
func resolve(path string) string {
	abs, err := filepath.Abs(terminal.ExpandUser(path))
	if err != nil {
		return path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// Whether two spellings name one profile folder
func SameProfile(a, b string) bool { return resolve(a) == resolve(b) }

// Whether the process's command line names the profile folder with --user-data-dir, however either is spelled
func NamesProfile(pid int, profile string) bool {
	for _, word := range terminal.Cmdline(pid) {
		if dir, ok := strings.CutPrefix(word, "--user-data-dir="); ok && SameProfile(dir, profile) {
			return true
		}
	}
	return false
}

// Browser process, never one of its --type helpers, whose command line names the profile folder; false when none runs on it
func Holder(profile string) (int, bool) {
	for _, pid := range terminal.Pids() {
		argv := terminal.Cmdline(pid)
		if len(argv) == 0 || slices.ContainsFunc(argv, func(word string) bool { return strings.HasPrefix(word, "--type=") }) {
			continue
		}
		if NamesProfile(pid, profile) {
			return pid, true
		}
	}
	return 0, false
}

// Whether a browser runs on the profile folder
func RunningOn(profile string) bool {
	_, ok := Holder(profile)
	return ok
}
