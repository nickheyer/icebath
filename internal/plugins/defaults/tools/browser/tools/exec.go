package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/tools/redact"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const (
	execDefault = 300
	execMax     = 1800
	execMin     = 5
	// Most characters of stderr a result carries
	stderrCap = 4000
)

// Python's whitespace, which ends a URL the code names
const pySpace = `\t\n\v\f\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	sessionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	unsafeTask  = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	codeURL     = regexp.MustCompile(`(?i)https?://[^` + pySpace + `'"\\)]+`)
	imagePath   = regexp.MustCompile(`(?i)((?:[A-Za-z]:[\\/]|/)[^` + pySpace + `"']+?\.(?:png|jpe?g|webp))`)
)

// Preamble the code runs under, every helper a call over the inherited pipe pair that Go answers
const preamble = `import sys
for _stream in (sys.stdout, sys.stderr):
    if hasattr(_stream, "reconfigure"):
        try:
            _stream.reconfigure(encoding="utf-8", errors="replace")
        except Exception:
            pass


def _icebath_browser_helpers():
    import base64, importlib, importlib.util, json, math, os, threading, time, types, urllib.request
    from io import StringIO
    from pathlib import Path
    from urllib.parse import urlparse

    def channel(desc, mode):
        if @HANDLES@:
            import msvcrt
            desc = msvcrt.open_osfhandle(desc, os.O_RDONLY if mode == "r" else os.O_WRONLY)
        os.set_inheritable(desc, False)
        return open(desc, mode, encoding="utf-8", newline="\n")

    replies, requests, lock = channel(@REPLIES@, "r"), channel(@REQUESTS@, "w"), threading.Lock()

    def unserializable(value):
        if value == "NaN":
            return math.nan
        if value == "Infinity":
            return math.inf
        if value == "-Infinity":
            return -math.inf
        if value == "-0":
            return -0.0
        if value.endswith("n"):
            return int(value[:-1])
        return value

    def call(helper, args):
        line = json.dumps({"helper": helper, "args": args}) + "\n"
        with lock:
            requests.write(line)
            requests.flush()
            reply = replies.readline()
        if not reply:
            raise RuntimeError("the browser closed the helper channel")
        reply = json.loads(reply)
        if "error" in reply:
            if reply.get("type") == "OSError":
                raise OSError(*reply["oserror"])
            raise (TimeoutError if reply.get("type") == "TimeoutError" else RuntimeError)(reply["error"])
        if "unserializable" in reply:
            return unserializable(reply["unserializable"])
        return reply.get("result")

    DEFAULT_IPC_RESPONSE_TIMEOUT_SECONDS = 5.0

    def cdp(method, session_id=None, _response_timeout=DEFAULT_IPC_RESPONSE_TIMEOUT_SECONDS, **params):
        return call("cdp", {"method": method, "session_id": session_id, "response_timeout": _response_timeout, "params": params})

    def drain_events():
        return call("drain_events", {})

    def goto_url(url):
        return call("goto_url", {"url": url, "domain_skills": os.environ.get("BH_DOMAIN_SKILLS") == "1"})

    def page_info():
        return call("page_info", {})

    def click_at_xy(x, y, button="left", clicks=1):
        call("click_at_xy", {"x": x, "y": y, "button": button, "clicks": clicks})

    def type_text(text):
        call("type_text", {"text": text})

    def fill_input(selector, text, clear_first=True, timeout=0.0):
        call("fill_input", {"selector": selector, "text": text, "clear_first": clear_first, "timeout": timeout})

    def press_key(key, modifiers=0):
        call("press_key", {"key": key, "modifiers": modifiers})

    def scroll(x, y, dy=-300, dx=0):
        call("scroll", {"x": x, "y": y, "dy": dy, "dx": dx})

    def capture_screenshot(path=None, full=False, max_dim=None):
        shot = call("capture_screenshot", {"path": os.path.abspath(os.fspath(path)) if path else None, "full": full, "max_dim": max_dim})
        return os.fspath(path) if path else shot

    def list_tabs(include_chrome=True):
        return call("list_tabs", {"include_chrome": include_chrome})

    def current_tab():
        return call("current_tab", {})

    def activate_tab(target):
        return call("activate_tab", {"target": target})

    def switch_tab(target, activate=False):
        return call("switch_tab", {"target": target, "activate": activate})

    def new_tab(url="about:blank"):
        return call("new_tab", {"url": url})

    def close_tab(target=None):
        call("close_tab", {"target": target})

    def ensure_real_tab():
        return call("ensure_real_tab", {})

    def iframe_target(url_substr):
        return call("iframe_target", {"url_substr": url_substr})

    def wait(seconds=1.0):
        time.sleep(seconds)

    def wait_for_load(timeout=15.0):
        return call("wait_for_load", {"timeout": timeout})

    def wait_for_element(selector, timeout=10.0, visible=False):
        return call("wait_for_element", {"selector": selector, "timeout": timeout, "visible": visible})

    def wait_for_network_idle(timeout=10.0, idle_ms=500):
        return call("wait_for_network_idle", {"timeout": timeout, "idle_ms": idle_ms})

    def js(expression, target_id=None):
        return call("js", {"expression": expression, "target_id": target_id})

    def dispatch_key(selector, key="Enter", event="keypress"):
        call("dispatch_key", {"selector": selector, "key": key, "event": event})

    def upload_file(selector, path):
        call("upload_file", {"selector": selector, "path": path if isinstance(path, str) else list(path)})

    def http_get(url, headers=None, timeout=20.0):
        if os.environ.get("BROWSER_USE_API_KEY"):
            try:
                from fetch_use import fetch_sync
                return fetch_sync(url, headers=headers, timeout_ms=int(timeout * 1000)).text
            except ImportError:
                pass
        import gzip
        h = {"User-Agent": "Mozilla/5.0", "Accept-Encoding": "gzip"}
        if headers:
            h.update(headers)
        with urllib.request.urlopen(urllib.request.Request(url, headers=h), timeout=timeout) as r:
            data = r.read()
            if r.headers.get("Content-Encoding") == "gzip":
                data = gzip.decompress(data)
            return data.decode()

    workspace = os.environ.get("BH_AGENT_WORKSPACE")
    AGENT_WORKSPACE = Path(workspace).expanduser().resolve() if workspace else None
    if AGENT_WORKSPACE is not None and (AGENT_WORKSPACE / ".env").exists():
        for line in (AGENT_WORKSPACE / ".env").read_text(encoding="utf-8-sig", errors="replace").splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            os.environ.setdefault(k.strip(), v.strip().strip('"').strip("'"))

    namespace = {
        "__name__": "__main__", "base64": base64, "importlib": importlib, "json": json, "math": math, "os": os,
        "sys": sys, "time": time, "urllib": urllib, "Path": Path, "urlparse": urlparse, "StringIO": StringIO,
        "NAME": os.environ.get("BU_NAME", "default"), "AGENT_WORKSPACE": AGENT_WORKSPACE,
        "INTERNAL": ("chrome://", "chrome-untrusted://", "devtools://", "chrome-extension://", "about:"),
        "DEFAULT_IPC_RESPONSE_TIMEOUT_SECONDS": DEFAULT_IPC_RESPONSE_TIMEOUT_SECONDS,
        "cdp": cdp, "drain_events": drain_events, "goto_url": goto_url, "page_info": page_info,
        "click_at_xy": click_at_xy, "type_text": type_text, "fill_input": fill_input, "press_key": press_key,
        "scroll": scroll, "capture_screenshot": capture_screenshot, "list_tabs": list_tabs,
        "current_tab": current_tab, "activate_tab": activate_tab, "switch_tab": switch_tab, "new_tab": new_tab,
        "close_tab": close_tab, "ensure_real_tab": ensure_real_tab, "iframe_target": iframe_target,
        "wait": wait, "wait_for_load": wait_for_load, "wait_for_element": wait_for_element,
        "wait_for_network_idle": wait_for_network_idle, "js": js, "dispatch_key": dispatch_key,
        "upload_file": upload_file, "http_get": http_get,
    }
    package, module = types.ModuleType("browser_harness"), types.ModuleType("browser_harness.helpers")
    module.__dict__.update({k: v for k, v in namespace.items() if k != "__name__"})
    package.helpers = module
    sys.modules["browser_harness"], sys.modules["browser_harness.helpers"] = package, module
    if AGENT_WORKSPACE is not None and (AGENT_WORKSPACE / "agent_helpers.py").exists():
        spec = importlib.util.spec_from_file_location("browser_harness_agent_helpers", AGENT_WORKSPACE / "agent_helpers.py")
        if spec and spec.loader:
            loaded = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(loaded)
            for name, value in vars(loaded).items():
                if not name.startswith("_"):
                    namespace[name] = value
                    setattr(module, name, value)
    return sys.stdin.read(), namespace


_icebath_code, _icebath_namespace = _icebath_browser_helpers()
del _icebath_browser_helpers
exec(compile(_icebath_code, "<string>", "exec"), _icebath_namespace)
`

