package codeexec

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	if BotDesktopEnv == nil {
		BotDesktopEnv = func(_ *tools.Env, base map[string]string) map[string]string { return base }
	}
}

// Replaces *target with value for the duration of the test
func cuSwap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

// Pins the config loader to a fixed tree or a load failure like patching load_config
func cuConfig(t *testing.T, tree map[string]any, fail bool) {
	t.Helper()
	cuSwap(t, &loadCUConfig, func(*tools.Env) (config.Tree, error) {
		if fail {
			return nil, errors.New("boom")
		}
		return config.Tree(tree), nil
	})
}

// cuRunCall is one argv and runOpts pair the faked subprocess runner saw
type cuRunCall struct {
	argv []string
	opts runOpts
}

// Replaces the subprocess runner with one answering stdout and recording each call
func cuFakeRun(t *testing.T, stdout func(argv []string) string, returncode int) *[]cuRunCall {
	t.Helper()
	calls := &[]cuRunCall{}
	cuSwap(t, &runQuiet, func(argv []string, _ float64, o runOpts) (*completedProcess, error) {
		*calls = append(*calls, cuRunCall{slices.Clone(argv), o})
		return &completedProcess{Args: argv, Returncode: returncode, Stdout: stdout(argv)}, nil
	})
	return calls
}

func TestCuaTelemetryPolicy(t *testing.T) {
	cuConfig(t, map[string]any{"computer_use": map[string]any{"cua_telemetry": false}}, false)
	if !cuaTelemetryDisabled(nil) {
		t.Fatal("explicit false must disable telemetry")
	}
	cuConfig(t, nil, true)
	if !cuaTelemetryDisabled(nil) {
		t.Fatal("config load failure must fail safe toward disabled")
	}
	cuConfig(t, map[string]any{"computer_use": map[string]any{"cua_telemetry": true}}, false)
	if cuaTelemetryDisabled(nil) {
		t.Fatal("opt in must leave telemetry enabled")
	}
	if _, ok := cuaDriverChildEnv(nil, map[string]string{"PATH": "/usr/bin"})[cuaTelemetryEnvVar]; ok {
		t.Fatal("opt in must leave the telemetry var untouched")
	}
	cuSwap(t, &cuaTelemetryDisabled, func(*tools.Env) bool { return true })
	env := cuaDriverChildEnv(nil, map[string]string{"PATH": "/usr/bin"})
	if env[cuaTelemetryEnvVar] != "0" || env["PATH"] != "/usr/bin" {
		t.Fatalf("disabled telemetry env %v", env)
	}
	if got := cuaDriverChildEnv(nil, map[string]string{cuaTelemetryEnvVar: "1"})[cuaTelemetryEnvVar]; got != "0" {
		t.Fatalf("disabled telemetry must override an inherited 1, got %q", got)
	}
}

func TestCuaNativeWaylandEnv(t *testing.T) {
	cuSwap(t, &cuPlatform, "linux")
	child := func(base map[string]string, native bool) map[string]string {
		cuConfig(t, map[string]any{"computer_use": map[string]any{"native_wayland": native}}, false)
		return cuaDriverChildEnv(nil, base)
	}
	if child(map[string]string{"WAYLAND_DISPLAY": "wayland-1"}, true)[cuaNativeWaylandEnvVar] != "1" {
		t.Fatal("configured native wayland must reach a wayland child")
	}
	if _, ok := child(map[string]string{"DISPLAY": ":0"}, true)[cuaNativeWaylandEnvVar]; ok {
		t.Fatal("no wayland display must not inject the bridge")
	}
	if _, ok := child(map[string]string{"WAYLAND_DISPLAY": "wayland-1"}, false)[cuaNativeWaylandEnvVar]; ok {
		t.Fatal("no opt in must not inject the bridge")
	}
}

