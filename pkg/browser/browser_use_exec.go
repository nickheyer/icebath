package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	browserUseProvider          = "browser-use"
	privateBrowserSentinel      = "_ICEBATH_BU_PRIVATE_BROWSER"
	botDesktopBrowserSentinel   = "_ICEBATH_BU_BOT_DESKTOP_BROWSER"
	realProfileFallbackSentinel = "_ICEBATH_BU_REAL_PROFILE_FALLBACK"
	localFallbackSentinel       = "_ICEBATH_BU_LOCAL_FALLBACK"
	cloudBrowserSentinel        = "_ICEBATH_BU_CLOUD_BROWSER"
	execStderrCapChars          = 4000
	ownDuringCall               = 500 * time.Millisecond
	harnessDaemonPurpose        = "browser-harness-daemon"
	fenceTakeoverPrefix         = "A human took over the bot's screen while this browser command ran"
)

// Lowest timeout a call may ask for and the bounded drain after a group kill, swapped by tests
var (
	execMinTimeoutS = 5
	postKillDrain   = 10 * time.Second
	killCLIGroup    = killCLIProcessGroup
)

// The one credential the harness itself reads
var harnessPassthroughKeys = []string{"BROWSER_USE_API_KEY"}

const execPySpace = `\t\n\v\f\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	execSessionRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}\n?$`)
	execTaskIDSafe  = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	execImagePathRE = regexp.MustCompile(`(?i)((?:[A-Za-z]:[\\/]|/)[^` + execPySpace + `"']+?\.(?:png|jpe?g|webp))`)
	execURLRE       = regexp.MustCompile(`(?i)https?://[^` + execPySpace + `'"\\)]+`)
	fhsBinDirs      = []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin",
		"/usr/bin", "/sbin", "/bin"}
)

const ownTabPreamble = `# icebath: pin this named session to its own tab (once per daemon process)
def _icebath_ensure_own_tab():
    import os as _os, tempfile as _tf
    _name = _os.environ.get("BU_NAME", "default")
    try:
        # Key the marker by the daemon's pid so a daemon restart (which
        # re-attaches to the first shared page) re-pins automatically,
        # while agent-driven tab switches mid-session are left alone.
        from browser_harness import _ipc as _bipc
        _dpid = _bipc.pid_path(_name).read_text().strip() or "0"
    except Exception:
        _dpid = "0"
    _uid = _os.getuid() if hasattr(_os, "getuid") else 0
    _marker = _os.path.join(
        _tf.gettempdir(), "icebath-bu-owntab-%s-%s-%s" % (_uid, _name, _dpid)
    )
    if _os.path.exists(_marker):
        return
    try:
        # Force a fresh target: new_tab() would REUSE a blank current tab,
        # which is exactly the tab a sibling daemon may also hold.
        _tid = cdp("Target.createTarget", url="about:blank").get("targetId")
        if _tid:
            switch_tab(_tid)
    except Exception:
        pass  # worst case is pre-fix behavior
    try:
        open(_marker, "w").close()
    except OSError:
        pass
_icebath_ensure_own_tab()
del _icebath_ensure_own_tab
`

// Seams tests replace, the session layer, provider lookup, supervisor attach and browser child env
var (
	execSessionInfo      = getSessionInfo
	execCloudProvider    = getCloudProvider
	execAttachSupervisor = supervisorGetOrStart
	execBrowserEnv       = buildBrowserEnv
	execRealProfileCDP   = func(env *tools.Env) (string, string) { return realProfileCDP(env) }
	execLiveGeneration   = func(env *tools.Env) int { return LiveCDPGeneration(cfgOf(env).homeKey()) }
	execCDPOverride      = func(ctx context.Context, env *tools.Env) string { return cfgOf(env).cdpOverride(ctx) }
	execUsingLightpanda  = usingLightpandaEngine
	execNativeVision     = tools.ShouldUseNativeVisionFastPath
	execResizeForVision  = tools.ResizeImageForVision
)

// Route bookkeeping across browser_exec calls, keyed by route cache key and daemon identity
var (
	execStateMu              sync.Mutex
	execRouteIsSidecar       = map[string]bool{}
	realProfileFallbackNotes = map[string]string{}
	realProfileNoteDelivered = map[string]bool{}
	drivenDaemonsMu          sync.Mutex
	drivenDaemons            = map[string]drivenDaemon{}
	ownedDaemonGroupsMu      sync.Mutex
	ownedDaemonGroups        = map[string]int{}
)

type drivenDaemon struct {
	identity []string
	home     string
}

func identityKey(identity []string) string { return strings.Join(identity, "\x00") }

// Exports a CDP endpoint as BU_CDP_URL for http and https, as BU_CDP_WS otherwise
func setCDPEnv(penv map[string]string, cdp string) {
	if strings.HasPrefix(cdp, "http://") || strings.HasPrefix(cdp, "https://") {
		penv["BU_CDP_URL"] = cdp
		return
	}
	penv["BU_CDP_WS"] = cdp
}