// Preamble with the descriptors of the child's pipe pair filled in
func launcherScript(pipe terminal.ChildPipe) string {
	handles := "False"
	if pipe.Handles {
		handles = "True"
	}
	return strings.NewReplacer("@HANDLES@", handles, "@REPLIES@", strconv.FormatUint(uint64(pipe.Replies), 10), "@REQUESTS@",
		strconv.FormatUint(uint64(pipe.Requests), 10)).Replace(preamble)
}

// What a route found: the endpoint the code drives and how
type route struct {
	cdp  string
	name string
	// Local browser the Bot Desktop fences, lightpanda or local; empty for one elsewhere
	engine string
	// The browser is the call's alone, so a named session gets no tab of its own
	private bool
	// Bring new tabs forward: a cloud browser or the screen's own, which someone may watch
	activate bool
	// Used the local browser beside a cloud one
	sidecar bool
	// Why the user's own profile could not serve the call
	realNote string
}

// What browser_exec remembers between calls of a route: whether it went local, and the note that the user's own profile failed
type execState struct {
	mu        sync.Mutex
	sidecar   map[string]bool
	notes     map[string]string
	delivered map[string]bool
}

func newExecState() execState {
	return execState{sidecar: map[string]bool{}, notes: map[string]string{}, delivered: map[string]bool{}}
}

