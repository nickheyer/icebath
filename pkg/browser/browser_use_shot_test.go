package browser

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/browser/harnesshost"
	"github.com/nickheyer/icebath/pkg/internal/pyhost"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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

// Venv over the host interpreter and its own Pillow holding the pinned browser-harness wheel from testdata
func harnessPython(t *testing.T) string {
	t.Helper()
	var host string
	for _, name := range []string{"python3", "python"} {
		if found, err := exec.LookPath(name); err == nil {
			host = found
			break
		}
	}
	if host == "" {
		t.Fatal("harness cases need a host Python")
	}
	real, err := filepath.EvalSymlinks(host)
	if err != nil {
		t.Fatal(err)
	}
	in, err := pyhost.Discover(real)
	if err != nil {
		t.Fatal(err)
	}
	venv := filepath.Join(t.TempDir(), "harness-venv")
	site := filepath.Join(venv, "lib", fmt.Sprintf("python%d.%d%s", in.Major, in.Minor, in.ABIFlags), "site-packages")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("home = %s\ninclude-system-site-packages = true\nversion = %d.%d.0\n", filepath.Dir(real), in.Major, in.Minor)
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(venv, "bin", "python")
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, python); err != nil {
		t.Fatal(err)
	}
	wheel, err := zip.OpenReader(filepath.Join("testdata", "browser_harness-0.1.13-py3-none-any.whl"))
	if err != nil {
		t.Fatal(err)
	}
	defer wheel.Close()
	for _, member := range wheel.File {
		target := filepath.Join(site, filepath.FromSlash(member.Name))
		if member.FileInfo().IsDir() {
			continue
		}
		r, err := member.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return python
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

// Runs code through this binary hosting the harness with the Go helpers in the given activation mode
func runHarnessShot(t *testing.T, activate string, activateWorks bool, code, daemonKind string) shotRun {
	t.Helper()
	python := harnessPython(t)
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake_transport")
	os.Mkdir(fake, 0o755)
	os.WriteFile(filepath.Join(fake, "sitecustomize.py"), []byte(fakeHarnessTransport), 0o644)
	logPath := filepath.Join(dir, "cdp.jsonl")
	shot := filepath.Join(dir, "shot.png")
	argv, err := harnessCommand([]string{python, "-m", "browser_harness.run"}, activate)
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
	if got, err := harnessCommand(other, activateNever); err != nil || !slices.Equal(got, other) {
		t.Fatalf("got %v", got)
	}
	if got, err := harnessCommand(nil, activateNever); err != nil || got != nil {
		t.Fatalf("got %v", got)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := harnessCommand([]string{"/py", "-m", "browser_harness.run"}, activateIfCloud)
	if want := []string{exe, harnesshost.Entry, "--python", "/py", "--activate", "if-cloud"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestInstalledHelpersKeepTheHermesNamesSignaturesAndDocs(t *testing.T) {
	code := "import inspect, json\nprint(json.dumps([[f.__name__, str(inspect.signature(f)), f.__doc__, f.__module__ == 'browser_harness.helpers'] for f in (capture_screenshot, new_tab)]))"
	run := runHarnessShot(t, activateAlways, true, code, "")
	run.requireSuccess(t)
	var got []any
	if err := json.Unmarshal([]byte(strings.TrimSpace(run.stdout)), &got); err != nil {
		t.Fatalf("stdout %q %v", run.stdout, err)
	}
	want := []any{
		[]any{"capture_screenshot", "(path=None, full=False, max_dim=None)", "Save a PNG of the current viewport. Set max_dim=1800 on a 2x display to keep the file under\nthe 2000px-per-side limit some image-aware LLMs enforce.", false},
		[]any{"new_tab_in_front", "(url='about:blank')", "Open (or reuse a blank) tab, navigate to url, and bring it to the front on a cloud browser.", false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("helpers %v", got)
	}
}

func TestCaptureScreenshotBindsArgumentsLikeItsDef(t *testing.T) {
	code := "for call in (lambda: capture_screenshot(1, 2, 3, 4), lambda: capture_screenshot(nope=1), lambda: capture_screenshot({shot!r}, path={shot!r})):\n" +
		"    try:\n        call()\n    except TypeError as e:\n        print(e)"
	run := runHarnessShot(t, activateNever, true, code, "")
	run.requireSuccess(t)
	want := "make_capture_screenshot.<locals>.capture_screenshot() takes from 0 to 3 positional arguments but 4 were given\n" +
		"make_capture_screenshot.<locals>.capture_screenshot() got an unexpected keyword argument 'nope'\n" +
		"make_capture_screenshot.<locals>.capture_screenshot() got multiple values for argument 'path'\n"
	if run.stdout != want {
		t.Fatalf("stdout %q", run.stdout)
	}
}

func TestHarnessExitStatusFollowsSystemExitAndUncaughtErrors(t *testing.T) {
	for _, c := range []struct {
		code   string
		status int
		stderr string
	}{
		{"raise SystemExit(7)", 7, ""},
		{"raise SystemExit('bye')", 1, "bye\n"},
		{"raise SystemExit(None)", 0, ""},
		{"raise ValueError('boom')", 1, "ValueError: boom"},
	} {
		run := runHarnessShot(t, activateNever, true, c.code, "")
		status := 0
		var exit *exec.ExitError
		if errors.As(run.err, &exit) {
			status = exit.ExitCode()
		} else if run.err != nil {
			t.Fatalf("%s: %v", c.code, run.err)
		}
		if status != c.status || !strings.Contains(run.stderr, c.stderr) {
			t.Fatalf("%s: status %d stderr %q", c.code, status, run.stderr)
		}
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
