package tools

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/term"
)

// Install and profile places of one Chromium browser
type product struct {
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

var products = []product{
	{key: "chrome", macApp: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", macSupport: []string{"Google", "Chrome"},
		winBins: []string{"chrome.exe", "chrome"}, winInstall: [][]string{{"Google", "Chrome", "Application", "chrome.exe"}},
		winProfile: []string{"Google", "Chrome", "User Data"}, linuxBins: []string{"google-chrome", "google-chrome-stable"},
		linuxPaths: []string{"/opt/google/chrome/chrome", "/usr/bin/google-chrome", "/usr/bin/google-chrome-stable"}, linuxConfig: "google-chrome"},
	{key: "chromium", macApp: "/Applications/Chromium.app/Contents/MacOS/Chromium", macSupport: []string{"Chromium"},
		winBins: []string{"chromium.exe", "chromium"}, winInstall: [][]string{{"Chromium", "Application", "chrome.exe"}, {"Chromium", "Application", "chromium.exe"}},
		winProfile: []string{"Chromium", "User Data"}, linuxBins: []string{"chromium-browser", "chromium"},
		linuxPaths: []string{"/usr/bin/chromium-browser", "/usr/bin/chromium"}, linuxConfig: "chromium"},
	{key: "brave", macApp: "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser", macSupport: []string{"BraveSoftware", "Brave-Browser"},
		winBins: []string{"brave.exe", "brave"}, winInstall: [][]string{{"BraveSoftware", "Brave-Browser", "Application", "brave.exe"}},
		winProfile: []string{"BraveSoftware", "Brave-Browser", "User Data"}, linuxBins: []string{"brave-browser", "brave-browser-stable", "brave"},
		linuxPaths: []string{"/usr/bin/brave-browser", "/usr/bin/brave-browser-stable", "/usr/bin/brave", "/snap/bin/brave",
			"/opt/brave.com/brave/brave-browser", "/opt/brave.com/brave/brave", "/opt/brave-bin/brave"},
		linuxConfig: "BraveSoftware/Brave-Browser"},
	{key: "brave-origin", macApp: "/Applications/Brave Origin.app/Contents/MacOS/Brave Origin", macSupport: []string{"BraveSoftware", "Brave-Origin"},
		winBins: []string{"brave-origin.exe", "brave-origin"}, winInstall: [][]string{{"BraveSoftware", "Brave-Origin", "Application", "brave.exe"},
			{"BraveSoftware", "Brave-Origin", "Application", "brave-origin.exe"}},
		winProfile: []string{"BraveSoftware", "Brave-Origin", "User Data"}, linuxBins: []string{"brave-origin", "brave-origin-nightly"},
		linuxPaths:  []string{"/usr/bin/brave-origin", "/opt/brave.com/brave-origin/brave-origin", "/opt/brave.com/brave-origin-nightly/brave-origin"},
		linuxConfig: "BraveSoftware/Brave-Origin", linuxExec: []string{"brave-origin"}},
	{key: "edge", macApp: "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge", macSupport: []string{"Microsoft Edge"},
		winBins: []string{"msedge.exe", "msedge"}, winInstall: [][]string{{"Microsoft", "Edge", "Application", "msedge.exe"}},
		winProfile: []string{"Microsoft", "Edge", "User Data"}, linuxBins: []string{"microsoft-edge", "microsoft-edge-stable", "msedge"},
		linuxPaths: []string{"/usr/bin/microsoft-edge", "/usr/bin/microsoft-edge-stable", "/opt/microsoft/msedge/microsoft-edge",
			"/opt/microsoft/msedge/msedge"},
		linuxConfig: "microsoft-edge", linuxExec: []string{"microsoft-edge", "microsoft-edge-stable"}},
}

// Keys of the browsers whose profile can be used, for the close-profile flag
func Browsers() []string {
	out := make([]string, len(products))
	for i, p := range products {
		out[i] = p.key
	}
	return out
}

func productOf(key string) *product {
	for i := range products {
		if products[i].key == key {
			return &products[i]
		}
	}
	return nil
}

type defaultEntry struct{ fragment, browser string }

var (
	windowsProgIDs = []defaultEntry{{"chromehtml", "chrome"}, {"msedgehtm", "edge"}, {"braveohtml", "brave-origin"}, {"bravehtml", "brave"},
		{"chromiumhtm", "chromium"}}
	windowsChannels = []string{"chromebhtml", "chromedhtml", "chromesshtml", "chromecanaryhtml", "msedgebhtml", "msedgedhtml", "msedgechtml",
		"bravebetahtml", "bravenightlyhtml", "braveobhtml", "braveodhtml", "braveoshtm"}
	linuxDesktops = []defaultEntry{{"google-chrome", "chrome"}, {"com.google.chrome", "chrome"}, {"chromium", "chromium"},
		{"brave-origin", "brave-origin"}, {"brave", "brave"}, {"microsoft-edge", "edge"}, {"com.microsoft.edge", "edge"}, {"msedge", "edge"}}
	linuxChannels = []string{"google-chrome-beta", "google-chrome-unstable", "google-chrome-canary", "com.google.chrome.beta",
		"com.google.chrome.dev", "com.google.chrome.canary", "microsoft-edge-beta", "microsoft-edge-dev", "microsoft-edge-canary",
		"brave-browser-beta", "brave-browser-nightly", "brave-browser-dev", "brave-origin-beta", "brave-origin-nightly", "brave-origin-dev"}
	flatpakIDs = map[string]string{"chrome": "com.google.Chrome", "chromium": "org.chromium.Chromium", "brave": "com.brave.Browser",
		"edge": "com.microsoft.Edge"}
	snapProfiles = map[string][]string{"chromium": {"snap", "chromium", "common", "chromium"},
		"brave": {"snap", "brave", "current", ".config", "BraveSoftware", "Brave-Browser"}}
	darwinBundles = []defaultEntry{{"com.google.chrome", "chrome"}, {"com.microsoft.edgemac", "edge"}, {"com.brave.browser", "brave"},
		{"com.brave.browser.origin", "brave-origin"}, {"org.chromium.chromium", "chromium"}}
	darwinChannels = []string{"com.google.chrome.beta", "com.google.chrome.dev", "com.google.chrome.canary", "com.microsoft.edgemac.beta",
		"com.microsoft.edgemac.dev", "com.microsoft.edgemac.canary", "com.brave.browser.beta", "com.brave.browser.nightly",
		"com.brave.browser.origin.beta", "com.brave.browser.origin.dev", "com.brave.browser.origin.nightly"}
)

// A recognized pre release channel of a Chromium browser as the default, distinct from no Chromium default
const preRelease = "__unsupported_channel__"

// Default user data folder of a browser, empty for one not known
func realDataDir(browser string) string {
	b := productOf(browser)
	if b == nil {
		return ""
	}
	h := fsx.ExpandHome("~")
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(append([]string{h, "Library", "Application Support"}, b.macSupport...)...)
	case "windows":
		local := cmp.Or(os.Getenv("LOCALAPPDATA"), filepath.Join(h, "AppData", "Local"))
		return filepath.Join(append([]string{local}, b.winProfile...)...)
	}
	configDir := cmp.Or(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(h, ".config"))
	parts := strings.Split(b.linuxConfig, "/")
	candidates := []string{filepath.Join(append([]string{configDir}, parts...)...)}
	if snap, ok := snapProfiles[browser]; ok {
		candidates = append(candidates, filepath.Join(append([]string{h}, snap...)...))
	}
	if id, ok := flatpakIDs[browser]; ok {
		candidates = append(candidates, filepath.Join(append([]string{h, ".var", "app", id, "config"}, parts...)...))
	}
	for _, c := range candidates {
		if fsx.IsDir(c) {
			return c
		}
	}
	return candidates[0]
}