// The fallback note for the first result after a route fell back, nothing later
func (e *execState) takeNote(key string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	note, ok := e.notes[key]
	if !ok || e.delivered[key] {
		return "", false
	}
	e.delivered[key] = true
	return note, true
}

// Session key of a route's browser: the task's own, else by name or the default one of the profile, with the local suffix beside a cloud one
func execKey(cfg *config.Config, task, name string, sidecar bool) string {
	key := task
	switch {
	case name != "":
		key = "bu-named-" + name + "@" + cfg.Profile()
	case key == "":
		key = "browser-exec-default@" + cfg.Profile()
	}
	if sidecar {
		key += localSuffix
	}
	return key
}

// Session name of the local browser beside a cloud one
func sidecarName(name string) string {
	out := []rune("_local_" + cmp.Or(name, "default"))
	return string(out[:min(len(out), 64)])
}

// Whether Browser Use's cloud is picked by its key: the key set and no other service chosen
func (r *registrant) browserUseByKey(cfg *config.Config) bool {
	if name, chosen := browser.Chosen(cfg); chosen && name != "browser-use" {
		return false
	}
	p, ok := r.browser.Provider("browser-use")
	return ok && p.Available != nil && p.Available(cfg)
}

// Marks a local session for the fence, and as the call's alone unless its profile is shared
func markLocal(rt *route, s *session) {
	rt.engine = "local"
	if s.features["lightpanda"] {
		rt.engine = "lightpanda"
	}
	if !s.shared && !s.features["real_profile"] {
		rt.private = true
	}
	if s.realNote != "" && rt.realNote == "" {
		rt.realNote = s.realNote
	}
}

// The local browser of the key through the session layer, the failure when it can't start
func (r *registrant) routeLocal(ctx context.Context, cfg *config.Config, rt *route, task, name string, sidecar bool) string {
	key := execKey(cfg, task, name, sidecar)
	s, err := r.info(ctx, cfg, key)
	if err != nil {
		if lightpanda(cfg) {
			return fmt.Sprintf("Lightpanda could not be started: %s Set browser.engine to auto to use local Chrome, or switch backends via "+
				"`icebath tools` → Browser Automation.", err)
		}
		return fmt.Sprintf("The local browser could not be started: %s Run `icebath tools` → Browser Automation to (re)install Chromium, or "+
			"switch backends.", err)
	}
	if s.cdp == "" {
		return "The local browser session returned no CDP endpoint."
	}
	rt.cdp = s.cdp
	markLocal(rt, s)
	if r.sharesScreen(ctx, cfg, s) && !s.features["real_profile"] && !s.features["lightpanda"] {
		rt.activate = true
	}
	return ""
}

