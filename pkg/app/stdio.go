package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var (
	stdioMu         sync.Mutex
	stdioConfigured bool
)

// Reports native Windows, never WSL
func IsWindows() bool { return runtime.GOOS == "windows" }

// Forces UTF-8 stdio on Windows once, true only when something changed, ICEBATH_DISABLE_WINDOWS_UTF8 opts out
func ConfigureWindowsStdio() bool {
	stdioMu.Lock()
	defer stdioMu.Unlock()
	if stdioConfigured {
		return false
	}
	if !IsWindows() || windowsUTF8Disabled() {
		stdioConfigured = true
		return false
	}
	setenvDefault("PYTHONIOENCODING", "utf-8")
	setenvDefault("PYTHONUTF8", "1")
	if editor := defaultWindowsEditor(); editor != "" && os.Getenv("EDITOR") == "" && os.Getenv("VISUAL") == "" {
		os.Setenv("EDITOR", editor)
	}
	augmentPathWithKnownTools()
	flipConsoleCodePageToUTF8()
	reconfigureStream()
	stdioConfigured = true
	return true
}

// Reports the ICEBATH_DISABLE_WINDOWS_UTF8 opt out values 1, true, True and yes
func windowsUTF8Disabled() bool {
	switch os.Getenv("ICEBATH_DISABLE_WINDOWS_UTF8") {
	case "1", "true", "True", "yes":
		return true
	}
	return false
}

// Sets an environment variable only while the process has none of that name
func setenvDefault(name, value string) {
	if _, ok := os.LookupEnv(name); !ok {
		os.Setenv(name, value)
	}
}

// Bare notepad when it is on PATH, empty on installs without it
func defaultWindowsEditor() string {
	if _, err := exec.LookPath("notepad"); err == nil {
		return "notepad"
	}
	return ""
}

// Prepends the installer managed tool directories under LOCALAPPDATA that PATH lacks, nothing off Windows
func augmentPathWithKnownTools() {
	if !IsWindows() {
		return
	}
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return
	}
	candidates := []string{
		filepath.Join(localAppData, "icebath", "git", "cmd"),
		filepath.Join(localAppData, "icebath", "git", "bin"),
		filepath.Join(localAppData, "icebath", "git", "usr", "bin"),
		filepath.Join(localAppData, "icebath", "icebath-agent", "venv", "Scripts"),
		filepath.Join(localAppData, "Microsoft", "WinGet", "Links"),
	}
	existing := os.Getenv("PATH")
	existingLower := map[string]bool{}
	for _, p := range strings.Split(existing, string(os.PathListSeparator)) {
		if p != "" {
			existingLower[strings.ToLower(p)] = true
		}
	}
	var prepend []string
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() && !existingLower[strings.ToLower(dir)] {
			prepend = append(prepend, dir)
		}
	}
	if len(prepend) > 0 {
		os.Setenv("PATH", strings.Join(append(prepend, existing), string(os.PathListSeparator)))
	}
}
