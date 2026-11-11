package gatewayrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Home under ICEBATH_HOME whose hooks run in a plugin host launched by the system python3
func hooksHome(t *testing.T) (context.Context, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	if err := os.MkdirAll(filepath.Join(h, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	restore := plugins.SwapHostInterpreter(func(context.Context) (string, error) { return python, nil })
	ctx := context.Background()
	t.Cleanup(func() {
		if host, err := plugins.GetPluginManager(ctx).PluginHost(); err == nil {
			host.Shutdown()
		}
		restore()
	})
	return ctx, filepath.Join(h, "hooks")
}

// Hook dir with HOOK.yaml and handler.py like the create hook helper
func createHook(t *testing.T, hooksDir, hookName, events, handlerCode string) string {
	t.Helper()
	dir := filepath.Join(hooksDir, hookName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: " + hookName + "\ndescription: Test hook\nevents: " + events + "\n"
	if err := os.WriteFile(filepath.Join(dir, "HOOK.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "handler.py"), []byte(handlerCode), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDiscoverAndLoadLoadsValidHook(t *testing.T) {
	ctx, hooksDir := hooksHome(t)
	createHook(t, hooksDir, "my-hook", `["agent:start"]`, "def handle(event_type, context):\n    pass\n")
	reg := NewHookRegistry()
	if err := reg.discoverAndLoad(ctx); err != nil {
		t.Fatal(err)
	}
	loaded := reg.LoadedHooks()
	if len(loaded) != 1 || loaded[0]["name"] != "my-hook" {
		t.Fatalf("loaded %v", loaded)
	}
	if !reflect.DeepEqual(loaded[0]["events"], []any{"agent:start"}) {
		t.Fatalf("events %v", loaded[0]["events"])
	}
}

func TestDiscoverAndLoadSkipsNoEvents(t *testing.T) {
	ctx, hooksDir := hooksHome(t)
	dir := filepath.Join(hooksDir, "empty-hook")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HOOK.yaml"), []byte("name: empty\nevents: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "handler.py"), []byte("def handle(e, c): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewHookRegistry()
	if err := reg.discoverAndLoad(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(reg.LoadedHooks()); n != 0 {
		t.Fatalf("loaded %d hooks", n)
	}
}

// Lines the hook handler appended to its results file
func hookResults(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestEmitCallsAsyncHandler(t *testing.T) {
	ctx, hooksDir := hooksHome(t)
	results := filepath.Join(t.TempDir(), "results")
	dir := filepath.Join(hooksDir, "async-hook")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HOOK.yaml"), []byte("name: async-hook\nevents: ['agent:end']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := "import asyncio\nasync def handle(event_type, context):\n    await asyncio.sleep(0)\n" +
		"    with open(" + pyQuote(results) + ", 'a') as f:\n        f.write(event_type + '\\n')\n"
	if err := os.WriteFile(filepath.Join(dir, "handler.py"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewHookRegistry()
	if err := reg.discoverAndLoad(ctx); err != nil {
		t.Fatal(err)
	}
	reg.emit(ctx, "agent:end", map[string]any{})
	if got := hookResults(t, results); got != "agent:end\n" {
		t.Fatalf("results %q", got)
	}
}

func TestWildcardMatching(t *testing.T) {
	ctx, hooksDir := hooksHome(t)
	results := filepath.Join(t.TempDir(), "results")
	createHook(t, hooksDir, "wildcard-hook", `["command:*"]`,
		"def handle(event_type, context):\n    with open("+pyQuote(results)+", 'a') as f:\n        f.write(event_type + '\\n')\n")
	reg := NewHookRegistry()
	if err := reg.discoverAndLoad(ctx); err != nil {
		t.Fatal(err)
	}
	reg.emit(ctx, "command:reset", map[string]any{})
	if got := hookResults(t, results); got != "command:reset\n" {
		t.Fatalf("results %q", got)
	}
}

// Python string literal of s
func pyQuote(s string) string {
	out := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' || s[i] == '\'' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
}

func TestCollectsSyncReturnValues(t *testing.T) {
	reg := NewHookRegistry()
	reg.handlers["command:status"] = []hookHandler{
		func(context.Context, string, map[string]any) (any, error) {
			return map[string]any{"decision": "allow"}, nil
		},
		func(context.Context, string, map[string]any) (any, error) {
			return map[string]any{"decision": "deny", "message": "nope"}, nil
		},
	}
	got := reg.emitCollect(context.Background(), "command:status", map[string]any{})
	want := []any{map[string]any{"decision": "allow"}, map[string]any{"decision": "deny", "message": "nope"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestDropsNoneReturnValues(t *testing.T) {
	reg := NewHookRegistry()
	reg.handlers["command:x"] = []hookHandler{
		func(context.Context, string, map[string]any) (any, error) { return nil, nil },
		func(context.Context, string, map[string]any) (any, error) {
			return map[string]any{"decision": "deny"}, nil
		},
		func(context.Context, string, map[string]any) (any, error) { return nil, nil },
	}
	got := reg.emitCollect(context.Background(), "command:x", map[string]any{})
	if !reflect.DeepEqual(got, []any{map[string]any{"decision": "deny"}}) {
		t.Fatalf("got %v", got)
	}
}

// Two profile homes under one root with the plugin hosts launched by the system python3
func twoHookProfiles(t *testing.T) (string, string) {
	t.Helper()
	ctx, _ := hooksHome(t)
	root := os.Getenv("ICEBATH_HOME")
	profA, profB := filepath.Join(root, "profiles", "a"), filepath.Join(root, "profiles", "b")
	for _, p := range []string{profA, profB} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if host, err := plugins.GetPluginManager(home.WithOverride(ctx, p)).PluginHost(); err == nil {
				host.Shutdown()
			}
		})
	}
	return profA, profB
}

func TestGatewayHooksDirResolutionHooksDirFollowsOverride(t *testing.T) {
	profA, profB := twoHookProfiles(t)
	ctx := context.Background()
	aSeen := resolveHooksDir(home.WithOverride(ctx, profA))
	bSeen := resolveHooksDir(home.WithOverride(ctx, profB))
	if aSeen != filepath.Join(profA, "hooks") || bSeen != filepath.Join(profB, "hooks") || aSeen == bSeen {
		t.Fatalf("a %s b %s", aSeen, bSeen)
	}
}

func TestGatewayHooksDirResolutionDiscoverAndLoadUsesActiveProfileHooksDir(t *testing.T) {
	profA, profB := twoHookProfiles(t)
	bHookDir := filepath.Join(profB, "hooks", "only-in-b")
	if err := os.MkdirAll(bHookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bHookDir, "HOOK.yaml"), []byte("name: only-in-b\nevents: [\"agent:start\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bHookDir, "handler.py"), []byte("async def handle(event_type, context):\n    pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	names := func(ctx context.Context) []string {
		reg := NewHookRegistry()
		if err := reg.discoverAndLoad(ctx); err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, h := range reg.LoadedHooks() {
			out = append(out, h["name"].(string))
		}
		return out
	}
	ctx := context.Background()
	aHooks := names(home.WithOverride(ctx, profA))
	bHooks := names(home.WithOverride(ctx, profB))
	if slices.Contains(aHooks, "only-in-b") || !slices.Contains(bHooks, "only-in-b") {
		t.Fatalf("a %v b %v", aHooks, bHooks)
	}
}