// The configured backend: an attached endpoint first, then the local browser beside a cloud one, the cloud service, the local engine
func (r *registrant) routeBackend(ctx context.Context, cfg *config.Config, rt *route, task, name string, sidecar bool) string {
	if rt.cdp != "" {
		return ""
	}
	if override := r.override(ctx, cfg); override != "" {
		rt.cdp = override
		return ""
	}
	if sidecar {
		rt.sidecar = true
		return r.routeLocal(ctx, cfg, rt, task, name, true)
	}
	cloud, err := r.browser.Cloud(cfg)
	if err != nil {
		slog.Debug(fmt.Sprintf("Cloud provider lookup failed: %s", err))
		cloud = nil
	}
	if cloud == nil {
		return r.routeLocal(ctx, cfg, rt, task, name, false)
	}
	key := execKey(cfg, task, name, false)
	s, err := r.info(ctx, cfg, key)
	if err != nil {
		return fmt.Sprintf("Cloud browser provider %s failed to provide a session: %s. Fix the provider configuration or switch backends via "+
			"`icebath tools` → Browser Automation.", cloud.Title, err)
	}
	if s.cdp == "" {
		return fmt.Sprintf("Cloud browser provider %s returned no CDP endpoint, so browser_exec cannot drive it. Pick a CDP-capable provider via "+
			"`icebath tools` → Browser Automation.", cloud.Title)
	}
	rt.cdp = s.cdp
	if s.fallback {
		slog.Warn(fmt.Sprintf("browser_exec: %s failed (%s); using local Chromium for this session", cloud.Title, s.fallbackReason))
		markLocal(rt, s)
		return ""
	}
	rt.activate = true
	if name != "" {
		rt.private = true
	}
	return ""
}

// The browser on the user's own profile when the backend is local or local was asked for, its failure when it can't load
func (r *registrant) routeReal(ctx context.Context, cfg *config.Config, rt *route, local bool) string {
	if !realProfile(cfg) || rt.cdp != "" || r.overrideRaw(cfg) != "" {
		return ""
	}
	if !local {
		if cloud, err := r.browser.Cloud(cfg); err != nil || cloud != nil || r.browserUseByKey(cfg) {
			return ""
		}
	}
	cdp, failure := r.realProfileCDP(ctx, cfg)
	if cdp != "" && failure == "" {
		rt.cdp, rt.engine = cdp, "local"
	}
	return failure
}

// Where one call runs: the user's own profile first unless it already failed this route, the failure when no browser can serve it
func (r *registrant) route(ctx context.Context, cfg *config.Config, task, name string, local, sidecar bool, key string) (*route, string) {
	r.exec.mu.Lock()
	_, fellBack := r.exec.notes[key]
	r.exec.mu.Unlock()
	rt := &route{name: name}
	realFailure := ""
	if !fellBack {
		realFailure = r.routeReal(ctx, cfg, rt, local)
	}
	if local && rt.cdp == "" && !realProfile(cfg) {
		return nil, "local=true was requested but browser.use_real_profile is off. Enable it in config.yaml (browser.use_real_profile: true), then retry."
	}
	if local && rt.cdp == "" && (realFailure != "" || fellBack) {
		if realFailure != "" {
			r.exec.mu.Lock()
			if _, ok := r.exec.notes[key]; !ok {
				r.exec.notes[key] = fallbackNote(realFailure)
			}
			r.exec.mu.Unlock()
		}
		sidecar = true
	}
	if failure := r.routeBackend(ctx, cfg, rt, task, name, sidecar); failure != "" {
		return nil, failure
	}
	if rt.realNote != "" {
		r.exec.mu.Lock()
		if _, ok := r.exec.notes[key]; !ok {
			r.exec.notes[key] = rt.realNote
		}
		r.exec.mu.Unlock()
	}
	return rt, ""
}

// Routes again, up to three times, while a /browser connect publishes another endpoint meanwhile
func (r *registrant) routeCurrent(ctx context.Context, cfg *config.Config, task, name string, local, sidecar bool, key string) (*route, string) {
	var rt *route
	var failure string
	for range 3 {
		generation := r.live.generation(cfg.Profile())
		rt, failure = r.route(ctx, cfg, task, name, local, sidecar, key)
		if failure != "" || r.live.generation(cfg.Profile()) == generation {
			break
		}
	}
	return rt, failure
}