func TestCuaNoOverlayPolicy(t *testing.T) {
	cuConfig(t, map[string]any{"computer_use": map[string]any{"no_overlay": true}}, false)
	if !cuaNoOverlay(nil) {
		t.Fatal("explicit true must override")
	}
	cuSwap(t, &cuPlatform, "darwin")
	cuConfig(t, nil, true)
	if !cuaNoOverlay(nil) {
		t.Fatal("macOS auto detect must disable the overlay after a config failure")
	}
	cuSwap(t, &cuPlatform, "linux")
	cuSwap(t, &isWSL, func() bool { return false })
	t.Setenv("DISPLAY", "")
	if !cuaNoOverlay(nil) {
		t.Fatal("headless linux must disable the overlay after a config failure")
	}
	cases := []struct {
		display, wayland, session string
		cfg                       map[string]any
		want                      bool
	}{
		{":0", "", "", map[string]any{}, true},
		{":0", "", "x11", map[string]any{}, true},
		{":0", "wayland-0", "wayland", map[string]any{}, false},
		{":0", "", "", map[string]any{"computer_use": map[string]any{"no_overlay": false}}, false},
	}
	for i, c := range cases {
		t.Setenv("DISPLAY", c.display)
		t.Setenv("WAYLAND_DISPLAY", c.wayland)
		t.Setenv("XDG_SESSION_TYPE", c.session)
		cuConfig(t, c.cfg, false)
		if got := cuaNoOverlay(nil); got != c.want {
			t.Fatalf("case %d got %v want %v", i, got, c.want)
		}
	}
}

func TestCuaDriverSupportsNoOverlayProbe(t *testing.T) {
	clearNoOverlayCache()
	t.Cleanup(clearNoOverlayCache)
	calls := cuFakeRun(t, func([]string) string {
		return "Usage: cua-driver [OPTIONS] COMMAND\n  --no-overlay  Disable cursor overlay\n"
	}, 0)
	if !cuaDriverSupportsNoOverlay(nil, "cua-driver") {
		t.Fatal("help naming the flag must report support")
	}
	if len(*calls) != 1 || (*calls)[0].opts.Env == nil || !slices.Equal((*calls)[0].argv, []string{"cua-driver", "--help"}) {
		t.Fatalf("help probe must pass an explicit sanitized env, calls %v", *calls)
	}
	cuaDriverSupportsNoOverlay(nil, "cua-driver")
	if len(*calls) != 1 {
		t.Fatal("the probe result must be cached per command")
	}
}

func TestMCPInvocationUsesResolvedCommand(t *testing.T) {
	manifest := `{"mcp_invocation":{"command":"/opt/relocated/cua-driver","args":["mcp"]}}`
	cuFakeRun(t, func([]string) string { return manifest }, 0)
	cuSwap(t, &cuaNoOverlay, func(*tools.Env) bool { return true })
	var probed []string
	cuSwap(t, &cuaDriverSupportsNoOverlay, func(_ *tools.Env, cmd string) bool {
		probed = append(probed, cmd)
		return cmd == "/opt/relocated/cua-driver"
	})
	cmd, args := resolveMCPInvocation(nil, "/usr/bin/cua-driver", 6.0)
	if cmd != "/opt/relocated/cua-driver" || !slices.Equal(probed, []string{"/opt/relocated/cua-driver"}) {
		t.Fatalf("manifest command must drive the support probe, cmd %q probed %v", cmd, probed)
	}
	if !slices.Equal(args, []string{"mcp", "--no-overlay"}) {
		t.Fatalf("args %v", args)
	}
	if got := mcpArgsWithOverlayFlag(nil, []string{"mcp"}, "/usr/bin/cua-driver"); slices.Contains(got, "--no-overlay") {
		t.Fatal("support must not leak between binaries")
	}
	original := []string{"mcp"}
	if got := mcpArgsWithOverlayFlag(nil, original, "/opt/relocated/cua-driver"); !slices.Equal(got, []string{"mcp", "--no-overlay"}) || len(original) != 1 {
		t.Fatalf("appended %v original %v", got, original)
	}
	cuSwap(t, &cuaNoOverlay, func(*tools.Env) bool { return false })
	if got := mcpArgsWithOverlayFlag(nil, []string{"mcp"}, cuaDriverDefaultCmd); !slices.Equal(got, []string{"mcp"}) {
		t.Fatalf("disabled overlay flag appended %v", got)
	}
}

