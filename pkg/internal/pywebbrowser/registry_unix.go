//go:build !windows

package pywebbrowser

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Process attributes of a browser started in its own session
var sessionAttr = &syscall.SysProcAttr{Setsid: true}

// Firefox family launcher
func newMozilla(name string) *browser {
	b := newUnix(name)
	b.remoteActionNewwin, b.remoteActionNewtab, b.hasNewtab, b.background = "-new-window", "-new-tab", true, true
	return b
}

// Epiphany launcher
func newEpiphany(name string) *browser {
	b := newUnix(name)
	b.raiseOpts, b.remoteAction, b.remoteActionNewwin, b.background = []string{"-noraise", ""}, "-n", "-w", true
	return b
}

// Elinks launcher that keeps its stdout attached
func newElinks(name string) *browser {
	b := newUnix(name)
	b.remoteArgs = []string{"-remote", "openURL(%s%action)"}
	b.remoteActionNewwin, b.remoteActionNewtab, b.hasNewtab, b.redirectStdout = ",new-window", ",new-tab", true, false
	return b
}

// KDE file manager controller
func newKonqueror(name string) *browser {
	return &browser{kind: kindKonqueror, name: name, basename: name, args: []string{"%s"}}
}

// AppleScript controller of one macOS application, default naming the system browser
func newMacOSX(name string) *browser {
	return &browser{kind: kindPlatform, name: name, basename: name, args: []string{"%s"}, launch: openMacOSX}
}

// AppleScript sent to osascript, the default browser looked up for a url that is not http or https
func openMacOSX(b *browser, url string) (bool, error) {
	url = strings.ReplaceAll(url, `"`, "%22")
	var script string
	if b.name == "default" {
		proto, _, found := strings.Cut(url, ":")
		if found && (pystr.Lower(proto) == "http" || pystr.Lower(proto) == "https") {
			script = `open location "` + url + `"`
		} else {
			script = "\n" +
				"                        use framework \"AppKit\"\n" +
				"                        use AppleScript version \"2.4\"\n" +
				"                        use scripting additions\n" +
				"\n" +
				"                        property NSWorkspace : a reference to current application's NSWorkspace\n" +
				"                        property NSURL : a reference to current application's NSURL\n" +
				"\n" +
				"                        set http_url to NSURL's URLWithString:\"https://python.org\"\n" +
				"                        set browser_url to (NSWorkspace's sharedWorkspace)'s ¬\n" +
				"                            URLForApplicationToOpenURL:http_url\n" +
				"                        set app_path to browser_url's relativePath as text -- NSURL to absolute path '/Applications/Safari.app'\n" +
				"\n" +
				"                        tell application app_path\n" +
				"                            activate\n" +
				"                            open location \"" + url + "\"\n" +
				"                        end tell\n" +
				"                    "
		}
	} else {
		script = "\n" +
			"                   tell application \"" + b.name + "\"\n" +
			"                       activate\n" +
			"                       open location \"" + url + "\"\n" +
			"                   end\n" +
			"                   "
	}
	cmd := exec.Command("/bin/sh", "-c", "/usr/bin/osascript")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(script), os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return false, err
	}
	return cmd.Wait() == nil, nil
}

// Registers one launcher per program of the list found on the path
func registerEachFound(build func(string) *browser, names ...string) {
	for _, name := range names {
		if _, ok := pyshutil.Which(name, nil); ok {
			register(name, nil, build(name), false)
		}
	}
}

// Graphical browsers of a desktop session in the stdlib preference order
func registerXBrowsers() {
	registerEachFound(newBackground, "xdg-open")
	if _, ok := pyshutil.Which("gio", nil); ok {
		register("gio", nil, newGenericList(kindBackground, []string{"gio", "open", "--", "%s"}), false)
	}
	desktop := strings.Split(os.Getenv("XDG_CURRENT_DESKTOP"), ":")
	_, gnomeSession := os.LookupEnv("GNOME_DESKTOP_SESSION_ID")
	if slices.Contains(desktop, "GNOME") || gnomeSession {
		registerEachFound(newBackground, "gvfs-open")
	}
	_, kdeSession := os.LookupEnv("KDE_FULL_SESSION")
	if slices.Contains(desktop, "KDE") || kdeSession {
		registerEachFound(newKonqueror, "kfmclient")
	}
	registerEachFound(newBackground, "x-www-browser")
	registerEachFound(newMozilla, "firefox", "iceweasel", "seamonkey", "mozilla-firefox", "mozilla")
	if _, ok := pyshutil.Which("kfm", nil); ok {
		register("kfm", nil, newKonqueror("kfm"), false)
	} else {
		registerEachFound(newKonqueror, "konqueror")
	}
	registerEachFound(newEpiphany, "epiphany")
	registerEachFound(newChrome, "google-chrome", "chrome", "chromium", "chromium-browser", "opera", "microsoft-edge")
}

// macOS application controllers or desktop browsers under a display server, then console browsers under a terminal
func registerPlatformBrowsers() {
	if runtime.GOOS == "darwin" {
		register("MacOSX", nil, newMacOSX("default"), false)
		register("chrome", nil, newMacOSX("google chrome"), false)
		register("firefox", nil, newMacOSX("firefox"), false)
		register("safari", nil, newMacOSX("safari"), false)
	} else if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		if out, err := exec.Command("xdg-settings", "get", "default-web-browser").Output(); err == nil {
			osPreferred = pystr.Strip(string(out))
		}
		registerXBrowsers()
	}
	if os.Getenv("TERM") == "" {
		return
	}
	registerEachFound(newGeneric, "www-browser", "links")
	registerEachFound(newElinks, "elinks")
	registerEachFound(newGeneric, "lynx", "w3m")
}