// Refusal of the first unsafe URL the code names, and whether any goes to the local browser beside a cloud one
func (r *registrant) codeVerdict(cfg *config.Config, code string) (string, bool, error) {
	sidecar := false
	for _, u := range codeURL.FindAllString(code, -1) {
		local, err := r.routesLocally(cfg, u)
		if err != nil {
			return "", false, err
		}
		sidecar = sidecar || local
		if refused := r.urlSafety(cfg, u, local); refused != nil {
			return cmp.Or(fmt.Sprint(refused["error"]), "Blocked: unsafe URL"), sidecar, nil
		}
	}
	return "", sidecar, nil
}

// Folder kept across calls, made when missing: browser.exec_workspace when set, under the profile when relative, else one per task
func workspace(cfg *config.Config, task string) string {
	path := execWorkspace(cfg)
	if path != "" && !filepath.IsAbs(path) {
		path = filepath.Join(profileDir(cfg), path)
	}
	if path == "" {
		safe := unsafeTask.ReplaceAllString(cmp.Or(task, "default"), "_")
		if r := []rune(safe); len(r) > 80 {
			safe = string(r[:80])
		}
		path = filepath.Join(profileDir(cfg), "cache", "browser-use", "workspace", cmp.Or(safe, "default"))
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		slog.Debug(fmt.Sprintf("browser_exec workspace unavailable: %s", err))
		return ""
	}
	return path
}

// Seconds the call may run, within the bounds: a bool counts as 0 or 1, a float is cut, text is read in base ten, anything else is the
// default
func clampTimeout(value any) int {
	var n int64
	switch x := value.(type) {
	case nil:
		return execDefault
	case bool:
		if x {
			n = 1
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return execDefault
		}
		n = int64(min(max(x, execMin), execMax))
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(strings.ReplaceAll(x, "_", "")), 10, 64)
		if err != nil {
			return execDefault
		}
		n = parsed
	default:
		parsed, err := cast.ToInt64E(x)
		if err != nil {
			return execDefault
		}
		n = parsed
	}
	return int(min(max(n, execMin), execMax))
}

// Last screenshot path the code printed that exists and was written since the call began
func screenshot(stdout string, since time.Time) string {
	paths := imagePath.FindAllString(stdout, -1)
	for i := len(paths) - 1; i >= 0; i-- {
		if st, err := os.Stat(paths[i]); err == nil && st.Mode().IsRegular() && !st.ModTime().Before(since.Add(-time.Second)) {
			return paths[i]
		}
	}
	return ""
}

// Variable of the Browser Use key, which the helpers read and the Browser Use service's settings read too
const browserUseKey = "BROWSER_USE_API_KEY"

// Environment of the interpreter: the browser's child environment, the one credential the helpers read, the route and no Python path
// override
func (r *registrant) execEnv(ctx context.Context, cfg *config.Config, rt *route, dir string) map[string]string {
	env := r.childEnv(ctx, cfg)
	if key := cfg.Variable(browserUseKey); key != "" {
		env[browserUseKey] = key
	}
	delete(env, "PYTHONHOME")
	delete(env, "PYTHONPATH")
	if runtime.GOOS != "windows" {
		env["PATH"] = completePath(env["PATH"])
	}
	if strings.HasPrefix(rt.cdp, "http://") || strings.HasPrefix(rt.cdp, "https://") {
		env["BU_CDP_URL"] = rt.cdp
	} else {
		env["BU_CDP_WS"] = rt.cdp
	}
	if rt.name != "" {
		env["BU_NAME"] = rt.name
	}
	if dir != "" {
		env["BH_AGENT_WORKSPACE"] = dir
	}
	return env
}

// What the code's process left
type execProc struct {
	code     int
	stdout   string
	stderr   string
	timedOut bool
	failure  error
}