func firstFile(paths []string) string {
	for _, p := range paths {
		if p != "" && fsx.IsFile(p) {
			return p
		}
	}
	return ""
}

func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// First installed program of a Chromium browser, empty when none is
func realExecutable(browser string) string {
	b := productOf(browser)
	if b == nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return firstFile([]string{b.macApp})
	case "windows":
		var candidates []string
		for _, base := range []string{envOr("PROGRAMFILES", `C:\Program Files`), envOr("PROGRAMFILES(X86)", `C:\Program Files (x86)`),
			envOr("LOCALAPPDATA", filepath.Join(fsx.ExpandHome("~"), "AppData", "Local"))} {
			for _, parts := range b.winInstall {
				candidates = append(candidates, filepath.Join(append([]string{base}, parts...)...))
			}
		}
		return firstFile(candidates)
	}
	names := b.linuxExec
	if len(names) == 0 {
		names = b.linuxBins
	}
	for _, name := range names {
		if found, err := exec.LookPath(name); err == nil {
			return found
		}
	}
	return firstFile(b.linuxPaths)
}

// Key of the browser an identifier of the default names, channels first so they fail closed
func classifyDefault(value string, channels []string, table []defaultEntry, match func(value, fragment string) bool) string {
	for _, channel := range channels {
		if match(value, channel) {
			return preRelease
		}
	}
	for _, entry := range table {
		if match(value, entry.fragment) {
			return entry.browser
		}
	}
	return ""
}

