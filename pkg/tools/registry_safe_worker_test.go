package tools

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

func withSafeWorker(t *testing.T, on bool) {
	prev := safeWorkerEnabled
	safeWorkerEnabled = func() bool { return on }
	t.Cleanup(func() { safeWorkerEnabled = prev })
}

func TestDiscoverBuiltinToolsSafeWorkerClosure(t *testing.T) {
	withSafeWorker(t, false)
	ordinary := DiscoverBuiltinTools()
	for _, m := range []string{"tools.file_tools", "tools.clarify", "tools.process_manage"} {
		if !slices.Contains(ordinary, m) {
			t.Fatalf("ordinary discovery lacks %s: %v", m, ordinary)
		}
	}
	if Default.Get("clarify") == nil {
		t.Fatal("clarify hidden without a safe worker policy")
	}
	withSafeWorker(t, true)
	got := make(chan []string, 1)
	go func() { got <- DiscoverBuiltinTools() }()
	if mods := <-got; !reflect.DeepEqual(mods, []string{"tools.file_tools", "tools.terminal_tool", "tools.process_registry"}) {
		t.Fatalf("safe worker modules %v", mods)
	}
	for name := range safeWorkerTools {
		if Default.Get(name) == nil || Default.Lookup(context.Background(), "", name) == nil {
			t.Fatalf("core tool %s hidden under safe worker", name)
		}
	}
	if Default.Get("clarify") != nil || Default.Lookup(context.Background(), "", "clarify") != nil || Default.SnapshotRegistration("", "clarify") != nil {
		t.Fatal("clarify visible under safe worker")
	}
	if slices.Contains(Default.Names(), "clarify") {
		t.Fatal("clarify listed under safe worker")
	}
	for _, d := range Default.Snapshot(context.Background(), "") {
		if b, ok := Default.builtin[d.Name]; ok && b.def == d && !safeWorkerTools[d.Name] {
			t.Fatalf("snapshot exposes builtin %s under safe worker", d.Name)
		}
	}
}

func TestSafeWorkerHidesOnlyInitBuiltins(t *testing.T) {
	withSafeWorker(t, true)
	r := NewRegistry()
	h := func(context.Context, *Call) string { return "" }
	r.registerBuiltin(Def{Name: "web_search", Toolset: "web", Schema: map[string]any{}, Handler: h}, "tools.web_tools")
	r.registerBuiltin(Def{Name: "terminal", Toolset: "terminal", Schema: map[string]any{}, Handler: h}, "tools.terminal")
	if r.Get("web_search") != nil || r.Get("terminal") == nil {
		t.Fatal("safe worker filter wrong on fresh registry")
	}
	if err := r.Deregister("web_search", "", ""); err != nil || r.tools["web_search"] == nil {
		t.Fatalf("hidden builtin deregistered: %v", err)
	}
	r.Register(Def{Name: "web_search", Toolset: "web", Schema: map[string]any{}, Handler: h, Plugin: "p"})
	if r.Get("web_search") == nil {
		t.Fatal("runtime replacement of a builtin hidden")
	}
}
