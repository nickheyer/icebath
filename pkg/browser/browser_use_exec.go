package browser

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	browserUseProvider          = "browser-use"
	privateBrowserSentinel      = "_ICEBATH_BU_PRIVATE_BROWSER"
	botDesktopBrowserSentinel   = "_ICEBATH_BU_BOT_DESKTOP_BROWSER"
	realProfileFallbackSentinel = "_ICEBATH_BU_REAL_PROFILE_FALLBACK"
	localFallbackSentinel       = "_ICEBATH_BU_LOCAL_FALLBACK"
	cloudBrowserSentinel        = "_ICEBATH_BU_CLOUD_BROWSER"
	activateNever               = "never"
	activateAlways              = "always"
	fenceTakeoverPrefix         = "A human took over the bot's screen while this browser command ran"
)

// Lowest timeout a call may ask for, swapped by tests
var execMinTimeoutS = 5

const execPySpace = `\t\n\v\f\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	execSessionRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	execTaskIDSafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	execURLRE      = regexp.MustCompile(`(?i)https?://[^` + execPySpace + `'"\\)]+`)
)

// Seams tests replace, the session layer, provider lookup and supervisor attach
var (
	execSessionInfo      = getSessionInfo
	execCloudProvider    = getCloudProvider
	execAttachSupervisor = supervisorGetOrStart
	execRealProfileCDP   = func(env *tools.Env) (string, string) { return realProfileCDP(env) }
	execLiveGeneration   = func(env *tools.Env) int { return LiveCDPGeneration(cfgOf(env).homeKey()) }
	execCDPOverride      = func(ctx context.Context, env *tools.Env) string { return cfgOf(env).cdpOverride(ctx) }
	execUsingLightpanda  = usingLightpandaEngine
	execNativeVision     = tools.ShouldUseNativeVisionFastPath
	execResizeForVision  = tools.ResizeImageForVision
)

// Route bookkeeping across browser_exec calls, keyed by route cache key
var (
	execStateMu              sync.Mutex
	execRouteIsSidecar       = map[string]bool{}
	realProfileFallbackNotes = map[string]string{}
	realProfileNoteDelivered = map[string]bool{}
)

// Records a DevTools endpoint on the route, BU_CDP_URL for http and https, BU_CDP_WS otherwise
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

// Blocking error of the first unsafe URL in the steps and whether any URL routes to the local sidecar
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

