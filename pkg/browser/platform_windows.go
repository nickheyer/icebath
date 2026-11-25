package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Browsers started directly when the shell has no handler for web addresses, found on PATH
var pathBrowsers = []string{"firefox", "chrome", "msedge", "brave", "opera", "seamonkey"}

// The shell's handler for the URL first, then Microsoft Edge from its install locations, then the browsers on PATH
func platformLaunchers() []Launcher {
	out := []Launcher{{}}
	seen := map[string]bool{}
	add := func(path string) {
		if key := strings.ToLower(filepath.Clean(path)); !seen[key] {
			seen[key] = true
			out = append(out, Launcher{Args: []string{path, "%s"}})
		}
	}
	for _, dir := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles")} {
		if dir == "" {
			continue
		}
		edge := filepath.Join(dir, "Microsoft", "Edge", "Application", "msedge.exe")
		if st, err := os.Stat(edge); err == nil && st.Mode().IsRegular() {
			add(edge)
		}
	}
	for _, name := range pathBrowsers {
		if path, err := exec.LookPath(name); err == nil {
			add(path)
		}
	}
	return out
}

// Hands a web address to the program the shell associates with it, the default browser
func systemOpen(target string) error {
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, nil, file, nil, nil, windows.SW_SHOWNORMAL)
}