func TestWSLWindowsManifestPath(t *testing.T) {
	cuSwap(t, &isWSL, func() bool { return true })
	win := `C:\Users\Fernando\AppData\Local\cua-driver\cua-driver.exe`
	want := "/mnt/c/Users/Fernando/AppData/Local/cua-driver/cua-driver.exe"
	if got := wslWindowsPathToPosix(win); got != want {
		t.Fatalf("got %q", got)
	}
	manifest, _ := json.Marshal(map[string]any{"mcp_invocation": map[string]any{"command": win, "args": []string{"mcp"}}})
	cuFakeRun(t, func([]string) string { return string(manifest) }, 0)
	clearNoOverlayCache()
	t.Cleanup(clearNoOverlayCache)
	cmd, args := resolveMCPInvocation(nil, "cua-driver", 6.0)
	if cmd != want || !slices.Equal(args, []string{"mcp"}) {
		t.Fatalf("cmd %q args %v", cmd, args)
	}
	cuSwap(t, &isWSL, func() bool { return false })
	if got := wslWindowsPathToPosix(win); got != win {
		t.Fatalf("non WSL host must keep the path, got %q", got)
	}
}

// Asserts a spawn env has no provider key, keeps PATH and disables telemetry
func cuAssertSanitized(t *testing.T, calls []cuRunCall) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("no spawn recorded")
	}
	env := calls[0].opts.Env
	if env == nil {
		t.Fatal("subprocess must receive an explicit env")
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Fatal("provider key leaked to cua-driver")
	}
	if !strings.HasSuffix(env["PATH"], "/usr/bin:/bin") {
		t.Fatalf("PATH %q", env["PATH"])
	}
	if env[cuaTelemetryEnvVar] != "0" {
		t.Fatal("telemetry not disabled")
	}
	if !calls[0].opts.HideWindow {
		t.Fatal("console window not hidden")
	}
}

func TestCuaSpawnSitesSanitizeEnv(t *testing.T) {
	env := testEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-super-secret-should-not-leak")
	t.Setenv("PATH", "/usr/bin:/bin")
	cuConfig(t, map[string]any{}, false)
	cuSwap(t, &cuaNoOverlay, func(*tools.Env) bool { return false })
	calls := cuFakeRun(t, func([]string) string { return `{"mcp_invocation": {"command": "cua-driver", "args": ["mcp"]}}` }, 0)
	if cmd, _ := resolveMCPInvocation(env, "cua-driver", 6.0); cmd != "cua-driver" {
		t.Fatalf("cmd %q", cmd)
	}
	cuAssertSanitized(t, *calls)
	calls = cuFakeRun(t, func([]string) string { return "{}" }, 0)
	if _, err := cuaDriverRuntimeContractStatus(env, "cua-driver"); err != nil {
		t.Fatal(err)
	}
	cuAssertSanitized(t, *calls)
	calls = cuFakeRun(t, func([]string) string { return "{}" }, 0)
	if _, err := permissionsRun(env, "cua-driver", 1.0, "doctor", "--json"); err != nil {
		t.Fatal(err)
	}
	cuAssertSanitized(t, *calls)
}