func hasCDPEnv(penv map[string]string) bool {
	return penv["BU_CDP_WS"] != "" || penv["BU_CDP_URL"] != ""
}

// Exports the session's CDP endpoint, the failure or missing endpoint message otherwise
func exportSessionCDP(penv map[string]string, info func(string) (map[string]any, error), key string,
	failMsg func(error) string, noCDPMsg string) string {
	got, err := info(key)
	if err != nil {
		return failMsg(err)
	}
	cdp := ""
	if v := got["cdp_url"]; pyrepr.Truthy(v) {
		cdp = pyrepr.Str(v)
	}
	if cdp == "" {
		return noCDPMsg
	}
	setCDPEnv(penv, cdp)
	return ""
}

// Blocking error of the first unsafe URL literal and whether any literal routes to the local sidecar
func codeURLVerdict(env *tools.Env, code string) (string, bool, error) {
	localSidecar := false
	for _, u := range execURLRE.FindAllString(code, -1) {
		routesLocal, err := hybridRoutesLocally(env, u)
		if err != nil {
			return "", false, err
		}
		localSidecar = localSidecar || routesLocal
		if blocked := evaluateURLSafety(env, u, routesLocal); blocked != nil {
			msg := "Blocked: unsafe URL"
			if v, ok := blocked.Get("error"); ok {
				msg = pyrepr.Str(v)
			}
			return msg, localSidecar, nil
		}
	}
	return "", localSidecar, nil
}

// Harness credential from the active profile scope, never the launch profile's process environment
func harnessCredentials(ctx context.Context, env *tools.Env) map[string]string {
	out := map[string]string{}
	routed := provider.ServesRoutedProfile(ctx)
	scope := provider.CurrentSecretScope(ctx)
	for _, key := range harnessPassthroughKeys {
		var value string
		var ok bool
		if routed {
			value, ok = scope[key]
		} else {
			value, ok = cfgOf(env).secretLookup(key)
		}
		if ok {
			out[key] = value
		}
	}
	return out
}

// Secret as get_secret returns it, absent when neither the home's env nor the process sets it
func (s settings) secretLookup(name string) (string, bool) {
	if s.env != nil && s.env.Secrets != nil {
		return s.env.Secrets.Get(name)
	}
	return os.LookupEnv(name)
}

// Harness child env with its own import path, a floored PATH, telemetry off, served profile dirs and a spawn tag
func baseSubprocessEnv(ctx context.Context, env *tools.Env) map[string]string {
	out := maps.Clone(execBrowserEnv(env))
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, harnessCredentials(ctx, env))
	delete(out, "PYTHONHOME")
	if site := harnessSiteDir(); site != "" {
		out["PYTHONPATH"] = site
	} else {
		delete(out, "PYTHONPATH")
	}
	out["PATH"] = floorSubprocessPath(out["PATH"])
	if _, ok := out["ANONYMIZED_TELEMETRY"]; !ok {
		out["ANONYMIZED_TELEMETRY"] = "false"
	}
	namespaceHarnessDirs(out, servedProfileTag(ctx))
	maps.Copy(out, tools.SpawnEnv(harnessDaemonPurpose, ""))
	return out
}

