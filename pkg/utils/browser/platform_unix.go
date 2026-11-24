//go:build unix && !darwin

package browserutil

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Graphical browsers started directly when no desktop opener is installed
var desktopBrowsers = []string{"firefox", "google-chrome", "chromium", "chromium-browser", "brave-browser", "microsoft-edge"}

// Openers of a Linux or BSD host found on PATH: Termux's, then from WSL the Windows URL handler (rundll32 first, which takes the URL without any shell or cmd.exe reading it), then the desktop's under a display server
func platformLaunchers() []Launcher {
	var out []Launcher
	add := func(args ...string) {
		if _, err := exec.LookPath(args[0]); err == nil {
			out = append(out, Launcher{Args: args})
		}
	}
	if os.Getenv("TERMUX_VERSION") != "" || strings.Contains(os.Getenv("PREFIX"), "com.termux") {
		add("termux-open-url", "%s")
	}
	if insideWSL() {
		add("rundll32.exe", "url.dll,FileProtocolHandler", "%s")
		add("wslview", "%s")
	}
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		add("xdg-open", "%s")
		add("gio", "open", "%s")
		add("gvfs-open", "%s")
		add("kde-open", "%s")
		add("kde-open5", "%s")
		add("exo-open", "%s")
		add("x-www-browser", "%s")
		for _, name := range desktopBrowsers {
			add(name, "%s")
		}
	}
	return out
}

// Whether the kernel names itself a WSL kernel, read once
var wslKernel = sync.OnceValue(func() bool {
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(release)), "microsoft")
})

// Whether this process runs inside the Windows Subsystem for Linux
func insideWSL() bool {
	return os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" || wslKernel()
}

// No operating system URL handler outside Windows, every opener here is a command
func systemOpen(string) error { return errors.ErrUnsupported }
