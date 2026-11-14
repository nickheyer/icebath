package plugins

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Builds the test plugin from its Go source in this tree into dir, as a WebAssembly reactor or a native program
func buildGuestPlugin(t *testing.T, dir string, wasm bool) string {
	t.Helper()
	gobin := filepath.Join(os.Getenv("GOROOT"), "bin", "go")
	if !isFile(gobin) {
		found, err := exec.LookPath("go")
		if err != nil {
			t.Fatalf("building the test plugin needs the go command: %v", err)
		}
		gobin = found
	}
	name, args, env := "guestplugin", []string{"build"}, append(os.Environ(), "GOPROXY=off", "CGO_ENABLED=0")
	if wasm {
		name, args, env = "plugin.wasm", append(args, "-buildmode=c-shared"), append(env, "GOOS=wasip1", "GOARCH=wasm")
	} else if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command(gobin, append(args, "-o", filepath.Join(dir, name), "./testdata/guestplugin")...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build of the test plugin failed: %v\n%s", err, out)
	}
	return name
}

// Home with the test plugin installed in one form and loaded by a fresh manager
func guestHome(t *testing.T, wasm bool, callSeconds string) (string, *Manager) {
	t.Helper()
	installHostEnvHooks()
	previous := installedServices()
	InstallServices(&Services{DispatchTool: func(ctx context.Context, scope, name string, args, _ map[string]any) (string, error) {
		return tools.Default.Lookup(ctx, scope, name).Handler(ctx, &tools.Call{Name: name, Args: args}), nil
	}})
	t.Cleanup(func() { InstallServices(previous) })
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	dir := filepath.Join(h, "plugins", "guestdemo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := buildGuestPlugin(t, dir, wasm)
	form := RuntimeProgram
	if wasm {
		form = RuntimeWasm
	}
	manifest := "name: guestdemo\nversion: '1.0'\nkind: standalone\nruntime: " + form + "\nentry: " + entry +
		"\ngrants:\n  write: [\"$PLUGIN_DATA\"]\nlimits:\n  memory_mb: 128\n  call_seconds: " + callSeconds + "\n"
	for path, body := range map[string]string{filepath.Join(dir, "plugin.yaml"): manifest,
		filepath.Join(h, "config.yaml"): "plugins:\n  enabled: [guestdemo]\n", filepath.Join(root, "outside.txt"): "private"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	m := NewManager(h)
	t.Cleanup(func() {
		for _, host := range m.GuestHosts() {
			host.Shutdown()
		}
	})
	m.DiscoverAndLoad(homeCtx(h), false)
	m.mu.RLock()
	loaded := m.plugins["guestdemo"]
	m.mu.RUnlock()
	if loaded == nil || !loaded.Enabled {
		t.Fatalf("the %s plugin did not load: %+v", form, loaded)
	}
	return h, m
}

// Calls a registered tool through the tool registry the agent dispatches from
func guestTool(t *testing.T, ctx context.Context, h, name string, args map[string]any) string {
	t.Helper()
	def := tools.Default.Lookup(homeCtx(h), h, name)
	if def == nil {
		t.Fatalf("tool %s is not registered", name)
	}
	return def.Handler(ctx, &tools.Call{Name: name, Args: args})
}

func guestFields(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("tool answered %q, not a JSON object: %v", raw, err)
	}
	return out
}

// Registration, tool dispatch, a nested call, a hook, granted files, a widget and cancellation of one loaded guest
func exerciseGuest(t *testing.T, h string, m *Manager) {
	t.Helper()
	ctx := homeCtx(h)
	if got := guestFields(t, guestTool(t, ctx, h, "guest_echo", map[string]any{"text": "hello"})); got["echo"] != "HELLO" || got["plugin"] != "guestdemo" {
		t.Fatalf("guest_echo answered %v", got)
	}
	if got := guestFields(t, guestTool(t, ctx, h, "guest_relay", map[string]any{"text": "nested"})); got["echo"] != "NESTED" {
		t.Fatalf("guest_relay, a tool that calls a tool of its own plugin through icebath, answered %v", got)
	}
	m.InvokeHook(ctx, "post_tool_call", map[string]any{"tool_name": "read_file", "args": map[string]any{}, "result": "ok"})
	if got := guestFields(t, guestTool(t, ctx, h, "guest_state", nil)); got["hooks"] != float64(1) {
		t.Fatalf("the post_tool_call hook ran %v times, want 1", got["hooks"])
	}
	if got := guestTool(t, ctx, h, "guest_note", map[string]any{"text": "kept"}); got != "kept" {
		t.Fatalf("guest_note answered %q", got)
	}
	if data, err := os.ReadFile(filepath.Join(h, "plugin-data", "guestdemo", "note.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("the granted write left %q, %v", data, err)
	}
	outside := filepath.Join(filepath.Dir(h), "outside.txt")
	if got := guestTool(t, ctx, h, "guest_peek", map[string]any{"path": outside}); !strings.Contains(got, "grants no read access") {
		t.Fatalf("a read outside the manifest grants answered %q", got)
	}
	widgets := m.Widgets()
	if len(widgets) != 1 || widgets[0].Spec.ID != "guest-clock" || widgets[0].Plugin != "guestdemo" || widgets[0].Spec.Zone != "top-right" {
		t.Fatalf("registered widgets are %+v", widgets)
	}
	state, ok, err := widgets[0].Spec.Open(ctx, "12:00")
	if err != nil || !ok {
		t.Fatalf("widget open answered %v, %v, %v", state, ok, err)
	}
	view, err := widgets[0].Spec.Render(ctx, WidgetFrame{Cols: 20, Rows: 1, State: state})
	if err != nil || len(view.Lines) != 1 || len(view.Lines[0].Spans) != 1 || view.Lines[0].Spans[0] != (WidgetSpan{Text: "12:00", Role: "accent", Bold: true}) {
		t.Fatalf("widget render answered %+v, %v", view, err)
	}
	waiting, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if got := guestTool(t, waiting, h, "guest_wait", nil); !strings.Contains(got, "error") {
		t.Fatalf("a cancelled call answered %q", got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := guestFields(t, guestTool(t, ctx, h, "guest_state", nil)); got["cancelled"] == float64(1) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the guest never saw its caller's context end")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWebAssemblyPluginRegistersAndServesThroughTheToolRegistry(t *testing.T) {
	h, m := guestHome(t, true, "30")
	exerciseGuest(t, h, m)
	if hosts := m.GuestHosts(); len(hosts) != 1 || hosts[0].PID() != 0 || hosts[0].Info()["form"] != "wasm" {
		t.Fatalf("the WebAssembly guest runs outside this process or reports another form")
	}
	if got := guestTool(t, homeCtx(h), h, "guest_peek", map[string]any{"path": "/etc/hostname"}); !strings.Contains(got, "grants no read access") {
		t.Fatalf("a WebAssembly guest read a file its manifest never granted: %q", got)
	}
}

func TestWebAssemblyPluginPastItsCallLimitIsRebuilt(t *testing.T) {
	h, m := guestHome(t, true, "2")
	ctx := homeCtx(h)
	started := time.Now()
	if got := guestTool(t, ctx, h, "guest_spin", nil); !strings.Contains(got, "error") {
		t.Fatalf("a call that never returns answered %q", got)
	}
	if spent := time.Since(started); spent < 2*time.Second || spent > 20*time.Second {
		t.Fatalf("the call limit of 2s ended the call after %s", spent)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		m.mu.RLock()
		loaded := m.plugins["guestdemo"]
		m.mu.RUnlock()
		if def := tools.Default.Lookup(ctx, h, "guest_echo"); def != nil && loaded != nil && loaded.Enabled && m.GuestHosts()[0].Alive() {
			if got := def.Handler(ctx, &tools.Call{Name: "guest_echo", Args: map[string]any{"text": "again"}}); strings.Contains(got, "AGAIN") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the plugin was not reloaded into a fresh instance")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestExternalProgramPluginRegistersAndServesOverStdio(t *testing.T) {
	h, m := guestHome(t, false, "30")
	exerciseGuest(t, h, m)
	host := m.GuestHosts()[0]
	pid := host.PID()
	if pid == 0 || pid == os.Getpid() || host.Info()["form"] != "program" {
		t.Fatalf("the program guest reports pid %d and form %v", pid, host.Info()["form"])
	}
	m.Unload("guestdemo")
	if host.Alive() {
		t.Fatal("unloading the plugin left its program running")
	}
	if def := tools.Default.Lookup(homeCtx(h), h, "guest_echo"); def != nil {
		t.Fatal("unloading the plugin left its tool registered")
	}
}