// PATH with the standard system bin dirs appended when missing, unchanged on Windows
func floorSubprocessPath(path string) string {
	if runtime.GOOS == "windows" {
		return path
	}
	sep := string(os.PathListSeparator)
	parts := []string{}
	for _, p := range strings.Split(path, sep) {
		if p != "" {
			parts = append(parts, p)
		}
	}
	original := slices.Clone(parts)
	for _, d := range fhsBinDirs {
		if !slices.Contains(original, d) && isDirPath(d) {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, sep)
}

func isDirPath(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func browserUseAPIKeySet(env *tools.Env) bool {
	return cfgOf(env).rawSecret("BROWSER_USE_API_KEY") != ""
}

const harnessSpecProbe = "import importlib.util\nspec = importlib.util.find_spec('browser_harness')\n" +
	"print(spec.origin if spec is not None and spec.origin else '')\n"

var harnessProbeCache struct {
	sync.Mutex
	key    string
	python string
	site   string
	ok     bool
}

// Repo root, isolated interpreter command and site-packages of the runtime environment, swapped by tests
var harnessRuntime = func(ctx context.Context) (string, []string, string, error) {
	root := pm.RepoRoot()
	venv, err := pm.SelectedVenv(ctx, root)
	if err != nil {
		return "", nil, "", err
	}
	command, err := pm.VenvCommand(ctx, root, venv, []string{"-I"})
	if err != nil {
		return "", nil, "", err
	}
	sitePackages, _ := pm.SitePackages(venv)
	return root, command, sitePackages, nil
}

// Runtime interpreter and the site dir it imports browser_harness from, memoized until site-packages changes
func probeHarness(ctx context.Context) (string, string) {
	root, command, sitePackages, err := harnessRuntime(ctx)
	if err != nil || len(command) == 0 || !isRegularFile(command[0]) {
		return "", ""
	}
	stamp := ""
	if st, err := os.Stat(sitePackages); err == nil {
		stamp = st.ModTime().String()
	}
	key := identityKey(append(slices.Clone(command), stamp))
	harnessProbeCache.Lock()
	defer harnessProbeCache.Unlock()
	if harnessProbeCache.ok && harnessProbeCache.key == key {
		return harnessProbeCache.python, harnessProbeCache.site
	}
	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c := exec.CommandContext(probeCtx, command[0], append(slices.Clone(command[1:]), "-c", harnessSpecProbe)...)
	c.Dir = root
	out, err := c.Output()
	if err != nil {
		return command[0], ""
	}
	site := ""
	if origin := pystr.Strip(string(out)); origin != "" {
		if resolved, err := pypath.Resolve(origin); err == nil {
			site = filepath.Dir(filepath.Dir(resolved))
		}
	}
	harnessProbeCache.key, harnessProbeCache.python, harnessProbeCache.site, harnessProbeCache.ok = key, command[0], site, true
	return command[0], site
}

// Site dir the runtime interpreter imports browser_harness from, empty when it is not installed
var harnessSiteDir = func() string {
	_, site := probeHarness(context.Background())
	return site
}

// The runtime interpreter that runs the harness
var harnessInterpreter = func() string {
	python, _ := probeHarness(context.Background())
	return python
}

// The harness module command on the runtime interpreter, nil when browser_harness is not importable
var findCLI = func(env *tools.Env) []string {
	if harnessSiteDir() == "" {
		return nil
	}
	return []string{harnessInterpreter(), "-m", "browser_harness.run"}
}

// Per task workspace dir persisting across calls, BH_AGENT_WORKSPACE when the process sets it
func workspaceDir(env *tools.Env, taskID string) string {
	if v := os.Getenv("BH_AGENT_WORKSPACE"); v != "" {
		return v
	}
	if taskID == "" {
		taskID = "default"
	}
	safe := execTaskIDSafe.ReplaceAllString(taskID, "_")
	if r := []rune(safe); len(r) > 80 {
		safe = string(r[:80])
	}
	if safe == "" {
		safe = "default"
	}
	path := filepath.Join(cfgOf(env).homeDir(), "cache", "browser-use", "workspace", safe)
	if err := os.MkdirAll(path, 0o777); err != nil {
		slog.Default().Debug(fmt.Sprintf("browser_exec workspace unavailable: %s", pypath.OSErrorText(err)))
		return ""
	}
	return path
}

// Last screenshot path printed during this exec that exists and was written after it started
func findScreenshot(stdout string, since float64) string {
	matches := execImagePathRE.FindAllStringSubmatch(stdout, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		path := matches[i][1]
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if float64(st.ModTime().UnixNano())/1e9 >= since-1 {
			return path
		}
	}
	return ""
}

// Multimodal result attaching the screenshot for a native vision model, empty otherwise
func nativeScreenshotResult(ctx context.Context, env *tools.Env, result *pyjson.Object, path string) string {
	if !execNativeVision(ctx, env) {
		return ""
	}
	dataURL, err := execResizeForVision(env, path, "image/png", tools.ResizeOptions{
		MaxBase64Bytes: tools.ResolveEmbedTargetBytes(env), MaxDimension: tools.VisionEmbedMaxDimension, ForceJPEG: true})
	if err != nil {
		slog.Default().Debug(fmt.Sprintf("Native screenshot attach failed (falling back to text): %s", err))
		return ""
	}
	text, err := pyjson.Dumps(result, pyjson.EnsureASCII(false))
	if err != nil {
		slog.Default().Debug(fmt.Sprintf("Native screenshot attach failed (falling back to text): %s", err))
		return ""
	}
	attached := text + "\n\nThe screenshot from this call is attached — inspect it with your native vision."
	return tools.ToolResult(pyjson.ObjectOf(
		"_multimodal", true, "text_summary", text,
		"meta", pyjson.ObjectOf("screenshot_path", path, "native_vision", true),
		"content", []any{
			pyjson.ObjectOf("type", "text", "text", attached),
			pyjson.ObjectOf("type", "image_url", "image_url", pyjson.ObjectOf("url", dataURL)),
		}))
}

// Home key of a served profile under a home override, empty outside one
func servedProfileTag(ctx context.Context) string {
	override := home.Override(ctx)
	if override == "" {
		return ""
	}
	return home.Key(override)
}

// Session cache key of a backend browser, per name, per served profile, with the local sidecar suffix
func backendCacheKey(ctx context.Context, taskID, sessionName string, sidecar bool) string {
	key := taskID
	if sessionName != "" {
		key = "bu-named-" + sessionName
	} else if key == "" {
		key = "browser-exec-default"
	}
	if tag := servedProfileTag(ctx); tag != "" {
		key += "@" + tag
	}
	if sidecar {
		key += localSuffix
	}
	return key
}

// BU_NAME of the daemon serving the hybrid local sidecar
func sidecarDaemonName(name string) string {
	if name == "" {
		name = "default"
	}
	out := []rune("_local_" + name)
	if len(out) > 64 {
		out = out[:64]
	}
	return string(out)
}

// Carries the session layer's real profile fallback note for the key
func exportRealProfileNote(penv map[string]string, key string) {
	note := realProfileNote(key)
	if !pyrepr.Truthy(note) {
		return
	}
	if _, ok := penv[realProfileFallbackSentinel]; !ok {
		penv[realProfileFallbackSentinel] = pyrepr.Str(note)
	}
}

// Flags a local session browser for the desktop fence and as exclusive unless its profile is shared
func markLocalBrowser(penv map[string]string, session map[string]any, key string) {
	features, _ := session["features"].(map[string]any)
	if pyrepr.Truthy(features["lightpanda"]) {
		penv[botDesktopBrowserSentinel] = "lightpanda"
	} else {
		penv[botDesktopBrowserSentinel] = "local"
	}
	if !pyrepr.Truthy(session["shared_profile"]) && !pyrepr.Truthy(features["real_profile"]) {
		penv[privateBrowserSentinel] = "1"
	}
	exportRealProfileNote(penv, key)
}

// Session info reader merging every answer into one record the caller inspects afterwards
func mergingSessionInfo(env *tools.Env, session map[string]any) func(string) (map[string]any, error) {
	return func(key string) (map[string]any, error) {
		got, err := execSessionInfo(env, key)
		if err != nil {
			return nil, err
		}
		maps.Copy(session, got)
		return session, nil
	}
}

// Local browser of the cache key through the session layer, the error message when it cannot start
func resolveLocalCDP(ctx context.Context, env *tools.Env, penv map[string]string, taskID, sessionName string, sidecar bool) string {
	key := backendCacheKey(ctx, taskID, sessionName, sidecar)
	session := map[string]any{}
	fail := func(e error) string {
		return fmt.Sprintf("The local browser could not be started: %s "+
			"Run `icebath tools` → Browser Automation to (re)install Chromium, or switch backends.", e)
	}
	if execUsingLightpanda(env) {
		fail = func(e error) string {
			return fmt.Sprintf("Lightpanda could not be started: %s Set browser.engine to auto "+
				"to use local Chrome, or switch backends via `icebath tools` → Browser Automation.", e)
		}
	}
	err := exportSessionCDP(penv, mergingSessionInfo(env, session), key, fail, "The local browser session returned no CDP endpoint.")
	if err == "" {
		markLocalBrowser(penv, session, key)
	}
	return err
}

// Points the harness at the configured backend, operator overrides first, then sidecar, provider and local engine
func resolveBackendCDP(ctx context.Context, env *tools.Env, penv map[string]string, taskID, sessionName string, forceLocalSidecar bool) string {
	if hasCDPEnv(penv) {
		return ""
	}
	if override := execCDPOverride(ctx, env); override != "" {
		setCDPEnv(penv, override)
		return ""
	}
	if forceLocalSidecar {
		return resolveLocalCDP(ctx, env, penv, taskID, sessionName, true)
	}
	prov, perr := execCloudProvider(env)
	if perr != nil {
		slog.Default().Debug(fmt.Sprintf("Cloud provider lookup failed: %s", perr))
		prov = nil
	}
	if prov == nil {
		return resolveLocalCDP(ctx, env, penv, taskID, sessionName, false)
	}
	providerKey := strings.ToLower(pystr.Strip(prov.Name()))
	if providerKey == browserUseProvider && !cfgOf(env).useGateway() && browserUseAPIKeySet(env) {
		penv[privateBrowserSentinel] = "1"
		penv[cloudBrowserSentinel] = activateIfCloud
		return ""
	}
	providerName := providerClassName(prov)
	session := map[string]any{}
	key := backendCacheKey(ctx, taskID, sessionName, false)
	err := exportSessionCDP(penv, mergingSessionInfo(env, session), key,
		func(e error) string {
			return fmt.Sprintf("Cloud browser provider %s failed to provide a session: %s. "+
				"Fix the provider configuration or switch backends via `icebath tools` → Browser Automation.", providerName, e)
		},
		fmt.Sprintf("Cloud browser provider %s returned no CDP endpoint, so browser_exec "+
			"cannot drive it. Pick a CDP-capable provider via `icebath tools` → Browser Automation.", providerName))
	if pyrepr.Truthy(session["fallback_from_cloud"]) {
		slog.Default().Warn(fmt.Sprintf("browser_exec: %s failed (%s); using local Chromium for this session",
			providerName, pyrepr.Str(session["fallback_reason"])))
		if err == "" {
			markLocalBrowser(penv, session, key)
		}
		return err
	}
	if err == "" {
		penv[cloudBrowserSentinel] = activateAlways
	}
	if err == "" && sessionName != "" {
		penv[privateBrowserSentinel] = "1"
	}
	return err
}

// Points the harness at the consented real profile browser when the backend is local or local was forced
func resolveRealProfileCDP(env *tools.Env, penv map[string]string, forceLocal bool) string {
	if !realProfileConsented(env) || hasCDPEnv(penv) {
		return ""
	}
	if cfgOf(env).cdpOverrideRaw() != "" {
		return ""
	}
	if !forceLocal {
		prov, err := execCloudProvider(env)
		if err != nil || prov != nil || isLegacyBrowserUseCloudConfig(env) {
			return ""
		}
	}
	cdp, err := execRealProfileCDP(env)
	if cdp != "" && err == "" {
		setCDPEnv(penv, cdp)
		penv[botDesktopBrowserSentinel] = "local"
	}
	return err
}

// Attaches the task's CDP supervisor to the browser this exec drives so vault fills share it
func attachVaultSupervisor(ctx context.Context, env *tools.Env, penv map[string]string, taskID string) {
	cdp := penv["BU_CDP_WS"]
	if cdp == "" {
		cdp = penv["BU_CDP_URL"]
	}
	if cdp == "" {
		return
	}
	if taskID == "" {
		taskID = "default"
	}
	policy, timeoutS := cfgOf(env).dialogPolicyConfig()
	if err := execAttachSupervisor(env, taskID, ResolveCDPOverride(ctx, cdp), policy, timeoutS); err != nil {
		slog.Default().Debug(fmt.Sprintf("browser_exec: CDP supervisor attach failed (non-fatal): %s", err))
	}
}

// Where the harness connects, real profile consent first, the error message when it cannot
func routeBackend(ctx context.Context, env *tools.Env, penv map[string]string, session, taskID string, local, forceLocalSidecar, realProfile bool) string {
	rpErr := ""
	if realProfile {
		rpErr = resolveRealProfileCDP(env, penv, local)
	}
	if local && !hasCDPEnv(penv) && !realProfileConsented(env) {
		return "local=true was requested but browser.use_real_profile is off. Enable it in config.yaml " +
			"(browser.use_real_profile: true) or the desktop Settings → Browser section, then retry."
	}
	if local && !hasCDPEnv(penv) && (rpErr != "" || !realProfile) {
		if rpErr != "" {
			penv[realProfileFallbackSentinel] = realProfileFallbackNote(rpErr)
		}
		penv[localFallbackSentinel] = "1"
		forceLocalSidecar = true
	}
	return resolveBackendCDP(ctx, env, penv, taskID, session, forceLocalSidecar)
}

// Python int of a value, false where int raises TypeError or ValueError
func pyIntOf(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case float64:
		if math.IsNaN(x) {
			return nil, false
		}
		if math.IsInf(x, 0) {
			panic(pyError("OverflowError", "cannot convert float infinity to integer"))
		}
		n, _ := big.NewFloat(math.Trunc(x)).Int(nil)
		return n, true
	case string:
		return pystr.IntText(x)
	}
	return pyrepr.Int(v)
}

// Timeout seconds clamped to the exec bounds, the default for a value int refuses
func clampTimeout(timeoutS any) int {
	n, ok := pyIntOf(timeoutS)
	if !ok {
		return execDefaultTimeoutS
	}
	if n.Cmp(big.NewInt(execMaxTimeoutS)) > 0 {
		return execMaxTimeoutS
	}
	if n.Cmp(big.NewInt(int64(execMinTimeoutS))) < 0 {
		return execMinTimeoutS
	}
	return int(n.Int64())
}

type cliResult struct {
	code           int
	stdout, stderr string
}

var errCLITimeout = errors.New("browser-use CLI timed out")

// Executable the child env's PATH resolves, the Popen FileNotFoundError when none does
func lookCLIPath(name string, penv map[string]string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) || (runtime.GOOS == "windows" && strings.ContainsRune(name, '/')) {
		return name, nil
	}
	pathVar, ok := penv["PATH"]
	if !ok {
		pathVar = os.Getenv("PATH")
	}
	var last error = syscall.ENOENT
	for _, dir := range filepath.SplitList(pathVar) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		if p, err := exec.LookPath(candidate); err == nil {
			return p, nil
		} else if errors.Is(err, os.ErrPermission) {
			last = syscall.EACCES
		}
	}
	return "", &os.PathError{Op: "exec", Path: name, Err: last}
}

