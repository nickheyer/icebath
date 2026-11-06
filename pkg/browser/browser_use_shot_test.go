package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// sitecustomize on the harness PYTHONPATH, the real browser_harness CLI runs with only its daemon transport faked
const fakeHarnessTransport = `import base64, io, json, os
from PIL import Image
import browser_harness.helpers as h

LOG = os.environ["FAKE_CDP_LOG"]
state = {"hidden": True, "activate_works": os.environ["FAKE_ACTIVATE_WORKS"] == "1"}

def log(entry):
    with open(LOG, "a") as f:
        f.write(json.dumps(entry) + "\n")

def image(fmt):
    buf = io.BytesIO()
    Image.new("RGB", (8, 6), "red").save(buf, format=fmt.upper())
    return base64.b64encode(buf.getvalue()).decode()

def cdp(method, session_id=None, _response_timeout=5.0, **params):
    log({"method": method, "timeout": _response_timeout, **params})
    if method == "Runtime.evaluate":
        return {"result": {"value": "hidden" if state["hidden"] else "visible"}}
    if method == "Target.activateTarget":
        if not state["activate_works"]:
            raise RuntimeError("Target.activateTarget wasn't found")
        state["hidden"] = False
        return {}
    if method == "Target.createTarget":
        return {"targetId": "T2"}
    if method == "Target.attachToTarget":
        return {"sessionId": "S2"}
    if method == "Page.captureScreenshot":
        if params.get("format") == "png" and state["hidden"]:
            raise h._IPCResponseTimeout(f"{method} timed out after {_response_timeout:g}s")
        return {"data": image(params["format"])}
    raise AssertionError(method)

h.cdp = cdp
h._send = lambda req: {}
h.current_tab = lambda: {"targetId": "T1", "target_id": "T1", "url": "about:blank", "title": ""}
import browser_harness.admin as admin, browser_harness.telemetry as telemetry
admin.ensure_daemon = admin.print_update_banner = lambda *a, **k: None
admin.daemon_alive = lambda *a, **k: True
admin.daemon_browser_kind = lambda *a, **k: os.environ.get("FAKE_DAEMON_KIND") or None
telemetry.is_enabled = lambda: False
telemetry.capture_cli_event = lambda *a, **k: None
`

var harnessEnv struct {
	once   sync.Once
	python string
	err    error
}

// Interpreter of the pm managed environment holding the pinned browser-harness and the Pillow it pins
func harnessPython(t *testing.T) string {
	t.Helper()
	harnessEnv.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		harnessEnv.python, harnessEnv.err = pm.EnsureEnvironment(ctx, pm.EnsureEnvironmentOptions{
			Name: "browser-harness", Requirements: []string{"browser-harness==0.1.13"},
			Explicit: true, Timeout: 20 * time.Minute})
	})
	if harnessEnv.err != nil {
		t.Fatal(harnessEnv.err)
	}
	return harnessEnv.python
}

type shotRun struct {
	stdout, stderr string
	err            error
	shot           string
	calls          []map[string]any
}

func (r shotRun) methods() []string {
	var out []string
	for _, c := range r.calls {
		out = append(out, c["method"].(string))
	}
	return out
}

func (r shotRun) screenshots() []map[string]any {
	var out []map[string]any
	for _, c := range r.calls {
		if c["method"] == "Page.captureScreenshot" {
			out = append(out, c)
		}
	}
	return out
}

func (r shotRun) requireSuccess(t *testing.T) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("harness failed %v\nstdout %s\nstderr %s", r.err, r.stdout, r.stderr)
	}
}

func (r shotRun) requirePNG(t *testing.T) {
	t.Helper()
	f, err := os.Open(r.shot)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, format, err := image.DecodeConfig(f); err != nil || format != "png" {
		t.Fatalf("shot format %q err %v", format, err)
	}
}

// Runs code through the harness launcher with the embedded helpers in the given activation mode
func runHarnessShot(t *testing.T, activate string, activateWorks bool, code, daemonKind string) shotRun {
	t.Helper()
	python := harnessPython(t)
	dir := t.TempDir()
	module, err := WriteBrowserUseShot(filepath.Join(dir, "helpers"))
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake_transport")
	os.Mkdir(fake, 0o755)
	os.WriteFile(filepath.Join(fake, "sitecustomize.py"), []byte(fakeHarnessTransport), 0o644)
	logPath := filepath.Join(dir, "cdp.jsonl")
	shot := filepath.Join(dir, "shot.png")
	argv, err := harnessCommand([]string{python, "-m", "browser_harness.run"}, activate, module)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != "BU_NAME" && k != "BU_AUTOSPAWN" && k != "PYTHONPATH" {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "PYTHONPATH="+fake, "FAKE_CDP_LOG="+logPath, "FAKE_DAEMON_KIND="+daemonKind,
		"FAKE_ACTIVATE_WORKS="+map[bool]string{true: "1", false: "0"}[activateWorks],
		"BU_CDP_WS=ws://127.0.0.1:47000/devtools/browser/t")
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(code, "{shot!r}", pyrepr.StrRepr(shot)))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	run := shotRun{err: cmd.Run(), shot: shot}
	run.stdout, run.stderr = stdout.String(), stderr.String()
	if data, err := os.ReadFile(logPath); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil {
				run.calls = append(run.calls, entry)
			}
		}
	}
	return run
}

