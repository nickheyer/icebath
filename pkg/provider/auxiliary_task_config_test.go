package provider

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Temp home whose config only carries the auxiliary blocks set so far, built in defaults merged on load
type auxTaskHome struct {
	t   *testing.T
	h   string
	aux map[string]any
}

func newAuxTaskHome(t *testing.T) *auxTaskHome {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return &auxTaskHome{t: t, h: h, aux: map[string]any{}}
}

// Updates the auxiliary section with whole task blocks and saves it
func (a *auxTaskHome) set(blocks map[string]any) {
	a.t.Helper()
	for k, v := range blocks {
		a.aux[k] = v
	}
	if err := config.Save(a.h, config.Tree{"auxiliary": a.aux}); err != nil {
		a.t.Fatal(err)
	}
}

// Reads the effective task config through a resolver over the freshly loaded config
func (a *auxTaskHome) resolve(ctx context.Context, task any) map[string]any {
	a.t.Helper()
	tree, err := config.Load(a.h, os.LookupEnv)
	if err != nil {
		a.t.Fatal(err)
	}
	return newResolver(ctx, tree, nil, a.h, &fakeCreds{}).auxiliaryTaskConfig(task, map[string]bool{})
}

// Installs plugin task registrations shaped as the plugins registrar stores them
func withPluginAuxTasks(t *testing.T, entries ...map[string]any) {
	withPluginHooks(t, func(h *PluginManagerHooks) {
		h.AuxiliaryTasks = func() []map[string]any { return entries }
	})
}