// Text mode decoding of a CLI pipe, UTF-8 with replacement and universal newlines
func decodeCLIText(data []byte) string {
	text, err := pycodecs.DecodeReplace(data, "utf-8")
	if err != nil {
		text = strings.ToValidUTF8(string(data), "�")
	}
	return pystr.TranslateNewlines(text)
}

// Runs the CLI in its own process group, killing the whole group and draining briefly on timeout
var runCLIKillingProcessGroup = func(cmd []string, code string, penv map[string]string, timeout time.Duration) (*cliResult, error) {
	path, err := lookCLIPath(cmd[0], penv)
	if err != nil {
		return nil, err
	}
	c := exec.Command(path, cmd[1:]...)
	c.Args[0] = cmd[0]
	c.Env = envList(penv)
	c.SysProcAttr = detachedSpawnAttrs()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	c.Stdin, c.Stdout, c.Stderr = stdinR, outW, errW
	startErr := c.Start()
	stdinR.Close()
	outW.Close()
	errW.Close()
	if startErr != nil {
		stdinW.Close()
		outR.Close()
		errR.Close()
		var pe *os.PathError
		if errors.As(startErr, &pe) {
			pe.Path = cmd[0]
		}
		return nil, startErr
	}
	go func() {
		input := code
		if runtime.GOOS == "windows" {
			input = strings.ReplaceAll(input, "\n", "\r\n")
		}
		stdinW.WriteString(input)
		stdinW.Close()
	}()
	var stdout, stderr []byte
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); stdout, _ = readAllFile(outR) }()
	go func() { defer wg.Done(); stderr, _ = readAllFile(errR) }()
	var state *os.ProcessState
	go func() { defer wg.Done(); c.Wait(); state = c.ProcessState }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		outR.Close()
		errR.Close()
		return &cliResult{code: popenReturnCode(state), stdout: decodeCLIText(stdout), stderr: decodeCLIText(stderr)}, nil
	case <-timer.C:
	}
	killCLIGroup(c.Process.Pid)
	select {
	case <-done:
	case <-time.After(postKillDrain):
	}
	outR.Close()
	errR.Close()
	return nil, errCLITimeout
}