func TestManifestContractReason(t *testing.T) {
	full := `{"binary_version": "v0.20.1", "mcp_invocation": {"command": "cua-driver", "args": ["mcp"]}, "subcommands": [
	 {"name": "mcp", "args": [{"name": "--socket"}, {"name": "--grant"}]},
	 {"name": "serve", "args": [{"name": "--socket"}, {"name": "--permission-mode"}, {"name": "--capability-manifest"}, {"name": "--approve-capability-manifest"}, {"name": "--embedded"}]},
	 {"name": "stop", "args": [{"name": "--socket"}]}]}`
	cases := []struct{ manifest, want string }{
		{"", "driver manifest is missing or invalid"},
		{`{"binary_version": "latest"}`, "driver manifest does not report a semantic version"},
		{`{"binary_version": "0.19.9"}`, "Icebath computer use requires cua-driver 0.20.0 or newer"},
		{`{"binary_version": "0.20.0", "mcp_invocation": {"args": []}}`, "driver manifest does not provide an MCP launch command"},
		{`{"binary_version": "0.20.0-rc1", "mcp_invocation": {"args": ["mcp"]}, "subcommands": [{"name": "mcp", "args": [{"name": "--grant"}]}]}`,
			"driver manifest is missing: mcp --socket, serve --approve-capability-manifest, serve --capability-manifest, serve --embedded, serve --permission-mode, serve --socket, stop --socket"},
		{full, ""},
	}
	for i, c := range cases {
		got, err := manifestContractReason(jsonObject(c.manifest))
		if err != nil || got != c.want {
			t.Fatalf("case %d got %q err %v", i, got, err)
		}
	}
	if _, err := manifestContractReason(jsonObject(`{"binary_version": "1.0.0", "mcp_invocation": {"args": ["mcp"]}, "subcommands": 5}`)); err == nil || err.Error() != "'int' object is not iterable" {
		t.Fatalf("non iterable subcommands must raise, got %v", err)
	}
	cuFakeRun(t, func([]string) string { return full }, 0)
	status, err := cuaDriverRuntimeContractStatus(nil, "/opt/cua-driver")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(status); string(b) != `{"ready":true,"binary":"/opt/cua-driver","version":"v0.20.1","reason":""}` {
		t.Fatalf("status %s", b)
	}
	cuSwap(t, &runQuiet, func(argv []string, _ float64, _ runOpts) (*completedProcess, error) {
		return &completedProcess{Args: argv, Returncode: 2, Stderr: "warn\nunknown verb manifest\n"}, nil
	})
	status, _ = cuaDriverRuntimeContractStatus(nil, "/opt/cua-driver")
	if b, _ := json.Marshal(status); string(b) != `{"ready":false,"binary":"/opt/cua-driver","version":null,"reason":"unknown verb manifest"}` {
		t.Fatalf("failed manifest status %s", b)
	}
}

func TestStaleTCCGrantHint(t *testing.T) {
	if staleTCCGrantHint("screen_recording_capturable") != "" {
		t.Fatal("fields without a TCC row produce no hint")
	}
	got := staleTCCGrantHint("accessibility", "screen_recording")
	if !strings.Contains(got, "`tccutil reset Accessibility com.trycua.driver && tccutil reset ScreenCapture com.trycua.driver`, then `icebath computer-use permissions grant`.") {
		t.Fatalf("hint %q", got)
	}
}

func TestEmptyDiscoveryReason(t *testing.T) {
	cuSwap(t, &cuPlatform, "linux")
	cuFakeRun(t, func(argv []string) string {
		if argv[1] == "list-sessions" {
			return "  2 1000 nick seat0 tty2\n"
		}
		return "LockedHint=yes\n"
	}, 0)
	if !strings.Contains(emptyDiscoveryReason(), "LOCKED") {
		t.Fatal("a locked session must be named")
	}
	cuSwap(t, &runQuiet, func([]string, float64, runOpts) (*completedProcess, error) {
		return nil, &spawnError{errors.New("no loginctl")}
	})
	if linuxSessionLocked() != nil {
		t.Fatal("a failing probe must report unknown")
	}
	t.Setenv("DISPLAY", "")
	if got := emptyDiscoveryReason(); got != "no DISPLAY is set — X11/XWayland is not reachable from this process" {
		t.Fatalf("reason %q", got)
	}
	cuSwap(t, &cuPlatform, "win32")
	if got := emptyDiscoveryReason(); !strings.HasSuffix(got, "run `icebath computer-use doctor` (display reachability, AX capability)") {
		t.Fatalf("reason %q", got)
	}
}