// Output of a short command, false when it can't run or takes over five seconds
func output(argv ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", false
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return "", false
	}
	return strings.ToValidUTF8(stdout.String(), "\uFFFD"), true
}

var (
	lsHTTPS     = regexp.MustCompile(`lshandlerurlscheme\s*=\s*"?https"?\s*;`)
	lsPreferred = regexp.MustCompile(`lshandlerpreferredversions\s*=\s*\{[^}]*\}\s*;`)
	lsRole      = regexp.MustCompile(`lshandlerrole(?:all|viewer)\s*=\s*"?([a-z0-9.\-]+)"?\s*;`)
)

// Bundle id a LaunchServices handlers dump registers for https, empty when none is
func httpsHandler(dump string) string {
	var entries []string
	depth := 0
	var buf []rune
	for _, ch := range dump {
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
		}
		switch {
		case ch == '{' && depth == 1:
			buf = buf[:0]
		case ch == '}' && depth == 0:
			entries = append(entries, string(buf))
		case depth >= 1:
			buf = append(buf, ch)
		}
	}
	for _, entry := range entries {
		low := strings.ToLower(entry)
		if !lsHTTPS.MatchString(low) {
			continue
		}
		low = lsPreferred.ReplaceAllString(low, "")
		for _, role := range lsRole.FindAllStringSubmatch(low, -1) {
			if role[1] != "-" {
				return role[1]
			}
		}
		return ""
	}
	return ""
}

// Key of the default Chromium browser, preRelease for a channel build, empty when the default is no Chromium
func defaultChromium() string {
	switch runtime.GOOS {
	case "windows":
		progID := cmp.Or(shellProgID("https"), registryProgID())
		if progID == "" {
			return ""
		}
		return classifyDefault(strings.ToLower(progID), windowsChannels, windowsProgIDs, strings.HasPrefix)
	case "darwin":
		out, ok := output("defaults", "read", "com.apple.LaunchServices/com.apple.launchservices.secure", "LSHandlers")
		if !ok {
			return ""
		}
		bundle := httpsHandler(out)
		if bundle == "" {
			return ""
		}
		return classifyDefault(strings.ToLower(bundle), darwinChannels, darwinBundles, func(v, f string) bool { return v == f })
	}
	out, _ := output("xdg-settings", "get", "default-web-browser")
	return classifyDefault(strings.ToLower(strings.TrimSpace(out)), linuxChannels, linuxDesktops, strings.Contains)
}

var (
	snapshotIgnores = []string{"*Cache*", "Extensions", "Extension*", "Local Extension Settings", "Service Worker", "IndexedDB", "Crash Reports",
		"Crashpad", "BrowserMetrics*", "Snapshots", "OptimizationGuide*", "optimization_guide_model_store", "Safe Browsing", "SafetyTips",
		"OnDeviceHeadSuggestModel", "segmentation_platform", "Sync Data", "Shared Dictionary", "History*", "Favicons*", "Singleton*",
		"RunningChromeVersion", "SingletonSocket", "*.tmp", "*-journal", "*-wal", "*-shm", "BrowserMetrics-spare.pma"}
	authFiles = []string{"Cookies", "Network/Cookies", "Login Data", "Login Data For Account", "Web Data", "Preferences"}
	authDBs   = []string{"Cookies", "Login Data", "Login Data For Account", "Web Data"}
	// Flags of the user's own browser run on its copy
	realFlags = []string{"--disable-hang-monitor", "--disable-popup-blocking", "--disable-prompt-on-repost"}
)

const (
	authLocked = "SQLite backup made no progress within five seconds — a running browser holds a write lock on it"
	doneMarker = ".icebath-snapshot-complete"
	// Beginning of a failure that a close of the browser holding the profile cures
	lockedPrefix = "[profile-locked] "
	realPrefix   = "browser.use_real_profile is on, but "
	// Longest backup of the session cookie database, and of the other auth databases
	sessionBackup  = 5 * time.Second
	optionalBackup = 500 * time.Millisecond
	// Longest wait for another call preparing the same profile's browser
	realLockWait = 30 * time.Second
)