// Connects the route's browser, puts the session on a tab and runs the code, its helper calls answered on that tab
func (r *registrant) runCode(ctx context.Context, cfg *config.Config, python string, rt *route, env map[string]string, code, dir string,
	timeout int, ownTab bool) execProc {
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	endpoint := rt.cdp
	if !strings.HasPrefix(endpoint, "ws://") && !strings.HasPrefix(endpoint, "wss://") {
		endpoint = resolveCDP(ctx, endpoint)
	}
	s := r.execs.of(cfg.Profile()+"\x00"+cmp.Or(env["BU_NAME"], "default"), cfg.Profile())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventMu.Lock()
	s.marker = tabMarker(cfg)
	s.eventMu.Unlock()
	limit := time.Now().Add(setupTimeout)
	if deadline.Before(limit) {
		limit = deadline
	}
	setup, cancel := context.WithDeadline(ctx, limit)
	defer cancel()
	h := &helperRun{ctx: setup, s: s, workspace: dir, activate: rt.activate, ownTab: ownTab,
		shotDir: cmp.Or(strings.TrimSpace(env["BH_TMP_DIR"]), filepath.Join(profileDir(cfg), "cache", "browser-use", "tmp"))}
	refuse := func(format string, args ...any) execProc {
		if time.Now().After(deadline) {
			return execProc{timedOut: true}
		}
		return execProc{code: 1, stderr: "browser_exec: " + fmt.Sprintf(format, args...) + "\n"}
	}
	if endpoint == "" {
		return refuse("the browser route names no DevTools endpoint")
	}
	if err := s.connect(setup, endpoint); err != nil {
		return refuse("%s", err)
	}
	if err := h.ensureTab(); err != nil {
		s.disconnect()
		return refuse("no tab could be attached: %s", err)
	}
	result, err := r.terminal.RunChild(ctx, cfg, terminal.ChildRun{
		Program: python,
		Args:    func(pipe terminal.ChildPipe) []string { return []string{"-c", launcherScript(pipe)} },
		Stdin:   code,
		Env:     env,
		Dir:     dir,
		Timeout: time.Until(deadline),
		Purpose: "browser-exec",
		Serve: func(serve context.Context, requests io.Reader, replies io.Writer) {
			h.ctx = serve
			h.serve(requests, replies)
		},
	})
	if err != nil {
		return execProc{failure: err}
	}
	if result.TimedOut || ctx.Err() != nil {
		s.disconnect()
	}
	return execProc{code: result.Code, stdout: result.Stdout, stderr: result.Stderr, timedOut: result.TimedOut}
}

func errorJSON(message string) string { return tools.JSON(map[string]any{"error": message}) }

// Runs the model's Python on the user's interpreter against the routed browser
func (r *registrant) execute(ctx context.Context, call *tools.Call) string {
	cfg := call.Env.Config
	code, _ := call.Args["code"].(string)
	name, _ := call.Args["session"].(string)
	local := config.Truthy(call.Args["local"])
	timeoutArg, given := call.Args["timeout_s"]
	if !given {
		timeoutArg = execDefault
	}
	if strings.TrimSpace(code) == "" {
		return errorJSON(`No code provided. Pass Python that uses the pre-imported helpers, e.g. new_tab("https://example.com") then print(page_info()).`)
	}
	blocked, sidecar, err := r.codeVerdict(cfg, code)
	if err != nil {
		return tools.ErrorResult(err)
	}
	if blocked != "" {
		return errorJSON(blocked)
	}
	task := call.SessionID
	key := execKey(cfg, task, name, false)
	if !codeURL.MatchString(code) {
		r.exec.mu.Lock()
		sidecar = r.exec.sidecar[key]
		r.exec.mu.Unlock()
	}
	if name != "" && !sessionName.MatchString(name) {
		return errorJSON(fmt.Sprintf("Invalid session name %s: use 1-64 letters, digits, dashes, or underscores (e.g. 'r7k2').", strconv.Quote(name)))
	}
	rt, failure := r.routeCurrent(ctx, cfg, task, name, local, sidecar, key)
	if failure != "" {
		return errorJSON(failure)
	}
	if rt.sidecar {
		rt.name = sidecarName(name)
	}
	r.exec.mu.Lock()
	r.exec.sidecar[key] = rt.sidecar
	r.exec.mu.Unlock()
	dir := workspace(cfg, task)
	timeout := clampTimeout(timeoutArg)
	started := time.Now()
	env := r.execEnv(ctx, cfg, rt, dir)
	python, err := r.packages.Python(ctx, cfg, env, "browser_exec runs Python code")
	if err != nil {
		return errorJSON(err.Error())
	}
	var proc execProc
	dispatch := func() (any, error) {
		if _, err := r.supervisors.ensure(cfg, cmp.Or(task, "default"), resolveCDP(ctx, rt.cdp)); err != nil {
			slog.Debug(fmt.Sprintf("browser_exec: CDP supervisor attach failed (non-fatal): %s", redactEndpoint(err.Error())))
		}
		proc = r.runCode(ctx, cfg, python, rt, env, code, dir, timeout, name != "" && !rt.private)
		return nil, nil
	}
	if rt.engine != "" {
		if _, refusal, _ := r.fenced(ctx, cfg, &session{features: map[string]bool{"local": true}}, dispatch); refusal != nil {
			return refusalJSON(refusal)
		}
	} else {
		dispatch()
	}
	if proc.timedOut {
		return errorJSON(fmt.Sprintf("browser_exec timed out after %ds. The browser may still be working; retry with a larger timeout_s (max %d), or "+
			"split the work into several calls that append to workspace files — anything already written to the workspace is preserved.", timeout, execMax))
	}
	if proc.failure != nil {
		return errorJSON("Failed to launch Python for browser_exec: " + proc.failure.Error())
	}
	clean := func(s string) string { return redact.Text(s, false, redact.Ends) }
	result := map[string]any{"success": proc.code == 0, "exit_code": proc.code, "output": clean(proc.stdout)}
	if dir != "" {
		result["workspace"] = dir
	}
	if name != "" {
		result["session"] = name
	}
	if note, ok := r.exec.takeNote(key); ok {
		result["real_profile_unavailable"] = note
	}
	stderr := clean(strings.TrimSpace(proc.stderr))
	if runes := []rune(stderr); len(runes) > stderrCap {
		stderr = string(runes[:stderrCap]) + "\n… (stderr truncated)"
	}
	if stderr != "" {
		result["stderr"] = stderr
	}
	if shot := screenshot(proc.stdout, started); shot != "" {
		result["screenshot_path"] = shot
		if native := r.attachShot(ctx, call, result, shot); native != "" {
			return native
		}
	}
	return orderedJSON(result, "success", "exit_code", "output", "workspace", "session", "real_profile_unavailable", "stderr", "screenshot_path")
}

