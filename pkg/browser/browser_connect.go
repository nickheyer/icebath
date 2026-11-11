package browser

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Install and profile locations of one Chromium family product
type chromiumProduct struct {
	key         string
	macApp      string
	macSupport  []string
	winBins     []string
	winInstall  [][]string
	winProfile  []string
	linuxBins   []string
	linuxPaths  []string
	linuxConfig string
	linuxExec   []string
}

var chromiumProducts = []chromiumProduct{
	{key: "chrome", macApp: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		macSupport: []string{"Google", "Chrome"}, winBins: []string{"chrome.exe", "chrome"},
		winInstall:  [][]string{{"Google", "Chrome", "Application", "chrome.exe"}},
		winProfile:  []string{"Google", "Chrome", "User Data"},
		linuxBins:   []string{"google-chrome", "google-chrome-stable"},
		linuxPaths:  []string{"/opt/google/chrome/chrome", "/usr/bin/google-chrome", "/usr/bin/google-chrome-stable"},
		linuxConfig: "google-chrome"},
	{key: "chromium", macApp: "/Applications/Chromium.app/Contents/MacOS/Chromium",
		macSupport: []string{"Chromium"}, winBins: []string{"chromium.exe", "chromium"},
		winInstall:  [][]string{{"Chromium", "Application", "chrome.exe"}, {"Chromium", "Application", "chromium.exe"}},
		winProfile:  []string{"Chromium", "User Data"},
		linuxBins:   []string{"chromium-browser", "chromium"},
		linuxPaths:  []string{"/usr/bin/chromium-browser", "/usr/bin/chromium"},
		linuxConfig: "chromium"},
	{key: "brave", macApp: "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		macSupport: []string{"BraveSoftware", "Brave-Browser"}, winBins: []string{"brave.exe", "brave"},
		winInstall: [][]string{{"BraveSoftware", "Brave-Browser", "Application", "brave.exe"}},
		winProfile: []string{"BraveSoftware", "Brave-Browser", "User Data"},
		linuxBins:  []string{"brave-browser", "brave-browser-stable", "brave"},
		linuxPaths: []string{"/usr/bin/brave-browser", "/usr/bin/brave-browser-stable", "/usr/bin/brave",
			"/snap/bin/brave", "/opt/brave.com/brave/brave-browser", "/opt/brave.com/brave/brave",
			"/opt/brave-bin/brave"},
		linuxConfig: "BraveSoftware/Brave-Browser"},
	{key: "brave-origin", macApp: "/Applications/Brave Origin.app/Contents/MacOS/Brave Origin",
		macSupport: []string{"BraveSoftware", "Brave-Origin"}, winBins: []string{"brave-origin.exe", "brave-origin"},
		winInstall: [][]string{{"BraveSoftware", "Brave-Origin", "Application", "brave.exe"},
			{"BraveSoftware", "Brave-Origin", "Application", "brave-origin.exe"}},
		winProfile: []string{"BraveSoftware", "Brave-Origin", "User Data"},
		linuxBins:  []string{"brave-origin", "brave-origin-nightly"},
		linuxPaths: []string{"/usr/bin/brave-origin", "/opt/brave.com/brave-origin/brave-origin",
			"/opt/brave.com/brave-origin-nightly/brave-origin"},
		linuxConfig: "BraveSoftware/Brave-Origin", linuxExec: []string{"brave-origin"}},
	{key: "edge", macApp: "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		macSupport: []string{"Microsoft Edge"}, winBins: []string{"msedge.exe", "msedge"},
		winInstall: [][]string{{"Microsoft", "Edge", "Application", "msedge.exe"}},
		winProfile: []string{"Microsoft", "Edge", "User Data"},
		linuxBins:  []string{"microsoft-edge", "microsoft-edge-stable", "msedge"},
		linuxPaths: []string{"/usr/bin/microsoft-edge", "/usr/bin/microsoft-edge-stable",
			"/opt/microsoft/msedge/microsoft-edge", "/opt/microsoft/msedge/msedge"},
		linuxConfig: "microsoft-edge", linuxExec: []string{"microsoft-edge", "microsoft-edge-stable"}},
}

func chromiumProductByKey(key string) *chromiumProduct {
	for i := range chromiumProducts {
		if chromiumProducts[i].key == key {
			return &chromiumProducts[i]
		}
	}
	return nil
}

type defaultBrowserEntry struct{ fragment, browser string }

var windowsProgIDMap = []defaultBrowserEntry{
	{"chromehtml", "chrome"}, {"msedgehtm", "edge"},
	{"braveohtml", "brave-origin"},
	{"bravehtml", "brave"}, {"chromiumhtm", "chromium"}}