// Launcher key of the profile's browser on the copy of the user's own profile
func realProfileKey(cfg *config.Config) string { return "icebath-real-profile@" + cfg.Profile() }

// Folder the profile keeps its copy of a browser's own profile in
func copyDir(cfg *config.Config, browser string) string {
	return filepath.Join(profileDir(cfg), "browser-profile", browser)
}

// Result note when the user's own profile can't load and a fresh signed out browser serves instead
func fallbackNote(failure string) string {
	return failure + " Using a fresh browser without the user's logins for this session instead; tell the user if the task needs their accounts."
}

// Profile folder the browser last used per its Local State, Default when that can't be read or is gone
func lastUsed(src string) string {
	last := "Default"
	if raw, err := os.ReadFile(filepath.Join(src, "Local State")); err == nil {
		var state struct {
			Profile struct {
				LastUsed any `json:"last_used"`
			} `json:"profile"`
		}
		if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &state) == nil {
			if name, ok := state.Profile.LastUsed.(string); ok && name != "" {
				last = name
			}
		}
	}
	if fsx.IsDir(filepath.Join(src, last)) {
		return last
	}
	return "Default"
}

// Owner only modes on a snapshot folder, or on everything inside it
func secure(path string, contents bool) {
	if runtime.GOOS == "windows" {
		return
	}
	if !contents {
		os.Chmod(path, 0o700)
		return
	}
	filepath.WalkDir(path, func(entry string, d fs.DirEntry, err error) error {
		if err != nil || entry == path || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			os.Chmod(entry, 0o700)
		} else {
			os.Chmod(entry, 0o600)
		}
		return nil
	})
}

type backupStalled struct{ msg string }

func (e *backupStalled) Error() string { return e.msg }

func fileURI(path string, query string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	slashed := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed}).String() + "?" + query, nil
}

type backuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

// Online backup of one auth database, bounded by a deadline only a stalled copy trips
func backupDB(src, dst string, deadline time.Duration) error {
	srcURI, err := fileURI(src, "mode=ro&_pragma=busy_timeout(0)")
	if err != nil {
		return err
	}
	dstURI, err := fileURI(dst, "_pragma=busy_timeout(0)")
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", srcURI)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		raw, ok := driverConn.(backuper)
		if !ok {
			return errors.New("the SQLite driver does not offer online backups")
		}
		b, err := raw.NewBackup(dstURI)
		if err != nil {
			return err
		}
		at := time.Now().Add(deadline)
		last, seen := 0, false
		for {
			more, err := b.Step(256)
			remaining, total := b.Remaining(), b.PageCount()
			before := total
			if seen {
				before = last
			}
			last, seen = remaining, true
			var code *sqlite.Error
			busy := errors.As(err, &code) && (code.Code()&0xff == 5 || code.Code()&0xff == 6)
			if err != nil && !busy {
				b.Finish()
				return err
			}
			if !more && err == nil {
				return b.Finish()
			}
			if !time.Now().Before(at) {
				b.Finish()
				if remaining < before {
					return &backupStalled{fmt.Sprintf("SQLite backup exceeded %gs while still making progress", deadline.Seconds())}
				}
				return &backupStalled{authLocked}
			}
			if busy {
				time.Sleep(100 * time.Millisecond)
			}
		}
	})
}

// Copies one auth file, answering why it could not; a database is never copied raw
func copyAuth(src, dst string, deadline time.Duration) string {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err.Error()
	}
	var err error
	if slices.Contains(authDBs, filepath.Base(src)) {
		err = backupDB(src, dst, deadline)
	} else {
		err = fsx.Copy(src, dst)
	}
	if err == nil {
		return ""
	}
	slog.Debug(fmt.Sprintf("real-profile: could not copy %s: %s", src, err))
	return cmp.Or(err.Error(), "copy failed without a reason")
}

type authFailure struct{ rel, reason string }