func readAllFile(f *os.File) ([]byte, error) {
	var out []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, err
		}
	}
}

func envList(penv map[string]string) []string {
	out := make([]string, 0, len(penv))
	for _, k := range slices.Sorted(maps.Keys(penv)) {
		out = append(out, k+"="+penv[k])
	}
	return out
}

// Stops every harness daemon this process drove, only homeKey's when given, through the harness reload
func stopHarnessDaemons(env *tools.Env, homeKey string) {
	drivenDaemonsMu.Lock()
	var daemons [][]string
	for key, d := range drivenDaemons {
		if homeKey == "" || d.home == homeKey {
			daemons = append(daemons, d.identity)
			delete(drivenDaemons, key)
		}
	}
	drivenDaemonsMu.Unlock()
	slices.SortFunc(daemons, slices.Compare)
	reloadDaemons(env, daemons)
}

// Pid the harness published for the daemon env drives, JSON with a pid field or a bare pid
func publishedDaemonPid(penv map[string]string) (int, bool) {
	data, err := os.ReadFile(harnessPidFile(penv))
	if err != nil {
		return 0, false
	}
	data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return 0, false
	}
	raw := pystr.Strip(string(data))
	var value any = nil
	if strings.HasPrefix(raw, "{") {
		parsed, err := pyjson.Loads(raw)
		if err != nil {
			return 0, false
		}
		obj, ok := parsed.(*pyjson.Object)
		if !ok {
			return 0, false
		}
		v, ok := obj.Get("pid")
		if !ok {
			return 0, false
		}
		value = v
	} else {
		fields := pystr.SplitWhitespace(raw, -1)
		if len(fields) == 0 {
			return 0, false
		}
		value = fields[0]
	}
	n, ok := pyIntOf(value)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return int(n.Int64()), true
}

