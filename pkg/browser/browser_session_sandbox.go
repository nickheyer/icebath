package browser

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/botdesktop"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	sandboxLaunchTimeoutS  = 60.0
	sandboxConfirmedTTLS   = 30.0
	sandboxGoneRC          = 3
	sandboxNotOursRC       = 4
	sandboxStartTimeOfP    = `s=$(cat /proc/"$p"/stat 2>/dev/null) && s=${s##*")"} && set -- $s && printf %s "${20}"`
	sandboxStopScript      = `p=$1; t=$(` + sandboxStartTimeOfP + `); [ -n "$t" ] && [ "$t" = "$2" ] || exit 4; ` + `ok=; while IFS= read -r -d "" a; do [ "$a" = "$3" ] && ok=1; done < /proc/"$p"/cmdline; ` + `[ -n "$ok" ] || exit 4; kill -TERM "$p"`
	sandboxStopScriptLabel = "icebath-sandbox-browser-stop"
)

// Sandbox Chromium this process launched under one shared key, start empty when unreadable at launch
type sandboxLaunch struct {
	env        tools.Environment
	pid        int
	start, udd string
}

type sandboxConfirmation struct {
	envID              uintptr
	port, failures     int
	confirmedMonotonic float64
}

var (
	sandboxMu        sync.Mutex
	sandboxLaunched  = map[string]sandboxLaunch{}
	sandboxKeyLocks  = map[string]*sync.Mutex{}
	sandboxConfirmed = map[string]sandboxConfirmation{}
)

// Seams the sandbox browser reaches the terminal sandbox through
var (
	sandboxTerminalEnv  = botdesktop.TerminalEnvironment
	sandboxPublishedEnv = botdesktop.PublishedEnv
	sandboxUserFor      = botdesktop.UserFor
	sandboxProfileDir   = botdesktop.BrowserProfileDir
	sandboxChromiumExe  = botdesktop.SandboxChromiumExecutable
	sandboxTempDir      = tools.SandboxTempDir
	sandboxRunIn        = func(e tools.Environment, argv []string, childEnv map[string]string, user string, timeoutS float64) (*tools.RunInResult, error) {
		return tools.RunIn(context.Background(), e, argv, childEnv, user, seconds(timeoutS), nil)
	}
	sandboxForwardPort = func(e tools.Environment, port int, user string) (int, error) {
		return tools.ForwardPort(e, port, "", user)
	}
	sandboxForwardFailures = func(e tools.Environment, port int) int { return tools.ForwardFailures(e, port, "") }
)

// Identity of a sandbox environment within this process
func envIdentity(e tools.Environment) uintptr {
	v := reflect.ValueOf(e)
	if v.Kind() == reflect.Pointer {
		return v.Pointer()
	}
	return 0
}

// The terminal sandbox and its screen env, an error when either is gone
func sandboxOf(env *tools.Env) (tools.Environment, map[string]string, error) {
	e, err := sandboxTerminalEnv(env, true)
	if err != nil {
		return nil, nil, err
	}
	if e == nil {
		return nil, nil, runtimeError("the terminal backend's sandbox is not running, so there is nowhere to run the browser")
	}
	published, err := sandboxPublishedEnv(env)
	if err != nil {
		return nil, nil, err
	}
	if published["DISPLAY"] == "" {
		return nil, nil, runtimeError("the screen inside the terminal backend's sandbox is gone; start it again")
	}
	return e, published, nil
}

type devtoolsAddr struct {
	port int
	path string
}

// DevTools port and browser path from DevToolsActivePort text, false when malformed
func parseDevtoolsLines(stdout []byte) (devtoolsAddr, bool) {
	lines := pystr.Splitlines(pystr.DecodeUTF8Replace(stdout), false)
	if len(lines) < 2 || !strIsDigit(pystr.Strip(lines[0])) || !strings.HasPrefix(lines[1], "/devtools/browser/") {
		return devtoolsAddr{}, false
	}
	port, err := strconv.Atoi(pystr.Strip(lines[0]))
	if err != nil {
		return devtoolsAddr{}, false
	}
	return devtoolsAddr{port, pystr.Strip(lines[1])}, true
}