// Mirrors the source profile's auth files into the copy's Default, answering the session cookie databases that failed
func mirrorAuth(src, dst, source string) []authFailure {
	var failed []authFailure
	for _, rel := range authFiles {
		s := filepath.Join(src, source, filepath.FromSlash(rel))
		if !fsx.IsFile(s) {
			continue
		}
		base := filepath.Base(s)
		session := base == "Cookies"
		deadline := optionalBackup
		if session {
			deadline = sessionBackup
		}
		reason := copyAuth(s, filepath.Join(dst, "Default", filepath.FromSlash(rel)), deadline)
		if reason == "" || !slices.Contains(authDBs, base) {
			continue
		}
		if session {
			failed = append(failed, authFailure{rel, reason})
			continue
		}
		why := reason
		if reason == authLocked {
			why = "the running browser holds a write lock on it"
		}
		slog.Info(fmt.Sprintf("real-profile snapshot: skipped %s (%s); session cookies still copied", rel, why))
	}
	return failed
}

// Which session cookie databases could not be copied and why
func unavailableAuth(browser string, failed []authFailure) string {
	var names, details []string
	allLocked := true
	for _, f := range failed {
		names = append(names, f.rel)
		details = append(details, f.rel+": "+f.reason)
		allLocked = allLocked && f.reason == authLocked
	}
	if allLocked {
		return browser + " is running and holds the profile's " + strings.Join(names, ", ") + " with a write lock, so their SQLite backup made " +
			"no progress within five seconds. Icebath does not fall back to a raw file copy (it could lose committed logins). Fully quit " +
			browser + " (including any background instance) and retry, or turn browser.use_real_profile off."
	}
	return "could not read the '" + browser + "' profile's login data (" + strings.Join(details, "; ") + "). Close " + browser +
		" and retry, or turn browser.use_real_profile off."
}

// Whether the active profile's cookie database can't be opened for a permission reason
func profileLocked(src, source string) bool {
	db := firstFile([]string{filepath.Join(src, source, "Network", "Cookies"), filepath.Join(src, source, "Cookies")})
	if db == "" {
		return false
	}
	f, err := os.Open(db)
	if err != nil {
		return errors.Is(err, fs.ErrPermission)
	}
	f.Close()
	return false
}

// The pinned source profile when set, else the last used one; a pin missing on disk fails closed
func sourceProfile(cfg *config.Config, src string) (string, string) {
	pin := strings.TrimSpace(cfg.GetString(browserconfig.RealProfilePin))
	if pin == "" {
		return lastUsed(src), ""
	}
	if fsx.IsDir(filepath.Join(src, pin)) {
		return pin, ""
	}
	return "", fmt.Sprintf("browser.real_profile_pin is set to '%s' but that profile directory does not exist under %q. Profile directories "+
		"are named like 'Default' or 'Profile 2' — list them with: ls %q. Fix the pin, or remove it to fall back to the last-used profile.", pin,
		src, src)
}

var profileBins = []string{"chrome", "chrome.exe", "chromium", "chromium.exe", "chrome_crashpad", "brave", "brave.exe", "msedge", "msedge.exe",
	"google chrome"}

func fold(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(path)
	}
	return path
}

// Chromium processes whose command line names exactly this user data folder
func holdingProfile(src string) []int {
	norm := fold(src)
	var out []int
	for _, pid := range terminal.Pids() {
		argv := terminal.Cmdline(pid)
		if len(argv) == 0 {
			continue
		}
		argv0 := strings.ToLower(argv[0])
		if !slices.ContainsFunc(profileBins, func(b string) bool { return strings.Contains(argv0, b) }) {
			continue
		}
		joined := strings.Join(argv, " ")
		if strings.Contains(fold(joined), norm) || strings.Contains(strings.ToLower(joined), strings.ToLower("--user-data-dir="+src)) {
			out = append(out, pid)
		}
	}
	return out
}