type daemonOwnership int

const (
	ownNothingYet daemonOwnership = iota
	ownNever
	ownOwned
)

// Hands the daemon group to the owner death supervisor, releasing the group it replaced once empty
func recordOwnedDaemon(daemon []string, pid int) {
	key := identityKey(daemon)
	ownedDaemonGroupsMu.Lock()
	replaced, had := ownedDaemonGroups[key]
	ownedDaemonGroups[key] = pid
	ownedDaemonGroupsMu.Unlock()
	tools.OwnProcessGroups([]int{pid})
	tools.RegisterChild(pid, harnessDaemonPurpose, "")
	if had {
		tools.ReleaseProcessGroupOnceEmpty(replaced, nil, 0)
	}
}

func alreadyOwned(daemon []string, pid int) bool {
	ownedDaemonGroupsMu.Lock()
	defer ownedDaemonGroupsMu.Unlock()
	got, ok := ownedDaemonGroups[identityKey(daemon)]
	return ok && got == pid
}

func isWorkerProcess() bool { return tools.IsWorkerProcess != nil && tools.IsWorkerProcess() }

// Reloads each daemon through the harness, releasing its owned group once it is gone
func reloadDaemons(env *tools.Env, daemons [][]string) {
	if len(daemons) == 0 {
		return
	}
	cmd := findCLI(env)
	if cmd == nil {
		return
	}
	base := baseSubprocessEnv(context.Background(), env)
	for _, daemon := range daemons {
		runCLIKillingProcessGroup(append(slices.Clone(cmd), "--reload"), "", daemonEnv(base, daemon), 15*time.Second)
		ownedDaemonGroupsMu.Lock()
		pgid, had := ownedDaemonGroups[identityKey(daemon)]
		delete(ownedDaemonGroups, identityKey(daemon))
		ownedDaemonGroupsMu.Unlock()
		if had {
			tools.ReleaseProcessGroupOnceEmpty(pgid, nil, time.Second)
		}
	}
}