func isDirPath(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
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

// Session name of the hybrid local sidecar browser
func sidecarSessionName(name string) string {
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
func mergingSessionInfo(ctx context.Context, env *tools.Env, session map[string]any) func(string) (map[string]any, error) {
	return func(key string) (map[string]any, error) {
		got, err := execSessionInfo(tools.WithEnvHome(ctx, env), env, key)
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
	err := exportSessionCDP(penv, mergingSessionInfo(tools.WithEnvHome(ctx, env), env, session), key, fail, "The local browser session returned no CDP endpoint.")
	if err == "" {
		markLocalBrowser(penv, session, key)
	}
	return err
}

// Routes to the configured backend, operator overrides first, then sidecar, provider and local engine
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
	providerName := providerClassName(prov)
	session := map[string]any{}
	key := backendCacheKey(ctx, taskID, sessionName, false)
	err := exportSessionCDP(penv, mergingSessionInfo(tools.WithEnvHome(ctx, env), env, session), key,
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

// Routes to the consented real profile browser when the backend is local or local was forced
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

// Where the steps run, real profile consent first, the error message when no browser can serve them
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
func routeExecCurrent(ctx context.Context, env *tools.Env, penv map[string]string, session, taskID string, local, localSidecar bool, routeKey string) (map[string]string, string, bool) {
	var routed map[string]string
	var routeErr string
	var sidecar bool
	for range 3 {
		routed = maps.Clone(penv)
		generation := execLiveGeneration(env)
		routeErr, sidecar = routeExec(ctx, env, routed, session, taskID, local, localSidecar, routeKey)
		if routeErr != "" || execLiveGeneration(env) == generation {
			break
		}
	}
	return routed, routeErr, sidecar
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

// Characters of stderr a result carries before the rest is cut
const execStderrCapChars = 4000

// Screenshot paths the code prints, POSIX or Windows drive letter absolute
var execImagePathRE = regexp.MustCompile(`(?i)((?:[A-Za-z]:[\\/]|/)[^` + execPySpace + `"']+?\.(?:png|jpe?g|webp))`)

// Interpreter the code runs on, the user's Python from PATH, the seam tests replace
var execPython = func(ctx context.Context, env *tools.Env) (string, error) {
	return codeexec.PythonOnPath(ctx, env, "browser_exec runs Python code")
}

// Last screenshot path printed during this call that exists and was written after the call started
func findScreenshot(stdout string, since time.Time) string {
	paths := execImagePathRE.FindAllString(stdout, -1)
	for i := len(paths) - 1; i >= 0; i-- {
		if st, err := os.Stat(paths[i]); err == nil && st.Mode().IsRegular() && !st.ModTime().Before(since.Add(-time.Second)) {
			return paths[i]
		}
	}
	return ""
}

// The one credential the code's helpers read, from the active profile's secrets, never another profile's
func harnessCredentials(env *tools.Env) map[string]string {
	out := map[string]string{}
	if key := cfgOf(env).rawSecret("BROWSER_USE_API_KEY"); key != "" {
		out["BROWSER_USE_API_KEY"] = key
	}
	return out
}

// PATH with the standard system directories that exist appended, unchanged on Windows
func floorSubprocessPath(path string) string {
	if runtime.GOOS == "windows" {
		return path
	}
	return mergeSanePath(path)
}

// Environment of the code's interpreter, the browser child env with the harness credential, the route and no Python path override
func execChildEnv(env *tools.Env, route map[string]string, workspace string) map[string]string {
	penv := buildBrowserEnv(env)
	maps.Copy(penv, harnessCredentials(env))
	delete(penv, "PYTHONHOME")
	delete(penv, "PYTHONPATH")
	penv["PATH"] = floorSubprocessPath(penv["PATH"])
	for k, v := range route {
		if strings.HasPrefix(k, "BU_") {
			penv[k] = v
		}
	}
	if workspace != "" {
		penv["BH_AGENT_WORKSPACE"] = workspace
	}
	return penv
}

// Runs the model's Python on the user's interpreter against the routed browser and returns its output
func browserExec(ctx context.Context, env *tools.Env, code, sessionName string, timeoutS any, taskID string, local bool, localEngine *string) (string, error) {
	if pystr.Strip(code) == "" {
		return toolErr(`No code provided. Pass Python that uses the pre-imported helpers, e.g. new_tab("https://example.com") then print(page_info()).`), nil
	}
	blocked, verdictSidecar, err := codeURLVerdict(env, code)
	if err != nil {
		return "", err
	}
	if blocked != "" {
		return toolErr(blocked), nil
	}
	routeKey := backendCacheKey(ctx, taskID, sessionName, false)
	localSidecar := verdictSidecar
	if !execURLRE.MatchString(code) {
		execStateMu.Lock()
		localSidecar = execRouteIsSidecar[routeKey]
		execStateMu.Unlock()
	}
	python, err := execPython(ctx, env)
	if err != nil {
		return toolErr(err.Error()), nil
	}
	if sessionName != "" && !execSessionRE.MatchString(sessionName) {
		return toolErr(fmt.Sprintf("Invalid session name %s: use 1-64 letters, digits, dashes, or underscores (e.g. 'r7k2').", pyrepr.StrRepr(sessionName))), nil
	}
	route := map[string]string{}
	if sessionName != "" {
		route["BU_NAME"] = sessionName
	}
	route, routeErr, localSidecar := routeExecCurrent(ctx, env, route, sessionName, taskID, local, localSidecar, routeKey)
	if routeErr != "" {
		return toolErr(routeErr), nil
	}
	if localSidecar {
		route["BU_NAME"] = sidecarSessionName(sessionName)
	}
	execStateMu.Lock()
	execRouteIsSidecar[routeKey] = localSidecar
	execStateMu.Unlock()
	engine := route[botDesktopBrowserSentinel]
	if localEngine != nil {
		*localEngine = engine
	}
	workspace := workspaceDir(env, taskID)
	timeout := clampTimeout(timeoutS)
	started := time.Now()
	run := execRun{python: python, childEnv: execChildEnv(env, route, workspace), route: route, code: code, workspace: workspace,
		timeout: timeout, activate: route[cloudBrowserSentinel] == activateAlways, ownTab: sessionName != "" && route[privateBrowserSentinel] == ""}
	dispatch := func() map[string]any {
		attachVaultSupervisor(ctx, env, route, taskID)
		proc := execRunCode(tools.WithEnvHome(ctx, env), env, run)
		if proc.timedOut {
			return map[string]any{"error_result": toolErr(fmt.Sprintf("browser_exec timed out after %ds. The browser may still be working; retry "+
				"with a larger timeout_s (max %d), or split the work into several calls that "+
				"append to workspace files — anything already written to the workspace is preserved.", timeout, execMaxTimeoutS))}
		}
		if proc.launchErr != nil {
			return map[string]any{"error_result": toolErr("Failed to launch Python for browser_exec: " + pypath.OSErrorText(proc.launchErr))}
		}
		return map[string]any{"proc": proc}
	}
	var dispatched map[string]any
	if engine != "" {
		dispatched = runFenced(env, map[string]any{"features": map[string]any{"local": true}}, dispatch)
	} else {
		dispatched = dispatch()
	}
	proc, ok := dispatched["proc"].(execProc)
	if !ok {
		if refusal, ok := dispatched["error_result"].(string); ok {
			return refusal, nil
		}
		return tools.ToolResult(fenceRefusal(dispatched)), nil
	}
	redact := func(s string) string { return tools.RedactSensitiveTextWith(env, s, tools.RedactOptions{Force: true}) }
	result := pyjson.ObjectOf("success", proc.returnCode == 0, "exit_code", proc.returnCode, "output", redact(proc.stdout))
	if workspace != "" {
		result.Set("workspace", workspace)
	}
	if sessionName != "" {
		result.Set("session", sessionName)
	}
	if note, ok := takeRealProfileNote(routeKey); ok {
		result.Set("real_profile_unavailable", note)
	}
	stderr := redact(pystr.Strip(proc.stderr))
	if runes := []rune(stderr); len(runes) > execStderrCapChars {
		stderr = string(runes[:execStderrCapChars]) + "\n… (stderr truncated)"
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