// Waits until every pid is gone or the timeout passes, answering those alive
func waitGone(pids []int, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	for {
		var left []int
		for _, pid := range pids {
			if terminal.Alive(pid) {
				left = append(left, pid)
			}
		}
		if len(left) == 0 || !time.Now().Before(deadline) {
			return left
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// Ends the browser process tree holding a user data folder and waits for its profile lock to go
func closeHolding(cfg *config.Config, src string, timeout time.Duration) (bool, string) {
	procs := holdingProfile(src)
	if len(procs) == 0 {
		return false, "Your browser is not running, so there was nothing to close."
	}
	targets := slices.Clone(procs)
	for _, pid := range procs {
		targets = append(targets, term.Descendants(pid)...)
	}
	for _, pid := range targets {
		if p, err := os.FindProcess(pid); err == nil {
			ask(p)
		}
	}
	alive := waitGone(targets, min(timeout, 8*time.Second))
	for _, pid := range alive {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	}
	waitGone(alive, 3*time.Second)
	source, _ := sourceProfile(cfg, src)
	if source == "" {
		source = lastUsed(src)
	}
	for deadline := time.Now().Add(timeout); ; time.Sleep(500 * time.Millisecond) {
		if !profileLocked(src, source) {
			return true, "Closed your browser. The agent can now copy your sign-ins from it."
		}
		if !time.Now().Before(deadline) {
			return false, "Closed your browser, but its profile is still in use, perhaps because it started again in the background. Quit it from " +
				"the system tray as well, then try again."
		}
	}
}

// Ends the browser holding the profile of the browser given, else of the default one, as `icebath browser close-profile` does
func CloseProfile(cfg *config.Config, browser string) (bool, string) {
	browser = cmp.Or(browser, defaultChromium())
	if browser == "" || browser == preRelease {
		return false, "Your default browser is not a stable Chrome, Edge, Brave or Chromium, so there is no browser profile to close."
	}
	src := realDataDir(browser)
	if src == "" {
		keys := make([]string, len(products))
		for i, b := range products {
			keys[i] = b.key
		}
		return false, fmt.Sprintf("No browser is called “%s”. Name one of %s.", browser, strings.Join(keys, ", "))
	}
	return closeHolding(cfg, src, 15*time.Second)
}

// Copies Local State into the snapshot, made over for the copy's one Default profile
func syncLocalState(src, dst, source string) {
	from, to := filepath.Join(src, "Local State"), filepath.Join(dst, "Local State")
	if fsx.IsFile(from) {
		if err := fsx.Copy(from, to); err != nil {
			slog.Debug(fmt.Sprintf("real-profile snapshot: skipped Local State: %s", err))
		}
	}
	err := func() error {
		raw, err := os.ReadFile(to)
		if err != nil {
			return err
		}
		var state map[string]any
		if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &state); err != nil {
			return err
		}
		if state == nil {
			return errors.New("Local State is no JSON object")
		}
		if prof, ok := state["profile"].(map[string]any); ok {
			if cache, ok := prof["info_cache"].(map[string]any); ok {
				entry := cache[source]
				if entry == nil {
					entry = cache["Default"]
				}
				if entry != nil {
					prof["info_cache"] = map[string]any{"Default": entry}
				}
			}
			prof["last_used"] = "Default"
			prof["last_active_profiles"] = []any{"Default"}
		}
		out, err := json.MarshalIndent(state, "", " ")
		if err != nil {
			return err
		}
		return os.WriteFile(to, out, 0o600)
	}()
	if err != nil {
		slog.Debug(fmt.Sprintf("real-profile snapshot: could not normalize Local State: %s", err))
	}
}

// Refusal for a locked profile, offering the close only when browser.real_profile_autoclose arms it
func lockedError(cfg *config.Config, browser string) string {
	if cfg.GetBool(browserconfig.RealProfileAutoclose) {
		return lockedPrefix + browser + " is running and has its profile locked, so its login data can't be copied yet. Icebath can close it " +
			"for you (this quits the browser — you'll lose unsaved tabs). Ask the user to confirm, then close it and retry; if it's still locked " +
			"after that, they must fully quit it (including any background/tray instance)."
	}
	return lockedPrefix + browser + " is running and has its profile locked, so its login data can't be copied. Fully quit the browser " +
		"(including any background/tray instance) and retry, or turn browser.use_real_profile off. (Enable browser.real_profile_autoclose " +
		"to let Icebath offer to close it for you.)"
}

// Copies a folder tree, the names the ignore says skipped and a dangling link left out; the files that failed counted, not fatal
func copyTree(src, dst string, ignore func(name string) bool) (int, error) {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, err
	}
	failed := 0
	for _, entry := range entries {
		name := entry.Name()
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		if ignore(name) {
			continue
		}
		info, err := os.Stat(from)
		if err != nil {
			continue
		}
		if info.IsDir() {
			n, err := copyTree(from, to, ignore)
			failed += n
			if err != nil {
				failed++
			}
			continue
		}
		if err := fsx.Copy(from, to); err != nil {
			failed++
		}
	}
	return failed, nil
}

// Fresh copy of the active profile folder into the copy's Default, without caches and auth databases
func copyProfile(src, dst, source string) error {
	target := filepath.Join(dst, "Default")
	os.RemoveAll(target)
	patterns := append(slices.Clone(snapshotIgnores), authDBs...)
	failed, err := copyTree(filepath.Join(src, source), target, func(name string) bool {
		return slices.ContainsFunc(patterns, func(p string) bool { return tools.Glob(name, p) })
	})
	if err != nil {
		return err
	}
	if failed > 0 {
		slog.Info(fmt.Sprintf("real-profile snapshot: %d file(s) skipped copying %s/%s", failed, src, source))
	}
	return nil
}

// Drops what a live instance leaves that makes the browser think the copy is already open
func dropSingletons(dst string) {
	for _, leftover := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		os.Remove(filepath.Join(dst, leftover))
	}
}

