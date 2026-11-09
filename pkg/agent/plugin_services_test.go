package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestPluginLLMClientReadsTheMainRoute(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  provider: openrouter\n  default: acme/forge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := PluginLLMClient()
	if got := client.ReadMainProvider(); got != "openrouter" {
		t.Fatalf("provider %q", got)
	}
	if got := client.ReadMainModel(); got != "acme/forge" {
		t.Fatalf("model %q", got)
	}
	task := "summarize"
	kw := pluginLLMKwargs(&plugins.AuxLLMRequest{Task: &task, Messages: []any{map[string]any{"role": "user", "content": "hi"}}, RouteInfo: map[string]string{}})
	if kw["task"] != "summarize" || kw["provider"] != nil || kw["extra_body"] != nil || kw["route_info"] == nil {
		t.Fatalf("kwargs %v", kw)
	}
}

func TestSubagentDelegationReadsParentAndToolsets(t *testing.T) {
	a, ctx, _ := brLoopAgent(t, nil)
	d := SubagentDelegationService()
	if d.SessionID(a) != a.SessionID() {
		t.Fatal("session id must come from the parent agent")
	}
	if enabled, ok := d.EnabledToolsets(a); !ok || len(enabled) != 1 || enabled[0] != "file" {
		t.Fatalf("enabled %v %v", enabled, ok)
	}
	if _, ok := d.EnabledToolsets("stranger"); ok || d.SessionID("stranger") != "" {
		t.Fatal("a foreign parent reads as None")
	}
	if !d.ValidateToolset(ctx, "terminal") || !d.ValidateToolset(ctx, "all") || d.ValidateToolset(ctx, "no-such-toolset") {
		t.Fatal("toolset validation must follow toolsets.validate_toolset")
	}
	if cfg := d.LoadConfig(ctx); cfg == nil {
		t.Fatal("the delegation section reads as a mapping")
	}
	if _, err := d.ResolveCredentials(ctx, map[string]any{}, "stranger"); err == nil {
		t.Fatal("a foreign parent cannot resolve credentials")
	}
	if _, err := d.BuildChild(ctx, plugins.SubagentChildSpec{Goal: "g", Parent: "stranger"}); err == nil {
		t.Fatal("a foreign parent builds no child")
	}
	child := subagentChild{agent: a}
	if child.SubagentID() != a.SubagentID() || child.DelegateDepth() != a.DelegateDepth() {
		t.Fatal("child attributes read the agent")
	}
	if p := child.Provider(); (a.Provider() == "") != (p == nil) || (p != nil && *p != a.Provider()) {
		t.Fatalf("provider %v", p)
	}
	if role, ok := child.DelegateRole(); !ok || role != a.delegateRole {
		t.Fatal("delegate role reads the agent field")
	}
}

func TestDelegationBackendsFacadeBindsItsMethods(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	dbRealRegistry(t)
	facade, ok := DelegationBackendsFor(nil).(plugins.HostFacade)
	if !ok {
		t.Fatal("the facade must be a host facade")
	}
	ctx := context.Background()
	if _, isMethod, async, err := facade.HostAttr(ctx, "list_backends"); err != nil || !isMethod || async {
		t.Fatal("list_backends is a sync method")
	}
	if _, _, _, err := facade.HostAttr(ctx, "nope"); err == nil {
		t.Fatal("unknown attributes raise AttributeError")
	}
	rows, err := facade.HostCall(ctx, "list_backends", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if list, ok := rows.([]any); !ok || len(list) == 0 {
		t.Fatalf("rows %v", rows)
	}
	var builtin *plugins.HostBuiltinError
	if _, err := facade.HostCall(ctx, "set_global_backend", nil, nil); !errors.As(err, &builtin) || builtin.Type != "TypeError" {
		t.Fatalf("a missing name is a TypeError, got %v", err)
	}
	if _, err := facade.HostCall(ctx, "set_global_backend", []any{"local"}, map[string]any{"bogus": 1}); !errors.As(err, &builtin) || builtin.Type != "TypeError" {
		t.Fatalf("an unknown keyword is a TypeError, got %v", err)
	}
}

func TestPlatformDeclarationsServiceRegistersParsedDeclarations(t *testing.T) {
	svc := PlatformDeclarationsService()
	t.Cleanup(func() { svc.Unregister("acme-app") })
	app := map[string]any{"linux": map[string]any{"presence": "executable", "location": "/nonexistent/acme"}}
	if err := svc.Register("acme-app", app, map[string]any{"app": true}); err != nil {
		t.Fatal(err)
	}
	decl := tools.LookupPlatformDeclaration("acme-app")
	if decl == nil || decl.Name != "acme-app" || !decl.RequiresApp() {
		t.Fatalf("declaration %+v", decl)
	}
	svc.Unregister("acme-app")
	if tools.LookupPlatformDeclaration("acme-app") != nil {
		t.Fatal("unregister removes the declaration")
	}
	if err := svc.Register("broken", map[string]any{"linux": "not a mapping"}, nil); err == nil {
		t.Fatal("a malformed declaration is refused")
	}
}
