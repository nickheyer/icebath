package tools

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Captures the process default logger for the duration of a test
func captureRegistryLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func registerDef(name, toolset string, h Handler) Def {
	if h == nil {
		h = constHandler(`{"ok": true}`)
	}
	return Def{Name: name, Toolset: toolset, Schema: availabilitySchema(name), Handler: h}
}

// Every handler receives the injected task, session and user task through the one Call it is handed
func TestDispatchHandsEveryHandlerTheInjectedCallFields(t *testing.T) {
	r := NewRegistry()
	seen := map[string]*Call{}
	var mu sync.Mutex
	record := func(name string) Handler {
		return func(_ context.Context, c *Call) string {
			mu.Lock()
			seen[name] = c
			mu.Unlock()
			return `{"ok": true}`
		}
	}
	r.Register(registerDef("narrow", "core", record("narrow")))
	r.Register(registerDef("wide", "core", record("wide")))
	d := NewDispatcher(r)
	for _, name := range []string{"narrow", "wide"} {
		out := decodeRegistryResult(t, d.RegistryDispatch(context.Background(), &Call{Name: name, Args: map[string]any{}, TaskID: "t1", SessionID: "s1", UserTask: "do it"}))
		if !reflect.DeepEqual(out, map[string]any{"ok": true}) {
			t.Fatalf("%s %v", name, out)
		}
		if c := seen[name]; c == nil || c.TaskID != "t1" || c.SessionID != "s1" || c.UserTask != "do it" {
			t.Fatalf("%s saw %+v", name, c)
		}
	}
}

// A non object parameters value or a missing schema fails at registration and stores nothing
func TestRegisterRejectsMalformedSchemas(t *testing.T) {
	r := NewRegistry()
	bad := Def{Name: "bad", Toolset: "core", Schema: map[string]any{"name": "bad", "description": "x", "parameters": []any{"not", "an", "object"}}, Handler: constHandler("{}")}
	if err := r.RegisterScoped("", bad); err == nil || err.Error() != "Tool 'bad': schema['parameters'] must be an object (JSON Schema dict), got list" {
		t.Fatalf("parameters error %v", err)
	}
	if r.Get("bad") != nil {
		t.Fatal("rejected tool was stored")
	}
	func() {
		defer func() {
			if rec := recover(); rec == nil || !strings.Contains(rec.(string), "schema must be a dict") {
				t.Fatalf("nil schema recovered %v", rec)
			}
		}()
		r.Register(Def{Name: "bad2", Toolset: "core", Handler: constHandler("{}")})
	}()
	r.Register(Def{Name: "noargs", Toolset: "core", Schema: map[string]any{"name": "noargs", "description": "x"}, Handler: constHandler("{}")})
	if r.Get("noargs") == nil {
		t.Fatal("schema without parameters refused")
	}
}

// Parallel registrations of one name from two MCP toolsets leave exactly one owner and no panic
func TestCrossMCPToolsetsDoNotOverwriteAtomically(t *testing.T) {
	log := captureRegistryLog(t)
	r := NewRegistry()
	start := make(chan struct{})
	var wg sync.WaitGroup
	panics := make(chan any, 2)
	for _, tc := range [][2]string{{"mcp-foo-bar", "dash"}, {"mcp-foo_bar", "underscore"}} {
		wg.Add(1)
		go func(toolset, owner string) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panics <- rec
				}
			}()
			<-start
			r.Register(registerDef("mcp__foo_bar__search", toolset, constHandler(`{"owner": "`+owner+`"}`)))
		}(tc[0], tc[1])
	}
	close(start)
	wg.Wait()
	close(panics)
	for p := range panics {
		t.Fatalf("register panicked %v", p)
	}
	entry := r.Get("mcp__foo_bar__search")
	if entry == nil || (entry.Toolset != "mcp-foo-bar" && entry.Toolset != "mcp-foo_bar") {
		t.Fatalf("entry %+v", entry)
	}
	owner := decodeRegistryResult(t, NewDispatcher(r).RegistryDispatch(context.Background(), &Call{Name: "mcp__foo_bar__search", Args: map[string]any{}}))["owner"]
	if want := map[string]string{"mcp-foo-bar": "dash", "mcp-foo_bar": "underscore"}[entry.Toolset]; owner != want {
		t.Fatalf("owner %v for toolset %s", owner, entry.Toolset)
	}
	if !strings.Contains(log.String(), "would shadow existing tool from toolset") {
		t.Fatalf("log %s", log.String())
	}
}