// Routes one call and remembers a real profile fallback so the route keeps its fresh browser
func routeExec(ctx context.Context, env *tools.Env, penv map[string]string, session, taskID string, local, localSidecar bool, routeKey string) (string, bool) {
	execStateMu.Lock()
	_, fellBack := realProfileFallbackNotes[routeKey]
	execStateMu.Unlock()
	routeErr := routeBackend(ctx, env, penv, session, taskID, local, localSidecar, !fellBack)
	note := penv[realProfileFallbackSentinel]
	delete(penv, realProfileFallbackSentinel)
	if note != "" {
		execStateMu.Lock()
		if _, ok := realProfileFallbackNotes[routeKey]; !ok {
			realProfileFallbackNotes[routeKey] = note
		}
		execStateMu.Unlock()
	}
	fallback := penv[localFallbackSentinel]
	delete(penv, localFallbackSentinel)
	return routeErr, localSidecar || fallback != ""
}

// Routes a copy of the env, again up to three tries while a new endpoint publishes meanwhile
func routeExecCurrent(ctx context.Context, env *tools.Env, penv map[string]string, session, taskID string, local, localSidecar bool, routeKey string) (map[string]string, string, bool, int) {
	var routed map[string]string
	var routeErr string
	var sidecar bool
	var generation int
	for range 3 {
		routed = maps.Clone(penv)
		generation = execLiveGeneration(env)
		routeErr, sidecar = routeExec(ctx, env, routed, session, taskID, local, localSidecar, routeKey)
		if routeErr != "" || execLiveGeneration(env) == generation {
			break
		}
	}
	return routed, routeErr, sidecar, generation
}

// Stops this call's daemon when the profile published a new endpoint while it spawned on the old one
func reloadIfRepublished(env *tools.Env, penv map[string]string, generation int) {
	if execLiveGeneration(env) == generation {
		return
	}
	daemon := daemonIdentity(penv)
	drivenDaemonsMu.Lock()
	delete(drivenDaemons, identityKey(daemon))
	drivenDaemonsMu.Unlock()
	reloadDaemons(env, [][]string{daemon})
}

// The fallback note for the first result after a route fell back, nothing afterwards
func takeRealProfileNote(routeKey string) (string, bool) {
	execStateMu.Lock()
	defer execStateMu.Unlock()
	note, ok := realProfileFallbackNotes[routeKey]
	if !ok || realProfileNoteDelivered[routeKey] {
		return "", false
	}
	realProfileNoteDelivered[routeKey] = true
	return note, true
}

// Fence refusal as the ordered dict the session layer builds
func fenceRefusal(m map[string]any) *pyjson.Object {
	out := pyjson.NewObject()
	order := []string{"success", "error", "code"}
	if msg, _ := m["error"].(string); strings.HasPrefix(msg, fenceTakeoverPrefix) {
		order = []string{"success", "code", "error"}
	}
	for _, k := range order {
		if v, ok := m[k]; ok {
			out.Set(k, v)
		}
	}
	return out
}