// Result with the screenshot handed to a main model that sees images, empty when it reads text only
func (r *registrant) attachShot(ctx context.Context, call *tools.Call, result map[string]any, path string) string {
	if call.Env.Aux != nil && !call.Env.Aux.NativeImages(ctx) {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Debug(fmt.Sprintf("Native screenshot attach failed (falling back to text): %s", err))
		return ""
	}
	mime := "image/png"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".webp":
		mime = "image/webp"
	}
	call.Media = append(call.Media, &sessionsv1.Media{MimeType: mime, Data: data})
	result["native_vision"] = true
	text := orderedJSON(result, "success", "exit_code", "output", "workspace", "session", "real_profile_unavailable", "stderr", "screenshot_path",
		"native_vision")
	return text + "\n\nThe screenshot from this call is attached — inspect it with your native vision."
}

// JSON of a result with its keys in the order given, any others after them sorted
func orderedJSON(result map[string]any, order ...string) string {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	write := func(k string, v any) {
		if !first {
			b.WriteString(", ")
		}
		first = false
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteString(": ")
		b.WriteString(tools.JSON(v))
	}
	seen := map[string]bool{}
	for _, k := range order {
		if v, ok := result[k]; ok {
			write(k, v)
			seen[k] = true
		}
	}
	for _, k := range slices.Sorted(maps.Keys(result)) {
		if !seen[k] {
			write(k, result[k])
		}
	}
	b.WriteByte('}')
	return b.String()
}

