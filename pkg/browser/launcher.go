package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Chromium startup flags for a host where its sandbox cannot work, shared with the Bot Desktop dock
var chromiumSandboxBypassArgs = []string{"--no-sandbox", "--disable-dev-shm-usage"}

var baseFlags = []string{
	"--no-first-run", "--no-default-browser-check", "--disable-background-networking",
	"--disable-component-update", "--disable-default-apps", "--disable-sync", "--disable-features=Translate",
}

var (
	packagedOnlyFlags      = []string{"--password-store=basic"}
	macosPackagedOnlyFlags = []string{"--use-mock-keychain"}
)

const (
	devtoolsFile           = "DevToolsActivePort"
	launchLog              = "icebath-chromium.log"
	minFirstLaunchTimeout  = 120 * time.Second
	unresponsiveWait       = 10 * time.Second
	launcherPollInterval   = 100 * time.Millisecond
	stderrTailChars        = 1500
	browserSharedProfiles  = "browser-shared-profiles"
	throwawayProfileSubdir = "chromium"
)

// Names defaultUserDataDir gives throwaway profiles, with owner pid, start time and namespace tag
var throwawayDirRE = regexp.MustCompile(`^[0-9a-f]{16}-(\d+)(?:-(\d+))?(-n[0-9a-f]{12})?$`)

// A Chromium this process launched or attached to on a shared profile
type LaunchedBrowser struct {
	Key         string
	PID         *int64
	StartTime   *int64
	CDPURL      string
	HTTPURL     string
	UserDataDir string
	Owned       bool
	Ephemeral   bool
	HomeKey     string
	// Launch record file name its leases are keyed by when this process did not name it
	RecordName string
	// Held from another PID namespace so the pid is never signalled and liveness is its DevTools endpoint
	Foreign bool
}

// Seams the launcher and its holders read process facts through
var (
	launcherPidAlive           = func(pid int) bool { return pid > 0 && proc.Exists(pid) }
	launcherStartTime          = func(pid int) (int64, bool) { return tools.ProcessStartTime(pid) }
	needsChromiumSandboxBypass = hostNeedsChromiumSandboxBypass
	launcherChromiumArgs       = func(env *tools.Env) []string { return cfgOf(env).chromiumArgs() }
)

func launcherLogger() *slog.Logger { return slog.Default() }

// Host facts the sandbox bypass and auto install read, seams for tests
var (
	apparmorUsernsSysctl = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
	launcherGeteuid      = os.Geteuid
	launcherInDocker     = runningInDocker
)

// Ubuntu default that denies unprivileged user namespaces so a non setuid Chromium sandbox dies
func apparmorRestrictsUnprivilegedUserns() bool {
	data, err := os.ReadFile(apparmorUsernsSysctl)
	return err == nil && strings.TrimSpace(string(data)) == "1"
}

// Whether this process runs inside a Docker container
func runningInDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	data, err := os.ReadFile("/proc/1/cgroup")
	return err == nil && strings.Contains(string(data), "docker")
}

// True when Chromium needs no-sandbox to start under root, Docker or AppArmor userns denial
func hostNeedsChromiumSandboxBypass() bool {
	if runtime.GOOS != "windows" && launcherGeteuid() == 0 {
		return true
	}
	return launcherInDocker() || apparmorRestrictsUnprivilegedUserns()
}

// Per process throwaway profile dir named by home, key, owner pid and start time and namespace tag
func defaultUserDataDir(key, homeKey string) string {
	sum := sha256.Sum256([]byte(homeKey + "\x00" + key))
	pid := os.Getpid()
	owner := fmt.Sprint(pid)
	if started, ok := launcherStartTime(pid); ok {
		owner = fmt.Sprintf("%d-%d", pid, started)
	}
	return filepath.Join(ephemeralRoot(homeKey), hex.EncodeToString(sum[:])[:16]+"-"+owner+nsTag())
}

// Launch wait, the caller's timeout or the command timeout floored at the first launch minimum
func launchTimeout(env *tools.Env, timeout *time.Duration) time.Duration {
	if timeout != nil {
		return *timeout
	}
	return max(time.Duration(cfgOf(env).commandTimeout())*time.Second, minFirstLaunchTimeout)
}

func hasDisplay(procEnv map[string]string) bool {
	return procEnv["DISPLAY"] != "" || procEnv["WAYLAND_DISPLAY"] != ""
}

// Headless unless browser.headed, and a display less Linux host always stays headless
func resolveHeaded(env *tools.Env, headed *bool, procEnv map[string]string) bool {
	want := false
	if headed != nil {
		want = *headed
	} else {
		want = cfgOf(env).headed()
	}
	if !want {
		return false
	}
	return hasDisplay(procEnv) || runtime.GOOS != "linux"
}

// The launch argv from its inputs, host sandbox facts and browser.chromium_args
func buildChromiumArgv(env *tools.Env, executable, userDataDir string, packaged, headed, sandboxBypass bool, extraArgs []string) []string {
	argv := append([]string{executable, "--remote-debugging-port=0", "--user-data-dir=" + userDataDir}, baseFlags...)
	if packaged {
		argv = append(argv, packagedOnlyFlags...)
		if runtime.GOOS == "darwin" {
			argv = append(argv, macosPackagedOnlyFlags...)
		}
	}
	if !headed {
		argv = append(argv, "--headless=new")
	}
	configured := launcherChromiumArgs(env)
	if sandboxBypass && needsChromiumSandboxBypass() {
		for _, flag := range chromiumSandboxBypassArgs {
			if !slices.Contains(configured, flag) {
				argv = append(argv, flag)
			}
		}
	}
	argv = append(argv, extraArgs...)
	argv = append(argv, configured...)
	return append(argv, "about:blank")
}