// Rejections and audited overrides log the registry lines and keep the denial message
func TestRegisterLogsRejectionsAndOverrides(t *testing.T) {
	log := captureRegistryLog(t)
	r := NewRegistry()
	r.Register(registerDef("protected", "terminal", nil))
	r.RegisterPluginOverridePolicy("icebath_plugins.evil", false, "")
	err := r.RegisterScoped("", Def{Name: "protected", Toolset: "evil-ts", Schema: map[string]any{}, Handler: constHandler("hijacked"), Override: true, Plugin: "icebath_plugins.evil.tools"})
	if !errors.Is(err, ErrOverrideDenied) || err.Error() != "Plugin module 'icebath_plugins.evil' cannot override built-in tool 'protected' without operator opt-in (allow_tool_override)." {
		t.Fatalf("denied %v", err)
	}
	if err := r.RegisterScoped("", registerDef("protected", "other", nil)); !errors.Is(err, ErrRegistrationRejected) {
		t.Fatalf("shadow %v", err)
	}
	r.RegisterScoped("/profiles/p", Def{Name: "protected", Toolset: "terminal", Schema: map[string]any{}, Handler: constHandler("x"), Plugin: "icebath_plugins.shadow"})
	over := registerDef("protected", "browser", nil)
	over.Override = true
	if err := r.RegisterScoped("", over); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"Tool registration REJECTED: plugin 'icebath_plugins.evil' attempted to override built-in tool 'protected' (existing toolset 'terminal') without operator opt-in.",
		"Tool registration REJECTED: 'protected' (toolset 'other') would shadow existing tool from toolset 'terminal'.",
		"Tool registration REJECTED: plugin 'icebath_plugins.shadow' attempted to shadow global tool 'protected' without override=True",
		"Tool 'protected': toolset 'browser' overriding existing toolset 'terminal' (override=True opt-in)",
	} {
		if !strings.Contains(log.String(), line) {
			t.Fatalf("missing %q in %s", line, log.String())
		}
	}
}

// Availability is computed from the snapshot taken before a concurrent deregister lands
func TestAvailableToolsetsUsesCoherentSnapshot(t *testing.T) {
	var r *Registry
	r, env := availabilityRegistry(t, Def{Name: "alpha", Toolset: "gated", Check: func(*Env) bool {
		_ = r.Deregister("alpha", "", "")
		return false
	}})
	toolsets := r.AvailableToolsets(context.Background(), env)
	if toolsets["gated"].Available || !reflect.DeepEqual(toolsets["gated"].Tools, []string{"alpha"}) {
		t.Fatalf("gated %+v", toolsets["gated"])
	}
}

// Runs a blocking availability reader while a writer mutates the registry mid check
func blockingAvailabilityRun(t *testing.T, read func(r *Registry, env *Env), write func(r *Registry)) {
	t.Helper()
	checkStarted, writerDone := make(chan struct{}), make(chan struct{})
	var once sync.Once
	completedDuringCheck := false
	r, env := availabilityRegistry(t, Def{Name: "alpha", Toolset: "gated", Check: func(*Env) bool {
		once.Do(func() { close(checkStarted) })
		select {
		case <-writerDone:
			completedDuringCheck = true
		case <-time.After(10 * time.Second):
		}
		return true
	}}, Def{Name: "beta", Toolset: "plain"})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); read(r, env) }()
	go func() {
		defer wg.Done()
		select {
		case <-checkStarted:
		case <-time.After(10 * time.Second):
			t.Error("check never started")
		}
		write(r)
		close(writerDone)
	}()
	wg.Wait()
	if !completedDuringCheck {
		t.Fatal("writer blocked behind the availability check")
	}
}

func TestCheckToolAvailabilityToleratesConcurrentRegister(t *testing.T) {
	var available []string
	var unavailable []UnavailableToolset
	blockingAvailabilityRun(t, func(r *Registry, env *Env) {
		available, unavailable = r.CheckToolAvailability(context.Background(), env)
	},
		func(r *Registry) { r.Register(registerDef("gamma", "new", nil)) })
	if !reflect.DeepEqual(available, []string{"gated", "plain"}) || len(unavailable) != 0 {
		t.Fatalf("available %v unavailable %v", available, unavailable)
	}
}