var windowsChannelProgIDs = []string{
	"chromebhtml", "chromedhtml", "chromesshtml", "chromecanaryhtml",
	"msedgebhtml", "msedgedhtml", "msedgechtml",
	"bravebetahtml", "bravenightlyhtml",
	"braveobhtml", "braveodhtml", "braveoshtm"}

var linuxDesktopMap = []defaultBrowserEntry{
	{"google-chrome", "chrome"}, {"com.google.chrome", "chrome"}, {"chromium", "chromium"},
	{"brave-origin", "brave-origin"}, {"brave", "brave"},
	{"microsoft-edge", "edge"}, {"com.microsoft.edge", "edge"}, {"msedge", "edge"}}

var linuxChannelFragments = []string{
	"google-chrome-beta", "google-chrome-unstable", "google-chrome-canary",
	"com.google.chrome.beta", "com.google.chrome.dev", "com.google.chrome.canary",
	"microsoft-edge-beta", "microsoft-edge-dev", "microsoft-edge-canary",
	"brave-browser-beta", "brave-browser-nightly", "brave-browser-dev",
	"brave-origin-beta", "brave-origin-nightly", "brave-origin-dev"}

var linuxFlatpakIDs = map[string]string{"chrome": "com.google.Chrome", "chromium": "org.chromium.Chromium",
	"brave": "com.brave.Browser", "edge": "com.microsoft.Edge"}

var linuxSnapProfileParts = map[string][]string{
	"chromium": {"snap", "chromium", "common", "chromium"},
	"brave":    {"snap", "brave", "current", ".config", "BraveSoftware", "Brave-Browser"}}

var darwinBundleMap = []defaultBrowserEntry{
	{"com.google.chrome", "chrome"}, {"com.microsoft.edgemac", "edge"},
	{"com.brave.browser", "brave"}, {"com.brave.browser.origin", "brave-origin"},
	{"org.chromium.chromium", "chromium"}}

var darwinChannelBundles = []string{
	"com.google.chrome.beta", "com.google.chrome.dev", "com.google.chrome.canary",
	"com.microsoft.edgemac.beta", "com.microsoft.edgemac.dev", "com.microsoft.edgemac.canary",
	"com.brave.browser.beta", "com.brave.browser.nightly",
	"com.brave.browser.origin.beta", "com.brave.browser.origin.dev",
	"com.brave.browser.origin.nightly"}

// Sentinel for a recognized Chromium pre release channel default, distinct from no Chromium default
const UnsupportedChannel = "__unsupported_channel__"

// Platform name of the host, the module attribute hermes tests patch
var connectSystem = pyplatform.System

func systemOr(system string) string {
	if system != "" {
		return system
	}
	return connectSystem()
}

func userHomeText() string {
	h, err := pypath.PathHome()
	if err != nil {
		return "~"
	}
	return h
}

func isFilePath(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Default user data dir of a browser on a system, empty for an unknown browser
func realProfileDataDir(browser, system string) string {
	b := chromiumProductByKey(browser)
	if b == nil {
		return ""
	}
	system = systemOr(system)
	homeDir := userHomeText()
	if system == "Darwin" {
		return pypath.PosixJoin(homeDir, append([]string{"Library", "Application Support"}, b.macSupport...)...)
	}
	if system == "Windows" {
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = pypath.NtJoin(homeDir, "AppData", "Local")
		}
		return pypath.NtJoin(local, b.winProfile...)
	}
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = pypath.PosixJoin(homeDir, ".config")
	}
	linuxParts := strings.Split(b.linuxConfig, "/")
	candidates := []string{pypath.PosixJoin(configDir, linuxParts...)}
	if parts, ok := linuxSnapProfileParts[browser]; ok {
		candidates = append(candidates, pypath.PosixJoin(homeDir, parts...))
	}
	if id, ok := linuxFlatpakIDs[browser]; ok {
		candidates = append(candidates, pypath.PosixJoin(homeDir, append([]string{".var", "app", id, "config"}, linuxParts...)...))
	}
	for _, c := range candidates {
		if isDirPath(c) {
			return c
		}
	}
	return candidates[0]
}

func firstPresent(paths []string) string {
	for _, p := range paths {
		if p != "" && isFilePath(p) {
			return p
		}
	}
	return ""
}

func connectEnvOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// First present executable of a Chromium browser on a system, empty when none is installed
func realChromiumExecutable(browser, system string) string {
	b := chromiumProductByKey(browser)
	if b == nil {
		return ""
	}
	system = systemOr(system)
	if system == "Darwin" {
		return firstPresent([]string{b.macApp})
	}
	if system == "Windows" {
		bases := []string{
			connectEnvOr("PROGRAMFILES", `C:\Program Files`),
			connectEnvOr("PROGRAMFILES(X86)", `C:\Program Files (x86)`),
			connectEnvOr("LOCALAPPDATA", pypath.PathJoin(userHomeText(), "AppData", "Local"))}
		candidates := []string{}
		for _, base := range bases {
			for _, parts := range b.winInstall {
				candidates = append(candidates, pypath.Join(base, parts...))
			}
		}
		return firstPresent(candidates)
	}
	names := b.linuxExec
	if len(names) == 0 {
		names = b.linuxBins
	}
	for _, name := range names {
		if found, ok := pyshutil.Which(name, nil); ok && found != "" {
			return found
		}
	}
	return firstPresent(b.linuxPaths)
}

// Maps an OS default browser identifier to a canonical key, channels checked first so they fail closed
func classifyDefault(value string, channels []string, table []defaultBrowserEntry, match func(value, fragment string) bool) string {
	for _, channel := range channels {
		if match(value, channel) {
			return UnsupportedChannel
		}
	}
	for _, entry := range table {
		if match(value, entry.fragment) {
			return entry.browser
		}
	}
	return ""
}

const windowsHTTPSAssoc = `Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https`

// Module attributes of the default browser detection hermes tests patch
var (
	connectWindowsShellProgID     = windowsShellProgID
	connectWindowsRegistryProgID  = windowsRegistryProgID
	connectRunStdout              = runStdout
	connectLaunchServicesHandler  = launchServicesHTTPSHandler
	connectDetectDefaultChromium  = detectDefaultChromium
	connectRealChromiumExecutable = realChromiumExecutable
	connectRealProfileDataDir     = realProfileDataDir
)

func detectDefaultWindows() string {
	progID := connectWindowsShellProgID("https")
	if progID == "" {
		progID = connectWindowsRegistryProgID()
	}
	if progID == "" {
		return ""
	}
	return classifyDefault(pystr.Lower(progID), windowsChannelProgIDs, windowsProgIDMap, strings.HasPrefix)
}

// Captured stdout of a short command, absent when it cannot run or exceeds five seconds
func runStdout(argv []string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", false
	}
	if _, exited := err.(*exec.ExitError); err != nil && !exited {
		return "", false
	}
	return pystr.DecodeUTF8Replace(stdout.Bytes()), true
}

var (
	reLSHandlerHTTPS     = regexp.MustCompile(`lshandlerurlscheme\s*=\s*"?https"?\s*;`)
	reLSPreferredVersion = regexp.MustCompile(`lshandlerpreferredversions\s*=\s*\{[^}]*\}\s*;`)
	reLSHandlerRole      = regexp.MustCompile(`lshandlerrole(?:all|viewer)\s*=\s*"?([a-z0-9.\-]+)"?\s*;`)
)

// Bundle id registered for https in a LaunchServices handlers dump, empty when none is
func launchServicesHTTPSHandler(dump string) string {
	entries := []string{}
	depth := 0
	buf := []rune{}
	for _, ch := range dump {
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
		}
		switch {
		case ch == '{' && depth == 1:
			buf = []rune{}
		case ch == '}' && depth == 0:
			entries = append(entries, string(buf))
		case depth >= 1:
			buf = append(buf, ch)
		}
	}
	for _, entry := range entries {
		low := pystr.Lower(entry)
		if !reLSHandlerHTTPS.MatchString(low) {
			continue
		}
		low = reLSPreferredVersion.ReplaceAllString(low, "")
		for _, role := range reLSHandlerRole.FindAllStringSubmatch(low, -1) {
			if role[1] != "-" {
				return role[1]
			}
		}
		return ""
	}
	return ""
}

func detectDefaultDarwin() string {
	out, ok := connectRunStdout([]string{"defaults", "read",
		"com.apple.LaunchServices/com.apple.launchservices.secure", "LSHandlers"})
	if !ok {
		return ""
	}
	bundle := connectLaunchServicesHandler(out)
	if bundle == "" {
		return ""
	}
	return classifyDefault(pystr.Lower(bundle), darwinChannelBundles, darwinBundleMap, func(value, fragment string) bool { return value == fragment })
}

func detectDefaultLinux() string {
	out, _ := connectRunStdout([]string{"xdg-settings", "get", "default-web-browser"})
	return classifyDefault(pystr.Lower(pystr.Strip(out)), linuxChannelFragments, linuxDesktopMap, strings.Contains)
}

// Canonical key of the default Chromium browser, empty when the default is not one
func detectDefaultChromium(system string) string {
	switch systemOr(system) {
	case "Windows":
		return detectDefaultWindows()
	case "Darwin":
		return detectDefaultDarwin()
	}
	return detectDefaultLinux()
}