func auxNonInheritingDefaults(extra map[string]any) map[string]any {
	d := map[string]any{"provider": "auto", "model": "", "base_url": "", "api_key": "", "timeout": 60, "extra_body": map[string]any{}}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

func TestInheritingTaskFollowsBaseUntilPinned(t *testing.T) {
	a := newAuxTaskHome(t)
	ctx := context.Background()
	withPluginAuxTasks(t, map[string]any{"key": "plug_aux", "plugin": "plug", "display_name": "Plug Aux", "description": "d",
		"defaults": map[string]any{"timeout": 90}, "inherit_from": "compression"})
	a.set(map[string]any{"compression": map[string]any{"provider": "openrouter", "model": "vendor/a",
		"base_url": "https://base.example", "api_key": "base-key"}})
	got := a.resolve(ctx, "plug_aux")
	if got["provider"] != "openrouter" || got["model"] != "vendor/a" || got["base_url"] != "https://base.example" {
		t.Fatalf("inherited route %v", got)
	}
	if toFloat(got["timeout"]) != 90 {
		t.Fatalf("plugin default must layer over the base, got %v", got["timeout"])
	}
	a.set(map[string]any{"compression": map[string]any{"provider": "openrouter", "model": "vendor/b"}})
	if got := a.resolve(ctx, "plug_aux"); got["model"] != "vendor/b" {
		t.Fatalf("base change must propagate at read time, got %v", got["model"])
	}
	a.set(map[string]any{"plug_aux": map[string]any{"provider": "auto", "model": "", "base_url": "", "api_key": "",
		"reasoning_effort": "", "timeout": 5}})
	got = a.resolve(ctx, "plug_aux")
	if got["provider"] != "openrouter" || got["model"] != "vendor/b" || toFloat(got["timeout"]) != 5 {
		t.Fatalf("no preference values keep following the base, got %v", got)
	}
	a.set(map[string]any{
		"compression": map[string]any{"provider": "custom", "model": "vendor/b", "base_url": "https://base.example", "api_key": "base-key"},
		"plug_aux":    map[string]any{"provider": "anthropic", "model": "claude-x", "base_url": "", "api_key": ""},
	})
	got = a.resolve(ctx, "plug_aux")
	if got["provider"] != "anthropic" || got["model"] != "claude-x" {
		t.Fatalf("explicit pin wins as a whole route, got %v", got)
	}
	if auxStrOrEmpty(got["base_url"]) != "" || auxStrOrEmpty(got["api_key"]) != "" {
		t.Fatalf("base endpoint and key must never leak into a pinned route, got %v", got)
	}
}

func TestBadInheritFromRegistersWithoutInheritance(t *testing.T) {
	for _, bad := range []string{"no_such_task", "plug_aux", ""} {
		t.Run(bad, func(t *testing.T) {
			a := newAuxTaskHome(t)
			withPluginAuxTasks(t, map[string]any{"key": "plug_aux", "plugin": "plug", "display_name": "Plug Aux", "description": "d",
				"defaults": auxNonInheritingDefaults(nil), "inherit_from": nil})
			if got := a.resolve(context.Background(), "plug_aux"); toFloat(got["timeout"]) != 60 {
				t.Fatalf("fixed non inheriting shape, got %v", got)
			}
		})
	}
}

func TestInheritanceCycleDoesNotRecurse(t *testing.T) {
	a := newAuxTaskHome(t)
	logs := auxCaptureLogs(t)
	withPluginAuxTasks(t,
		map[string]any{"key": "plug_a", "plugin": "plug", "defaults": map[string]any{"timeout": 90}, "inherit_from": "plug_b"},
		map[string]any{"key": "plug_b", "plugin": "plug", "defaults": map[string]any{}, "inherit_from": "plug_a"})
	if got := a.resolve(context.Background(), "plug_a"); toFloat(got["timeout"]) != 90 {
		t.Fatalf("cycle must be cut with the task defaults, got %v", got)
	}
	warned := logs.matching("Auxiliary task 'plug_a' has a circular inherit_from chain")
	if len(warned) != 1 || warned[0].level != slog.LevelWarn {
		t.Fatalf("one cycle warning expected, got %v", warned)
	}
}

func TestPluginTaskWithoutDefaultsStillInherits(t *testing.T) {
	a := newAuxTaskHome(t)
	withPluginAuxTasks(t, map[string]any{"key": "plug_aux", "plugin": "plug", "defaults": nil, "inherit_from": "compression"})
	a.set(map[string]any{"compression": map[string]any{"provider": "openrouter", "model": "vendor/a"}})
	if got := a.resolve(context.Background(), "plug_aux"); got["provider"] != "openrouter" || got["model"] != "vendor/a" {
		t.Fatalf("absent defaults are an empty mapping, inheritance still applies, got %v", got)
	}
	withPluginAuxTasks(t, map[string]any{"key": "plug_aux", "plugin": "plug", "defaults": []any{"x"}, "inherit_from": "compression"})
	a.set(map[string]any{"plug_aux": map[string]any{"timeout": 7}})
	if got := a.resolve(context.Background(), "plug_aux"); !reflect.DeepEqual(got, map[string]any{"timeout": 7}) {
		t.Fatalf("non mapping defaults leave the user block alone, got %v", got)
	}
}

func TestSessionAuxiliaryScopeReplacesTheLiveConfig(t *testing.T) {
	a := newAuxTaskHome(t)
	withPluginAuxTasks(t)
	a.set(map[string]any{"vision": map[string]any{"provider": "openrouter", "model": "live"}})
	ctx := SessionAuxiliaryScope(context.Background(), map[string]any{"vision": map[string]any{"provider": "nous", "model": "frozen"}})
	if got := a.resolve(ctx, "vision"); !reflect.DeepEqual(got, map[string]any{"provider": "nous", "model": "frozen"}) {
		t.Fatalf("session section must win over config.yaml, got %v", got)
	}
	if got := a.resolve(SessionAuxiliaryScope(context.Background(), "not a mapping"), "vision"); len(got) != 0 {
		t.Fatalf("a non mapping session section is empty, got %v", got)
	}
	if got := a.resolve(context.Background(), "vision"); got["model"] != "live" {
		t.Fatalf("outside the scope the live config is read, got %v", got)
	}
}

func TestAuthoritativeTaskStripsOnlyRouteFields(t *testing.T) {
	a := newAuxTaskHome(t)
	withPluginAuxTasks(t)
	task := NewAuthoritativeAuxiliaryTask("compression")
	if task != "compression" || NewAuthoritativeAuxiliaryTask(task) != task || NewAuthoritativeAuxiliaryTask(nil) != "" {
		t.Fatalf("authoritative task keeps its name, got %q", task)
	}
	a.set(map[string]any{"compression": map[string]any{"provider": "custom", "model": "aux-model", "base_url": "https://aux.example/v1",
		"api_key": "aux-key", "api_mode": "chat_completions", "key_env": "AUX_KEY", "api_key_env": "AUX_KEY", "reasoning_effort": "none",
		"timeout": 417, "max_concurrency": 2, "extra_body": map[string]any{"trace": "kept-policy"}}})
	got := a.resolve(context.Background(), task)
	if toFloat(got["timeout"]) != 417 || toFloat(got["max_concurrency"]) != 2 || !reflect.DeepEqual(got["extra_body"], map[string]any{"trace": "kept-policy"}) {
		t.Fatalf("policy must stay, got %v", got)
	}
	for k := range auxRouteKeys {
		if _, ok := got[k]; ok {
			t.Fatalf("route field %s must be stripped, got %v", k, got)
		}
	}
	if plain := a.resolve(context.Background(), "compression"); plain["model"] != "aux-model" {
		t.Fatalf("a plain task name keeps its route, got %v", plain)
	}
}

func TestAuxRouteKeysExport(t *testing.T) {
	if !AuxRouteKeys["api_key"] || !AuxRouteKeys["base_url"] || AuxRouteKeys["timeout"] {
		t.Fatalf("aux route keys %v", AuxRouteKeys)
	}
}