// Copies a browser's active profile into the profile's copy folder, answering that folder or why it could not
func (r *registrant) snapshot(cfg *config.Config, browser string) (string, string) {
	src := realDataDir(browser)
	if src == "" || !fsx.IsDir(src) {
		shown := "None"
		if src != "" {
			shown = strconv.Quote(src)
		}
		return "", "profile directory for '" + browser + "' was not found (" + shown + "). Launch that browser at least once, or turn " +
			"browser.use_real_profile off."
	}
	source, failure := sourceProfile(cfg, src)
	if failure != "" || source == "" {
		return "", failure
	}
	dst := copyDir(cfg, browser)
	marker := filepath.Join(dst, doneMarker)
	populated := fsx.IsFile(marker)
	sameSource := false
	if populated {
		raw, _ := os.ReadFile(marker)
		sameSource = strings.TrimSpace(string(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))) == source
	}
	if profileLocked(src, source) {
		if sameSource {
			dropSingletons(dst)
			slog.Info(fmt.Sprintf("real-profile: %s holds its profile lock; reusing the last snapshot in %s (logins as of that copy)", browser, dst))
			return dst, ""
		}
		return "", lockedError(cfg, browser)
	}
	failed, err := func() ([]authFailure, error) {
		if populated && !sameSource {
			if err := os.Remove(marker); err != nil {
				return nil, err
			}
		}
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return nil, err
		}
		secure(filepath.Dir(dst), false)
		secure(dst, false)
		syncLocalState(src, dst, source)
		if !populated {
			if err := copyProfile(src, dst, source); err != nil {
				return nil, err
			}
		}
		if failed := mirrorAuth(src, dst, source); len(failed) > 0 {
			return failed, nil
		}
		dropSingletons(dst)
		if err := os.WriteFile(marker, []byte(source), 0o600); err != nil {
			slog.Debug(fmt.Sprintf("real-profile snapshot: could not write done marker: %s", err))
		}
		secure(dst, true)
		return nil, nil
	}()
	if err != nil {
		return "", "could not snapshot the '" + browser + "' profile into " + dst + ": " + err.Error()
	}
	if len(failed) > 0 {
		return "", unavailableAuth(browser, failed)
	}
	return dst, ""
}

// Snapshot failure, a locked profile naming the close command the user approves
func snapshotError(failure string) string {
	if strings.HasPrefix(failure, lockedPrefix) {
		return failure[len(lockedPrefix):] + " To close it (only after the user approves — it quits their browser and loses unsaved tabs), run: " +
			"`icebath browser close-profile`; the next browser session then uses the profile."
	}
	return realPrefix + failure
}

// Why the default browser's profile can't be used, empty when it can
func unsupported(browser string) string {
	switch browser {
	case "":
		return realPrefix + "your default browser is not a supported Chromium browser (Chrome, Edge, Brave, Brave Origin, Chromium). Real-profile " +
			"browsing requires a Chromium default; set one or turn the toggle off."
	case preRelease:
		return realPrefix + "your default browser is a pre-release Chromium channel (Beta / Dev / Canary), which real-profile browsing does not " +
			"support. Set your default to a stable Chrome / Edge / Brave / Brave Origin / Chromium, or turn the toggle off."
	}
	return ""
}

// One launch at a time of the profile's browser on its own profile
type realLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func (l *realLocks) of(profile string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks == nil {
		l.locks = map[string]chan struct{}{}
	}
	lock, ok := l.locks[profile]
	if !ok {
		lock = make(chan struct{}, 1)
		l.locks[profile] = lock
	}
	return lock
}