func makeBrowser(key string, port int, path string, pid, startTime *int64, userDataDir string, owned, ephemeral bool, homeKey string) *LaunchedBrowser {
	return &LaunchedBrowser{Key: key, PID: pid, StartTime: startTime, CDPURL: fmt.Sprintf("ws://127.0.0.1:%d%s", port, path),
		HTTPURL: fmt.Sprintf("http://127.0.0.1:%d", port), UserDataDir: userDataDir, Owned: owned, Ephemeral: ephemeral, HomeKey: homeKey}
}

// Where a home's throwaway profiles live, nothing outside it is ever deleted
func ephemeralRoot(homeDir string) string {
	return filepath.Join(homeDir, "cache", "browser", throwawayProfileSubdir)
}

// A home's throwaway profile launch records, read only after privateDir vouches for it
func recordsDir(homeDir string) string {
	return filepath.Join(homeDir, "cache", "browser", "chromium-records")
}

// A real dir owned by this user and closed to group and other writes, Windows checking only the dir
func isPrivateDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || ownedWithoutGroupWrite(info)
}

// The path created 0700 when missing, empty when an existing one is not private to this user
func privateDir(path string) string {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return ""
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return ""
	}
	if isPrivateDir(path) {
		return path
	}
	launcherLogger().Debug(fmt.Sprintf("browser: not using %s for launcher state: not a private directory of this user", path))
	return ""
}

// Per OS user launcher state root of shared profiles, inside the host rendezvous lock dir
func sharedProfilesRoot() string {
	return filepath.Join(gateway.GatewayLockDir(), browserSharedProfiles)
}

// One digest per profile dir however it is spelled
func profileDigest(userDataDir string) string {
	resolved, err := pypath.Realpath(userDataDir, pypath.Lax)
	if err != nil {
		resolved = userDataDir
	}
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:])
}

func sameProfile(recorded any, userDataDir string) bool {
	s, ok := recorded.(string)
	if !ok {
		return false
	}
	a, errA := pypath.Realpath(s, pypath.Lax)
	b, errB := pypath.Realpath(userDataDir, pypath.Lax)
	return errA == nil && errB == nil && a == b
}

// Entry points of the launcher module that the session, real profile and Bot Desktop layers call
var launcherEntryPoints = []any{chromiumReady, runningInstance, launchChromium, getBrowser, stopBrowser, stopAllBrowsers, reapOrphanedBrowsers}

// The DevTools port and browser path once Chromium wrote both lines, an error where Python raises ValueError
func readDevtoolsFile(userDataDir string) (int, string, bool, error) {
	raw, err := os.ReadFile(filepath.Join(userDataDir, devtoolsFile))
	if err != nil {
		return 0, "", false, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return 0, "", false, err
	}
	lines := strings.SplitAfterN(text, "\n", 3)
	for len(lines) < 2 {
		lines = append(lines, "")
	}
	port, path := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	if port == "" || strings.IndexFunc(port, func(r rune) bool { return !pystr.IsDigit(r) }) >= 0 || !strings.HasPrefix(path, "/devtools/browser/") {
		return 0, "", false, nil
	}
	n := 0
	for _, r := range port {
		if !unicode.Is(unicode.Nd, r) {
			return 0, "", false, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(port))
		}
		start := r
		for unicode.Is(unicode.Nd, start-1) {
			start--
		}
		n = n*10 + int(r-start)%10
	}
	return n, path, true, nil
}

// The last bytes of the Chromium launch log decoded with replacement
func logTail(userDataDir string) string {
	raw, err := os.ReadFile(filepath.Join(userDataDir, launchLog))
	if err != nil {
		return ""
	}
	if len(raw) > stderrTailChars {
		raw = raw[len(raw)-stderrTailChars:]
	}
	return strings.TrimSpace(pystr.DecodeUTF8Replace(raw))
}

// Hostname and pid Chromium wrote into the profile's SingletonLock symlink
func singletonOwner(userDataDir string) (string, int, bool) {
	target, err := os.Readlink(filepath.Join(userDataDir, "SingletonLock"))
	if err != nil {
		return "", 0, false
	}
	host, pidText := "", target
	if i := strings.LastIndexByte(target, '-'); i >= 0 {
		host, pidText = target[:i], target[i+1:]
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pidText == "" || strings.Trim(pidText, "0123456789") != "" {
		return "", 0, false
	}
	return host, pid, true
}

// Sorted launch record names in a records dir for one profile's browser pid from any namespace
func recordNames(records, userDataDir string, pid any) []string {
	stemText := fmt.Sprintf("%s-%s", profileDigest(userDataDir)[:16], pyrepr.Str(pid))
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(stemText) + `(-n[0-9a-f]{12})?\.json$`)
	entries, _ := os.ReadDir(records)
	var names []string
	for _, e := range entries {
		if pattern.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

var devtoolsProbeClient = &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: loopbackAwareProxy}}

// The port answers json/version with the browser id the DevToolsActivePort file names
func devtoolsEndpointLive(port int, path string) bool {
	resp, err := devtoolsProbeClient.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	decoded, err := pyjson.LoadsBytes(body)
	obj, ok := decoded.(*pyjson.Object)
	if err != nil || !ok {
		return false
	}
	ws := ""
	if v, _ := obj.Get("webSocketDebuggerUrl"); v != nil && v != false && v != "" {
		ws = pyrepr.Str(v)
	}
	return strings.HasSuffix(ws, path)
}