// str.isdigit of s
func strIsDigit(s string) bool {
	for _, r := range s {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

func isRunInTimeout(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "command timed out")
}

func readSandboxDevtools(e tools.Environment, user, udd string) (devtoolsAddr, bool, error) {
	proc, err := sandboxRunIn(e, []string{"cat", udd + "/DevToolsActivePort"}, nil, user, 10)
	if err != nil {
		return devtoolsAddr{}, false, err
	}
	if proc.ReturnCode != 0 {
		return devtoolsAddr{}, false, nil
	}
	addr, ok := parseDevtoolsLines(proc.Stdout)
	return addr, ok, nil
}

type liveness int

const (
	livenessUnknown liveness = iota
	livenessGone
	livenessLive
)

// One exec reporting the live DevTools address, gone when the sandbox says no browser runs, unknown when the exec failed
func liveSandboxDevtools(env *tools.Env, e tools.Environment, user, udd string) (devtoolsAddr, liveness) {
	active := pyshlex.Quote(udd + "/DevToolsActivePort")
	script := fmt.Sprintf(`[ -s %s ] || exit %d; p=$(head -n1 %s); ( exec 3<>"/dev/tcp/127.0.0.1/$p" ) 2>/dev/null || exit %d; cat %s`,
		active, sandboxGoneRC, active, sandboxGoneRC, active)
	proc, err := sandboxRunIn(e, []string{"bash", "-c", script}, nil, user, 10)
	if err != nil {
		cfgOf(env).logger().Debug(fmt.Sprintf("sandbox browser liveness probe failed: %s", pyrepr.ExceptionMessage(err)))
		return devtoolsAddr{}, livenessUnknown
	}
	if proc.ReturnCode == sandboxGoneRC {
		return devtoolsAddr{}, livenessGone
	}
	if proc.ReturnCode == 0 {
		if addr, ok := parseDevtoolsLines(proc.Stdout); ok {
			return addr, livenessLive
		}
	}
	cfgOf(env).logger().Debug(fmt.Sprintf("sandbox browser liveness probe inconclusive (exit %d)", proc.ReturnCode))
	return devtoolsAddr{}, livenessUnknown
}

// Launcher key every session on this container's Chromium shares, keyed by container never by profile
func sandboxSharedKey(e tools.Environment, udd string) string {
	return fmt.Sprintf("sandbox:%x:%s", envIdentity(e), udd)
}

// Launches Chromium detached in the sandbox, returning its sandbox pid and start time
func startSandboxChromium(env *tools.Env, e tools.Environment, user, exe, udd string, childEnv map[string]string) (int, string, error) {
	extra := append(botDesktopSharedProfileArgs(), chromiumSandboxBypassArgs...)
	argv := buildChromiumArgv(env, exe, udd, false, true, false, extra)
	seen := map[string]bool{}
	var unique []string
	for _, a := range argv {
		if !seen[a] {
			seen[a] = true
			unique = append(unique, pyshlex.Quote(a))
		}
	}
	qudd := pyshlex.Quote(udd)
	script := fmt.Sprintf(`mkdir -p %s && rm -f %s/DevToolsActivePort && (setsid %s >/dev/null 2>&1 & p=$!; echo "$p $(%s)")`,
		qudd, qudd, strings.Join(unique, " "), sandboxStartTimeOfP)
	proc, err := sandboxRunIn(e, []string{"bash", "-c", script}, childEnv, user, 30)
	if err != nil {
		return 0, "", err
	}
	var words []string
	if proc.ReturnCode == 0 {
		if lines := pystr.Splitlines(pystr.Strip(pystr.DecodeUTF8Replace(proc.Stdout)), false); len(lines) > 0 {
			words = strings.FieldsFunc(lines[len(lines)-1], pystr.IsSpace)
		}
	}
	if len(words) == 0 || !strIsDigit(words[0]) {
		stderr := []rune(pystr.DecodeUTF8Replace(proc.Stderr))
		if len(stderr) > 500 {
			stderr = stderr[len(stderr)-500:]
		}
		return 0, "", runtimeError("Chromium could not be started in the sandbox: " + string(stderr))
	}
	start := ""
	if len(words) > 1 && strIsDigit(words[1]) {
		start = words[1]
	}
	pid, _ := strconv.Atoi(words[0])
	return pid, start, nil
}

func waitForSandboxPort(e tools.Environment, user, udd string) (devtoolsAddr, error) {
	deadline := pythread.Monotonic() + sandboxLaunchTimeoutS
	for pythread.Monotonic() < deadline {
		found, ok, err := readSandboxDevtools(e, user, udd)
		if err != nil && !isRunInTimeout(err) {
			return devtoolsAddr{}, err
		}
		if ok {
			return found, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return devtoolsAddr{}, runtimeError("Chromium in the sandbox did not expose a DevTools endpoint in time")
}

func sandboxKeyLock(key string) *sync.Mutex {
	sandboxMu.Lock()
	defer sandboxMu.Unlock()
	l, ok := sandboxKeyLocks[key]
	if !ok {
		l = &sync.Mutex{}
		sandboxKeyLocks[key] = l
	}
	return l
}

// Launches the sandbox Chromium and owns it under key from the moment it exists
func spawnOwnedSandbox(env *tools.Env, e tools.Environment, user, udd, key string, published map[string]string) error {
	exe, err := sandboxChromiumExe(e)
	if err != nil {
		return err
	}
	if exe == "" {
		return runtimeError("the sandbox image has no Chromium to run the browser")
	}
	childEnv := map[string]string{}
	for k, v := range published {
		childEnv[k] = v
	}
	childEnv["TMPDIR"] = sandboxTempDir(e)
	pid, start, err := startSandboxChromium(env, e, user, exe, udd, childEnv)
	if err != nil {
		return err
	}
	sandboxMu.Lock()
	sandboxLaunched[key] = sandboxLaunch{env: e, pid: pid, start: start, udd: udd}
	sandboxMu.Unlock()
	return nil
}

// Session on the sandbox Chromium, attaching to the running one or launching it
func createSandboxSession(env *tools.Env, cacheKey string) (map[string]any, error) {
	e, published, err := sandboxOf(env)
	if err != nil {
		return nil, err
	}
	user, err := sandboxUserFor(e)
	if err != nil {
		return nil, err
	}
	udd, err := sandboxProfileDir(e)
	if err != nil {
		return nil, err
	}
	key := sandboxSharedKey(e, udd)
	lock := sandboxKeyLock(key)
	lock.Lock()
	found, state := liveSandboxDevtools(env, e, user, udd)
	if state == livenessUnknown {
		lock.Unlock()
		return nil, runtimeError("the terminal backend's sandbox did not answer whether its browser is running; try again")
	}
	localPort, err := func() (port int, err error) {
		spawned := false
		defer func() {
			if r := recover(); r != nil {
				if spawned {
					stopLaunchedSandbox(env, key)
				}
				panic(r)
			}
			if err != nil && spawned {
				stopLaunchedSandbox(env, key)
			}
		}()
		if state == livenessGone {
			if err = spawnOwnedSandbox(env, e, user, udd, key, published); err != nil {
				return 0, err
			}
			spawned = true
			if found, err = waitForSandboxPort(e, user, udd); err != nil {
				return 0, err
			}
		}
		return sandboxForwardPort(e, found.port, user)
	}()
	lock.Unlock()
	if err != nil {
		return nil, err
	}
	info := sessionRecord("sb", fmt.Sprintf("ws://127.0.0.1:%d%s", localPort, found.path), map[string]any{"local": true, "sandbox": true})
	confirmSandbox(info["cdp_url"].(string), e, found.port)
	info["shared_profile"], info["launcher_key"] = true, key
	cfgOf(env).logger().Info(fmt.Sprintf("Created sandbox browser session %s for %s", pyrepr.Str(info["session_name"]), cacheKey))
	return info, nil
}

func confirmSandbox(cdpURL string, e tools.Environment, port int) {
	failures := sandboxForwardFailures(e, port)
	sandboxMu.Lock()
	sandboxConfirmed[cdpURL] = sandboxConfirmation{envIdentity(e), port, failures, pythread.Monotonic()}
	sandboxMu.Unlock()
}

// True while the sandbox Chromium still serves the session's endpoint through the current forward
func sandboxBrowserAlive(env *tools.Env, info map[string]any) bool {
	e, err := sandboxTerminalEnv(env, false)
	if err != nil {
		panic(err)
	}
	if e == nil {
		return false
	}
	cdpURL := ""
	if pyrepr.Truthy(info["cdp_url"]) {
		cdpURL = pyrepr.Str(info["cdp_url"])
	}
	sandboxMu.Lock()
	confirmed, ok := sandboxConfirmed[cdpURL]
	sandboxMu.Unlock()
	if ok && confirmed.envID == envIdentity(e) && sandboxForwardFailures(e, confirmed.port) == confirmed.failures &&
		pythread.Monotonic()-confirmed.confirmedMonotonic < sandboxConfirmedTTLS {
		return true
	}
	user, err := sandboxUserFor(e)
	var udd string
	if err == nil {
		udd, err = sandboxProfileDir(e)
	}
	if err != nil {
		cfgOf(env).logger().Debug(fmt.Sprintf("sandbox browser liveness probe failed: %s", pyrepr.ExceptionMessage(err)))
		return true
	}
	found, state := liveSandboxDevtools(env, e, user, udd)
	if state == livenessUnknown {
		return true
	}
	gone := state == livenessGone
	if !gone {
		port, err := sandboxForwardPort(e, found.port, user)
		if err != nil {
			panic(err)
		}
		gone = cdpURL != fmt.Sprintf("ws://127.0.0.1:%d%s", port, found.path)
	}
	if gone {
		sandboxMu.Lock()
		delete(sandboxConfirmed, cdpURL)
		sandboxMu.Unlock()
		return false
	}
	confirmSandbox(cdpURL, e, found.port)
	return true
}

// SIGTERMs the sandbox Chromium this process launched on the session's shared key
func stopSandboxBrowser(env *tools.Env, info map[string]any) {
	cdpURL, key := "", ""
	if pyrepr.Truthy(info["cdp_url"]) {
		cdpURL = pyrepr.Str(info["cdp_url"])
	}
	if pyrepr.Truthy(info["launcher_key"]) {
		key = pyrepr.Str(info["launcher_key"])
	}
	sandboxMu.Lock()
	delete(sandboxConfirmed, cdpURL)
	sandboxMu.Unlock()
	lock := sandboxKeyLock(key)
	lock.Lock()
	defer lock.Unlock()
	stopLaunchedSandbox(env, key)
}

// Signals the launched browser only while its pid still carries the launch's start time and profile token
func stopLaunchedSandbox(env *tools.Env, key string) {
	sandboxMu.Lock()
	launched, ok := sandboxLaunched[key]
	delete(sandboxLaunched, key)
	sandboxMu.Unlock()
	if !ok {
		return
	}
	log := cfgOf(env).logger()
	if launched.start == "" {
		log.Warn(fmt.Sprintf("browser: leaving sandbox Chromium pid %d running: its start time could not be read at "+
			"launch, so the pid cannot be proven to still be that browser", launched.pid))
		return
	}
	user, err := sandboxUserFor(launched.env)
	var proc *tools.RunInResult
	if err == nil {
		proc, err = sandboxRunIn(launched.env, []string{"bash", "-c", sandboxStopScript, sandboxStopScriptLabel, strconv.Itoa(launched.pid),
			launched.start, "--user-data-dir=" + launched.udd}, nil, user, 15)
	}
	if err != nil {
		log.Debug(fmt.Sprintf("sandbox browser stop for pid %d failed: %s", launched.pid, pyrepr.ExceptionMessage(err)))
		return
	}
	if proc.ReturnCode == sandboxNotOursRC {
		log.Info(fmt.Sprintf("browser: not signalling sandbox pid %d: it is gone or no longer the Chromium launched on %s", launched.pid, launched.udd))
	} else if proc.ReturnCode != 0 {
		log.Debug(fmt.Sprintf("sandbox browser stop for pid %d exited %d", launched.pid, proc.ReturnCode))
	}
}
