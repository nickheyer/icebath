package backend

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	launcherProbeTimeout   = 15 * time.Second
	launcherProbeMaxBuffer = 1024 * 1024
)

var (
	cmdUnsafePath      = regexp.MustCompile(`["%&|<>^\r\n]`)
	reportedInstallDir = regexp.MustCompile(`(?m)^Install directory: (.+)$`)
)

// Real install directory a launcher reports for --version, empty when it reports none or the probe fails
func LauncherInstallDirectory(launcher, root string) string {
	viaCmd := runtime.GOOS == "windows" && strings.HasSuffix(strings.ToLower(launcher), ".cmd")
	if viaCmd && cmdUnsafePath.MatchString(launcher) {
		return ""
	}
	env := ProcessEnv()
	if root != "" {
		env["ICEBATH_INSTALL_ROOT"] = root
	}
	command := launcher
	if viaCmd {
		command = `"` + launcher + `"`
	}
	out, err := runVersionProbe(command, []string{"--version"}, root, env, launcherProbeTimeout, viaCmd, launcherProbeMaxBuffer)
	if err != nil {
		return ""
	}
	m := reportedInstallDir.FindStringSubmatch(out)
	if m == nil || strings.TrimSpace(m[1]) == "" {
		return ""
	}
	real, err := filepath.EvalSymlinks(strings.TrimSpace(m[1]))
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(real)
	if err != nil {
		return ""
	}
	return abs
}

// Published user bin launchers at their fixed locations, which a GUI launch PATH may omit
func userBinLaunchers(isWindows bool, icebathHome string) []string {
	names := []string{"icebath"}
	platform := "linux"
	if isWindows {
		names, platform = []string{"icebath.exe", "icebath.cmd"}, "win32"
	}
	userHome, _ := os.UserHomeDir()
	defaultHome := PlatformDefaultHermesHome(userHome, ProcessEnv(), platform)
	homeDir := icebathHome
	if homeDir == "" {
		homeDir = defaultHome
	}
	dirs := []string{filepath.Join(userHome, ".local", "bin"), filepath.Join(homeDir, "bin")}
	if isWindows {
		dirs = []string{filepath.Join(homeDir, "bin"), filepath.Join(defaultHome, "bin")}
	}
	seen := map[string]bool{}
	var out []string
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		for _, name := range names {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				out = append(out, candidate)
			}
		}
	}
	return out
}

// Install outside the canonical root that a published user bin launcher reports, empty when none does
func UserLauncherInstallRoot(isWindows bool, icebathHome string, isInstallRoot func(root string) bool) (launcher, root string) {
	for _, candidate := range userBinLaunchers(isWindows, icebathHome) {
		if reported := LauncherInstallDirectory(candidate, ""); reported != "" && isInstallRoot(reported) {
			return candidate, reported
		}
	}
	return "", ""
}
