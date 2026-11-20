package browser

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Preamble the model's code runs under, every helper a call over the inherited pipe pair that Go answers
const helperPreamble = `import sys
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

// Preamble with the descriptors of this child's pipe pair filled in
func helperLauncher(pipe codeexec.ChildPipe) string {
	handles := "False"
	if pipe.Handles {
		handles = "True"
	}
	return strings.NewReplacer("@HANDLES@", handles, "@REPLIES@", strconv.FormatUint(uint64(pipe.Replies), 10),
		"@REQUESTS@", strconv.FormatUint(uint64(pipe.Requests), 10)).Replace(helperPreamble)
}

// execRun is one call's code with its interpreter, environment, route, workspace and tab rules
type execRun struct {
	python    string
	childEnv  map[string]string
	route     map[string]string
	code      string
	workspace string
	timeout   int
	activate  bool
	ownTab    bool
}

// execProc is what the code's process produced, the launch error when it never started
type execProc struct {
	returnCode int
	stdout     string
	stderr     string
	timedOut   bool
	launchErr  error
}

// Where capture_screenshot saves when the code names no path, BH_TMP_DIR when the process sets it
func defaultShotDir(env *tools.Env) string {
	if v := strings.TrimSpace(os.Getenv("BH_TMP_DIR")); v != "" {
		return v
	}
	return filepath.Join(cfgOf(env).homeDir(), "cache", "browser-use", "tmp")
}

// Screenshot dir of one call, the BH_TMP_DIR of its child env ahead of the process one
func callShotDir(env *tools.Env, childEnv map[string]string) string {
	if v := strings.TrimSpace(childEnv["BH_TMP_DIR"]); v != "" {
		return v
	}
	return defaultShotDir(env)
}

// Connects the session's browser, attaches a tab, then runs the code with its helper calls answered on that tab
var execRunCode = func(ctx context.Context, env *tools.Env, run execRun) execProc {
	deadline := time.Now().Add(time.Duration(run.timeout) * time.Second)
	endpoint := run.route["BU_CDP_WS"]
	if endpoint == "" {
		endpoint = ResolveCDPOverride(ctx, run.route["BU_CDP_URL"])
	}
	homeKey := cfgOf(env).homeKey()
	identity := DaemonIdentity(run.childEnv)
	if identity[4] == "" {
		identity[4] = "default"
	}
	sess := execSessionFor(strings.Join(append([]string{homeKey}, identity[:]...), "\x00"), homeKey)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	setupDeadline := time.Now().Add(execSetupTimeout)
	if deadline.Before(setupDeadline) {
		setupDeadline = deadline
	}
	setupCtx, cancelSetup := context.WithDeadline(ctx, setupDeadline)
	defer cancelSetup()
	h := &helperRun{ctx: setupCtx, s: sess, workspace: run.workspace, shotDir: callShotDir(env, run.childEnv),
		activate: run.activate, ownTab: run.ownTab}
	refuse := func(format string, args ...any) execProc {
		if time.Now().After(deadline) {
			return execProc{timedOut: true}
		}
		return execProc{returnCode: 1, stderr: "browser_exec: " + fmt.Sprintf(format, args...) + "\n"}
	}
	if endpoint == "" {
		return refuse("the browser route names no DevTools endpoint")
	}
	if err := sess.connect(setupCtx, endpoint); err != nil {
		return refuse("%s", err)
	}
	if err := h.ensureTab(); err != nil {
		sess.disconnect()
		return refuse("no tab could be attached: %s", err)
	}
	result, err := codeexec.RunChild(ctx, env, codeexec.ChildRun{
		Python:  run.python,
		Args:    func(pipe codeexec.ChildPipe) []string { return []string{"-c", helperLauncher(pipe)} },
		Stdin:   run.code,
		Env:     run.childEnv,
		Timeout: time.Until(deadline),
		Purpose: "browser-exec",
		Serve: func(serveCtx context.Context, requests io.Reader, replies io.Writer) {
			h.ctx = serveCtx
			h.serve(requests, replies)
		},
	})
	if err != nil {
		return execProc{launchErr: err}
	}
	if result.TimedOut || ctx.Err() != nil {
		sess.disconnect()
	}
	return execProc{returnCode: result.ReturnCode, stdout: result.Stdout, stderr: result.Stderr, timedOut: result.TimedOut}
}