// Headers of the browser_exec description
const (
	execHeader = "Drive a real browser for JS pages, logins, forms and clicks (Browser Use CLI). `code` runs as full Python (stdlib available) with " +
		"pre-imported browser helpers; stdout comes back in the result. Start `code` with a one-line comment describing the step for the user in " +
		"plain language, max 60 chars (e.g. `# Searching Amazon for paper towels`) — the UI shows it as the step label.\n\n" +
		"STATE: the browser session and workspace persist across calls; Python variables do NOT (fresh interpreter each call). The workspace dir " +
		"is $BH_AGENT_WORKSPACE (also `workspace` in every result); functions defined in agent_helpers.py there are auto-imported into every " +
		"call. For multi-item tasks ('all N products / every entry'), append each batch to a JSON/CSV file in the workspace, then read it back and " +
		"aggregate in code — dedupe/count/sort with Python, not in your head — and verify the collected count against what was asked before " +
		"answering.\n\n" +
		"Batch each sub-procedure (navigate, wait, extract, act) into one call — do not spend a call per action — but for long extractions prefer " +
		"several medium calls that append to workspace files over one giant call, so progress survives timeouts."
	execVision = " Screenshots are attached to your context automatically: when the exec output contains a capture_screenshot() path, the image " +
		"arrives with this tool's result and you inspect it directly with your own vision — never send browser screenshots to a separate vision tool."
	textFirst = "page_info() for state, js() for reading/extracting DOM text, fill_input(selector, text) for inputs, and " +
		"js(\"document.querySelector('…').click()\") for clicks."
	detachedShots  = " Screenshots are not attached to this tool's results."
	detachedVision = " To inspect one, pass its capture_screenshot() path to vision_analyze."
	execLightpanda = " The local engine is Lightpanda (no graphical renderer, one page per session): capture_screenshot() is unavailable, so work " +
		"text-first: " + textFirst + " Navigate with new_tab(url) exactly once, then goto_url(url) for every later navigation — a second new_tab() " +
		"fails with TargetAlreadyLoaded."
	helpersDigest = "\n\nHELPERS (pre-imported): new_tab(url) opens/navigates (use for the FIRST navigation), goto_url(url) navigates the current " +
		"tab, wait_for_load() after navigation, page_info() summarizes the current page state, js(expr) evaluates a JS expression and returns its " +
		"value (js('document.title'); wrap function bodies as js('(() => {...})()') — a bare '() => {...}' returns the function itself, uncalled), " +
		"fill_input(selector, text) types into inputs, click_at_xy(x, y) clicks viewport coordinates, capture_screenshot() saves and prints a " +
		"screenshot path, cdp('Domain.method', **kwargs) is raw CDP — cdp('Accessibility.getFullAXTree')['nodes'] lists every element's " +
		"role/name/backendDOMNodeId (filter in Python before printing; it is thousands of nodes), then cdp('DOM.getBoxModel', backendNodeId=n) " +
		"gives click coordinates. ensure_real_tab() recovers from a stale/internal tab. Login walls: never guess credentials; see the vault note " +
		"below if present, otherwise stop and ask the user."
	localAbout = "Drive the user's own local browser (an Icebath-managed copy of their real default-Chromium profile, logins/cookies included) " +
		"instead of the configured cloud browser backend. Use when the user asks to act as themselves — their accounts, their sessions. No-op when " +
		"the backend is already local. Default false."
)

// Schema of browser_exec, its description fitted to the session: Lightpanda first since it renders nothing, then whether screenshots ride
// back to the main model; the local parameter only with the user's own profile allowed
func (r *registrant) execSchema(env *tools.Env) map[string]any {
	header := execHeader + detachedShots
	switch {
	case env == nil:
	case func() bool { on, _ := r.lightpandaStatus(env.Config); return on }():
		header = execHeader + execLightpanda
	case env.Aux == nil || env.Aux.NativeImages(context.Background()):
		header = execHeader + execVision
	case r.offers(env, "vision_analyze"):
		header = execHeader + detachedShots + detachedVision
	}
	properties := map[string]any{
		"code": tools.Property("string", "Python code to execute using the pre-imported browser helpers. Use print(...) for any data you need back."),
		"session": tools.Property("string", "Named isolated browser session — its own daemon and (on cloud backends) own browser, so concurrent "+
			"tasks don't share tabs. Reuse the same name on every related call; omit for the shared default session."),
		"timeout_s": map[string]any{"type": "integer", "default": execDefault,
			"description": fmt.Sprintf("Max seconds to wait for the code to finish (default %d, max %d).", execDefault, execMax)},
	}
	if env != nil && realProfile(env.Config) {
		properties["local"] = map[string]any{"type": "boolean", "default": false, "description": localAbout}
	}
	return map[string]any{"description": header + helpersDigest, "parameters": map[string]any{"type": "object", "properties": properties,
		"required": []string{"code"}}}
}

// Whether the session is offered the tool
func (r *registrant) offers(env *tools.Env, name string) bool {
	for _, def := range r.registry.Offered(env) {
		if def.Name == name {
			return true
		}
	}
	return false
}