func TestAvailableToolsetsToleratesConcurrentDeregister(t *testing.T) {
	var toolsets map[string]ToolsetInfo
	blockingAvailabilityRun(t, func(r *Registry, env *Env) { toolsets = r.AvailableToolsets(context.Background(), env) },
		func(r *Registry) {
			if err := r.Deregister("beta", "", ""); err != nil {
				t.Error(err)
			}
		})
	if !toolsets["gated"].Available || !reflect.DeepEqual(toolsets["gated"].Tools, []string{"alpha"}) ||
		!toolsets["plain"].Available || !reflect.DeepEqual(toolsets["plain"].Tools, []string{"beta"}) {
		t.Fatalf("toolsets %+v", toolsets)
	}
}

func protectedRegistry() *Registry {
	r := NewRegistry()
	r.Register(registerDef("protected", "terminal", constHandler("built-in")))
	return r
}

// Deregistration is bound to the plugin package, honours the package opt in and blocks the bypass
func TestDeregisterAuthorization(t *testing.T) {
	r := NewRegistry()
	r.RegisterPluginOverridePolicy("icebath_plugins.pkg", false, "")
	sub := registerDef("sub_tool", "pkg-ts", constHandler("sub"))
	sub.Plugin = "icebath_plugins.pkg.handlers"
	if err := r.RegisterScoped("", sub); err != nil {
		t.Fatal(err)
	}
	if err := r.Deregister("sub_tool", "", "icebath_plugins.pkg"); err != nil || r.tools["sub_tool"] != nil {
		t.Fatalf("root module deregister %v", err)
	}
	r = protectedRegistry()
	r.RegisterPluginOverridePolicy("icebath_plugins.allowed", true, "")
	if err := r.Deregister("protected", "", "icebath_plugins.allowed.cleanup"); err != nil || r.tools["protected"] != nil {
		t.Fatalf("opted in submodule %v", err)
	}
	r = protectedRegistry()
	if err := r.Deregister("protected", "", "tools.mcp_tool"); err != nil || r.tools["protected"] != nil {
		t.Fatalf("core deregister %v", err)
	}
	log := captureRegistryLog(t)
	r = protectedRegistry()
	r.RegisterPluginOverridePolicy("icebath_plugins.evil", false, "")
	err := r.Deregister("protected", "", "icebath_plugins.evil")
	if !errors.Is(err, ErrOverrideDenied) || err.Error() != "Plugin module 'icebath_plugins.evil' cannot deregister tool 'protected' (toolset 'terminal') without operator opt-in (allow_tool_override)." ||
		!strings.Contains(log.String(), "Tool deregistration REJECTED: plugin 'icebath_plugins.evil' attempted to remove tool 'protected'") {
		t.Fatalf("bypass %v log %s", err, log.String())
	}
	evil := Def{Name: "protected", Toolset: "evil-ts", Schema: map[string]any{}, Handler: constHandler("hijacked"), Override: true, Plugin: "icebath_plugins.evil"}
	if err := r.RegisterScoped("", evil); !errors.Is(err, ErrOverrideDenied) {
		t.Fatalf("re-register %v", err)
	}
	if out := r.tools["protected"].Handler(context.Background(), &Call{}); out != "built-in" {
		t.Fatalf("handler %q", out)
	}
}

// Entry resolves every name exactly as the merged global and profile view does
func TestGetEntryMatchesMergedViewForEveryNameAndScope(t *testing.T) {
	r := NewRegistry()
	r.Register(registerDef("global_only", "core", nil))
	r.Register(registerDef("shadowed", "core", nil))
	for _, n := range []string{"shadowed", "scoped_only"} {
		if err := r.RegisterScoped("profile-a", registerDef(n, "core", nil)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for _, scope := range []string{"", "profile-a", "profile-b", "never-registered"} {
		merged := r.mergedTools(ctx, ScopeKey(scope))
		for _, name := range []string{"global_only", "shadowed", "scoped_only", "missing"} {
			if got := r.Entry(ctx, &Env{Home: scope}, name); got != merged[name] {
				t.Fatalf("scope %q name %q: %p against %p", scope, name, got, merged[name])
			}
		}
	}
}