func (r *registrant) launchReal(ctx context.Context, cfg *config.Config, browser string) (string, string) {
	exe := realExecutable(browser)
	if exe == "" {
		return "", realPrefix + "the real browser binary for '" + browser + "' could not be found. Reinstall it or turn the toggle off."
	}
	var failure string
	overlay := func() string {
		copied, err := r.snapshot(cfg, browser)
		if err != "" || copied == "" {
			failure = snapshotError(err)
			return failure
		}
		return ""
	}
	if headed(cfg) {
		r.desktop.Ready(ctx, cfg)
	}
	b, err := r.launch(ctx, cfg, realProfileKey(cfg), launchOptions{exe: exe, dir: copyDir(cfg, browser), extra: realFlags, timeout: 30 * time.Second,
		prepare: overlay, userProfile: true})
	if b == nil {
		if failure != "" {
			return "", failure
		}
		return "", realPrefix + "the real-profile browser failed to start: " + err
	}
	slog.Info(fmt.Sprintf("real-profile browser ready for %s at %s (%s)", browser, b.http, b.dir))
	return b.cdp, ""
}

// Endpoint of the browser on the user's own profile, else why it can't load; both empty without consent, which stops that browser and
// deletes the copies
func (r *registrant) realProfileCDP(ctx context.Context, cfg *config.Config) (string, string) {
	if !realProfile(cfg) {
		r.stopBrowser(cfg, realProfileKey(cfg))
		root := filepath.Join(profileDir(cfg), "browser-profile")
		if fsx.IsDir(root) {
			os.RemoveAll(root)
			slog.Info(fmt.Sprintf("real-profile: removed snapshot store %s (consent off)", root))
		}
		return "", ""
	}
	if lightpanda(cfg) {
		return "", realPrefix + "browser.engine is set to 'lightpanda', which cannot load a real Chromium profile. Set browser.engine to 'auto' " +
			"to use real-profile browsing, or turn the toggle off."
	}
	lock := r.real.of(cfg.Profile())
	timer := time.NewTimer(realLockWait)
	defer timer.Stop()
	select {
	case lock <- struct{}{}:
	case <-timer.C:
		return "", realPrefix + "the real-profile browser is already being prepared by another call that has not finished. Retry after that " +
			"call completes, or restart Icebath if it was abandoned."
	}
	defer func() { <-lock }()
	if running := r.lookup(cfg, realProfileKey(cfg)); running != nil {
		return running.cdp, ""
	}
	browser := defaultChromium()
	if refused := unsupported(browser); refused != "" {
		return "", refused
	}
	return r.launchReal(ctx, cfg, browser)
}

// Names of the browsers as people know them, by key
var productTitles = map[string]string{"chrome": "Google Chrome", "chromium": "Chromium", "brave": "Brave", "brave-origin": "Brave Origin",
	"edge": "Microsoft Edge"}

// A browser installed on this computer
type Installed struct {
	Title, Path string
}

// Every Chromium browser installed on this computer, by its name and its program
func InstalledBrowsers() []Installed {
	var out []Installed
	for _, p := range products {
		if path := realExecutable(p.key); path != "" {
			out = append(out, Installed{Title: productTitles[p.key], Path: path})
		}
	}
	return out
}

// A profile of the browser your own profile is copied from
type OwnProfile struct {
	// Folder inside the browser's data, such as Default or Profile 2, and the name the browser shows for it
	Folder, Name string
	// The one the browser used last
	Last bool
}

// Title of the default browser and its profiles, those its Local State names that are there; no title for a default icebath knows not
func OwnProfiles() (string, []OwnProfile) {
	browser := defaultChromium()
	src := realDataDir(browser)
	if src == "" {
		return "", nil
	}
	last := lastUsed(src)
	var state struct {
		Profile struct {
			Cache map[string]struct {
				Name string `json:"name"`
			} `json:"info_cache"`
		} `json:"profile"`
	}
	if raw, err := os.ReadFile(filepath.Join(src, "Local State")); err == nil {
		json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &state)
	}
	var out []OwnProfile
	for folder, info := range state.Profile.Cache {
		if fsx.IsDir(filepath.Join(src, folder)) {
			out = append(out, OwnProfile{Folder: folder, Name: cmp.Or(info.Name, folder), Last: folder == last})
		}
	}
	slices.SortFunc(out, func(a, b OwnProfile) int { return strings.Compare(a.Folder, b.Folder) })
	return productTitles[browser], out
}