// Runs Python code through the browser harness CLI on the routed browser and returns its output
func browserExec(ctx context.Context, env *tools.Env, code, session, timeoutS any, taskID string, local bool, localEngine *string) (string, error) {
	if !pyrepr.Truthy(code) {
		return toolErr("No code provided. Pass Python that uses the pre-imported helpers, e.g. new_tab(\"https://example.com\") then print(page_info())."), nil
	}
	codeText, ok := code.(string)
	if !ok {
		return "", pyError("AttributeError", fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(code)))
	}
	if pystr.Strip(codeText) == "" {
		return toolErr("No code provided. Pass Python that uses the pre-imported helpers, e.g. new_tab(\"https://example.com\") then print(page_info())."), nil
	}
	blocked, verdictSidecar, err := codeURLVerdict(env, codeText)
	if err != nil {
		return "", err
	}
	if blocked != "" {
		return toolErr(blocked), nil
	}
	sessionName := ""
	if pyrepr.Truthy(session) {
		sessionName = pyrepr.Str(session)
	}
	routeKey := backendCacheKey(ctx, taskID, sessionName, false)
	localSidecar := verdictSidecar
	if !execURLRE.MatchString(codeText) {
		execStateMu.Lock()
		localSidecar = execRouteIsSidecar[routeKey]
		execStateMu.Unlock()
	}
	cmd := findCLI(env)
	if cmd == nil {
		return toolErr("browser-harness is missing from Icebath's Python environment. Run `icebath update` to re-sync it."), nil
	}
	penv := baseSubprocessEnv(ctx, env)
	if sessionName != "" {
		text, isStr := session.(string)
		if !isStr {
			return "", pyError("TypeError", fmt.Sprintf("expected string or bytes-like object, got '%s'", pyrepr.TypeName(session)))
		}
		if !execSessionRE.MatchString(text) {
			return toolErr(fmt.Sprintf("Invalid session name %s: use 1-64 letters, digits, dashes, or underscores (e.g. 'r7k2').", pyrepr.StrRepr(text))), nil
		}
		penv["BU_NAME"] = text
	}
	penv, routeErr, localSidecar, generation := routeExecCurrent(ctx, env, penv, sessionName, taskID, local, localSidecar, routeKey)
	if routeErr != "" {
		return toolErr(routeErr), nil
	}
	if localSidecar {
		penv["BU_NAME"] = sidecarDaemonName(sessionName)
	}
	fitDefaultSocketDir(penv)
	execStateMu.Lock()
	execRouteIsSidecar[routeKey] = localSidecar
	execStateMu.Unlock()
	engine := penv[botDesktopBrowserSentinel]
	delete(penv, botDesktopBrowserSentinel)
	if localEngine != nil {
		*localEngine = engine
	}
	private := penv[privateBrowserSentinel]
	delete(penv, privateBrowserSentinel)
	if sessionName != "" && private == "" {
		codeText = ownTabPreamble + codeText
	}
	activate := penv[cloudBrowserSentinel]
	delete(penv, cloudBrowserSentinel)
	if activate == "" {
		activate = activateNever
	}
	shotPath, err := WriteBrowserUseShot(filepath.Join(cfgOf(env).homeDir(), "cache", "browser-use"))
	if err != nil {
		return "", err
	}
	if cmd, err = harnessCommand(cmd, activate, shotPath); err != nil {
		return "", err
	}
	workspace := workspaceDir(env, taskID)
	if workspace != "" {
		penv["BH_AGENT_WORKSPACE"] = workspace
	}
	if _, ok := penv["BU_AUTOSPAWN"]; !ok && isLegacyBrowserUseCloudConfig(env) {
		penv["BU_AUTOSPAWN"] = "1"
	}
	timeout := clampTimeout(timeoutS)
	started := float64(time.Now().UnixNano()) / 1e9
	dispatch := func() map[string]any {
		attachVaultSupervisor(ctx, env, penv, taskID)
		daemon := daemonIdentity(penv)
		drivenDaemonsMu.Lock()
		drivenDaemons[identityKey(daemon)] = drivenDaemon{identity: daemon, home: cfgOf(env).homeKey()}
		drivenDaemonsMu.Unlock()
		owning := cron.SchedulePeriodic(ctx, func(context.Context) bool { return ownHarnessDaemon(penv) == ownNothingYet }, ownDuringCall)
		defer owning.Cancel(5 * time.Second)
		res, err := runCLIKillingProcessGroup(cmd, codeText, penv, time.Duration(timeout)*time.Second)
		if errors.Is(err, errCLITimeout) {
			return map[string]any{"error_result": toolErr(fmt.Sprintf(
				"browser-use exec timed out after %ds. The daemon may still be working; retry "+
					"with a larger timeout_s (max %d), or split the work into several calls that "+
					"append to workspace files — anything already written to the workspace is preserved.", timeout, execMaxTimeoutS))}
		}
		if err != nil {
			return map[string]any{"error_result": toolErr("Failed to launch browser-use CLI: " + pypath.OSErrorText(err))}
		}
		return map[string]any{"proc": res}
	}
	var dispatched map[string]any
	if engine != "" {
		dispatched = runFenced(env, map[string]any{"features": map[string]any{"local": true}}, dispatch)
	} else {
		dispatched = dispatch()
	}
	reloadIfRepublished(env, penv, generation)
	ownHarnessDaemon(penv)
	proc, ok := dispatched["proc"].(*cliResult)
	if !ok {
		if er, ok := dispatched["error_result"]; ok {
			return er.(string), nil
		}
		return tools.ToolResult(fenceRefusal(dispatched)), nil
	}
	redact := func(s string) string { return tools.RedactSensitiveTextWith(env, s, tools.RedactOptions{Force: true}) }
	result := pyjson.ObjectOf("success", proc.code == 0, "exit_code", proc.code, "output", redact(proc.stdout))
	if workspace != "" {
		result.Set("workspace", workspace)
	}
	if sessionName != "" {
		result.Set("session", session)
	}
	if note, ok := takeRealProfileNote(routeKey); ok {
		result.Set("real_profile_unavailable", note)
	}
	stderr := redact(pystr.Strip(proc.stderr))
	if r := []rune(stderr); len(r) > execStderrCapChars {
		stderr = string(r[:execStderrCapChars]) + "\n… (stderr truncated)"
	}
	if stderr != "" {
		result.Set("stderr", stderr)
	}
	if shot := findScreenshot(proc.stdout, started); shot != "" {
		result.Set("screenshot_path", shot)
		if native := nativeScreenshotResult(ctx, env, result, shot); native != "" {
			return native, nil
		}
	}
	return tools.ToolResult(result), nil
}