func TestHarnessCommandWrapsOnlyTheHarnessModule(t *testing.T) {
	other := []string{"/usr/bin/python3", "-m", "other"}
	if got, err := harnessCommand(other, activateNever, "/x/browser_use_shot.py"); err != nil || !slices.Equal(got, other) {
		t.Fatalf("got %v", got)
	}
	if got, err := harnessCommand(nil, activateNever, "/x/browser_use_shot.py"); err != nil || got != nil {
		t.Fatalf("got %v", got)
	}
	got, err := harnessCommand([]string{"/py", "-m", "browser_harness.run"}, activateIfCloud, "/x/it's/browser_use_shot.py")
	want := "import importlib.util, runpy\n" +
		"spec = importlib.util.spec_from_file_location('icebath_browser_use_shot', \"/x/it's/browser_use_shot.py\")\n" +
		"shot = importlib.util.module_from_spec(spec); spec.loader.exec_module(shot)\n" +
		"import browser_harness.helpers\n" +
		"shot.install(browser_harness.helpers, activate='if-cloud')\n" +
		"runpy.run_module('browser_harness.run', run_name='__main__', alter_sys=True)\n"
	if err != nil || len(got) != 3 || got[0] != "/py" || got[1] != "-c" || got[2] != want {
		t.Fatalf("got %q", got)
	}
}

func TestWriteBrowserUseShotWritesTheEmbeddedModule(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	path, err := WriteBrowserUseShot(dir)
	if err != nil || path != filepath.Join(dir, "browser_use_shot.py") {
		t.Fatalf("path %q err %v", path, err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, browserUseShotSource) || !bytes.Contains(data, []byte(`logging.getLogger("icebath_browser_use_shot")`)) {
		t.Fatal("written module differs from the embedded one")
	}
	os.WriteFile(path, []byte("stale"), 0o644)
	if _, err := WriteBrowserUseShot(dir); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); !bytes.Equal(data, browserUseShotSource) {
		t.Fatal("stale module not replaced")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left %v", entries)
	}
}

func TestHiddenTabScreenshotOnTheUsersChromeNeverSwitchesTheirTab(t *testing.T) {
	for _, code := range []string{"print(capture_screenshot({shot!r}))", "from pathlib import Path\nprint(capture_screenshot(Path({shot!r})))"} {
		run := runHarnessShot(t, activateNever, true, code, "")
		run.requireSuccess(t)
		run.requirePNG(t)
		if slices.Contains(run.methods(), "Target.activateTarget") {
			t.Fatalf("activated the user's tab %v", run.calls)
		}
		shots := run.screenshots()
		if len(shots) != 2 || shots[0]["format"] != "png" || shots[1]["format"] != "jpeg" || shots[0]["timeout"].(float64) > 10 {
			t.Fatalf("shots %v", shots)
		}
	}
}

func TestHiddenTabScreenshotOnACloudBrowserIsBroughtToFront(t *testing.T) {
	for _, works := range []bool{true, false} {
		run := runHarnessShot(t, activateAlways, works, "print(capture_screenshot({shot!r}))", "")
		run.requireSuccess(t)
		run.requirePNG(t)
		shots := run.screenshots()
		if len(shots) == 0 || shots[0]["format"] != "png" || shots[0]["timeout"].(float64) > 10 {
			t.Fatalf("shots %v", shots)
		}
		methods := run.methods()
		if slices.Index(methods, "Target.activateTarget") < 0 || slices.Index(methods, "Target.activateTarget") > slices.Index(methods, "Page.captureScreenshot") {
			t.Fatalf("activation not before the capture %v", methods)
		}
		if works && len(shots) != 1 {
			t.Fatalf("activated tab needed %d captures", len(shots))
		}
		if !works && (len(shots) != 2 || shots[1]["format"] != "jpeg") {
			t.Fatalf("unsupported activation shots %v", shots)
		}
	}
}

func activations(run shotRun) []map[string]any {
	var out []map[string]any
	for _, c := range run.calls {
		if c["method"] == "Target.activateTarget" {
			out = append(out, c)
		}
	}
	return out
}

func TestNewTabIsBroughtToFrontOnlyOnACloudBrowser(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		mode := map[bool]string{true: activateAlways, false: activateNever}[cloud]
		run := runHarnessShot(t, mode, true, "print('TAB', new_tab())", "")
		run.requireSuccess(t)
		if !strings.Contains(run.stdout, "TAB T2") {
			t.Fatalf("stdout %q", run.stdout)
		}
		got := activations(run)
		if cloud && (len(got) != 1 || got[0]["targetId"] != "T2" || got[0]["timeout"] != 5.0 || len(got[0]) != 3) {
			t.Fatalf("cloud activations %v", got)
		}
		if !cloud && len(got) != 0 {
			t.Fatalf("local activations %v", got)
		}
	}
}

func TestCloudNewTabSurvivesAProviderRejectingActivation(t *testing.T) {
	run := runHarnessShot(t, activateAlways, false, "print('TAB', new_tab())", "")
	run.requireSuccess(t)
	if got := activations(run); !strings.Contains(run.stdout, "TAB T2") || len(got) != 1 || got[0]["targetId"] != "T2" {
		t.Fatalf("stdout %q activations %v", run.stdout, got)
	}
}

func TestAutospawnRouteActivatesOnlyWhenTheHarnessReachedCloud(t *testing.T) {
	for _, kind := range []string{"local", "cloud"} {
		run := runHarnessShot(t, activateIfCloud, true, "print(capture_screenshot({shot!r})); print('TAB', new_tab())", kind)
		run.requireSuccess(t)
		var targets []string
		for _, a := range activations(run) {
			targets = append(targets, a["targetId"].(string))
		}
		want := []string(nil)
		if kind == "cloud" {
			want = []string{"T1", "T2"}
		}
		if !slices.Equal(targets, want) {
			t.Fatalf("%s activations %v calls %v", kind, targets, run.calls)
		}
	}
}
