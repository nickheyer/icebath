package gatewayrun

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner whose session runtime is the frozen policy's own route, the chain never walked
type pickRunner struct {
	*GatewayRunner
	policy *gateway.LocalSessionPolicy
}

func (p *pickRunner) ResolveSessionAgentRuntime(ctx context.Context, _ *gateway.SessionSource, _ string, _ map[string]any, _ bool) (string, map[string]any, error) {
	return p.ResolvePolicyAgentRuntime(ctx, p.policy, false)
}

// Owner over the database whose runner resolves the given policy
func pickOwner(db *state.SessionDB, policy *gateway.LocalSessionPolicy) (*gateway.SessionAuthority, *pickRunner) {
	authority := &gateway.SessionAuthority{InstanceID: "review", Epoch: 1, ProfileID: "review", DB: db}
	runner := &pickRunner{GatewayRunner: authorityRunner(authority), policy: policy}
	authority.Runner = runner
	return authority, runner
}

type pickOutcome struct {
	warmCfg, warm, coldCfg, cold, frozen map[string]any
}

// Warm and cold config and runtime of a model pick made after the profile moved from before to after
func pickAfterEdit(t *testing.T, before, after func() map[string]any, providerName string) pickOutcome {
	t.Helper()
	ctx := context.Background()
	h := runtimeTestHome(t, "{}\n", nil)
	writeJSONConfig(t, h, before())
	private := &gateway.RecoveredSecrets{}
	policy, err := gateway.BuildPolicy(ctx, map[string]any{"cwd": h, "model": "original"}, before(), private)
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	authority, runner := pickOwner(db, nil)
	if policy, err = gateway.BindLaunchKey(ctx, authority, "sid", policy, nil, private); err != nil {
		t.Fatal(err)
	}
	runner.policy = policy
	writeJSONConfig(t, h, after())
	prepared := pyjson.ObjectOf("snapshot", pyjson.ObjectOf("receipt", pyjson.ObjectOf("session_id", "sid", "policy", policy.AsDict())))
	committed, err := gateway.PrepareModel(ctx, authority, &gateway.LiveSession{Route: "r"}, pyjson.ObjectOf("model", "switched", "provider", providerName), prepared)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := committed.Get("policy")
	next, err := gateway.RestorePolicy(raw)
	if err != nil {
		t.Fatalf("committed %v: %v", committed, err)
	}
	var out pickOutcome
	read := func(a *gateway.SessionAuthority, r *pickRunner) (map[string]any, map[string]any) {
		cfg, _, err := next.EffectiveFor(ctx, a, true)
		if err != nil {
			t.Fatal(err)
		}
		_, runtime, err := r.ResolvePolicyAgentRuntime(ctx, next, false)
		if err != nil {
			t.Fatal(err)
		}
		return cfg, runtime
	}
	out.warmCfg, out.warm = read(authority, runner)
	coldAuthority, coldRunner := pickOwner(db, policy)
	out.coldCfg, out.cold = read(coldAuthority, coldRunner)
	frozen, err := pyjson.Loads(next.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	out.frozen, _ = pyjson.Plain(frozen).(map[string]any)
	return out
}

// Custom provider entry of the config by name
func customEntry(t *testing.T, cfg map[string]any, name string) map[string]any {
	t.Helper()
	entries, _ := cfg["custom_providers"].([]any)
	for _, e := range entries {
		if entry, ok := e.(map[string]any); ok && entry["name"] == name {
			return entry
		}
	}
	t.Fatalf("no custom provider %q in %v", name, cfg["custom_providers"])
	return nil
}

// Provider names of a providers mapping in its order
func providerKeys(cfg map[string]any) []string {
	var keys []string
	for k := range cfg["providers"].(map[string]any) {
		keys = append(keys, k)
	}
	return keys
}

func TestDisplacedUnchangedLegacyEntryKeepsItsKey(t *testing.T) {
	x := func() map[string]any {
		return map[string]any{"name": "x-prov", "base_url": "http://127.0.0.1:9/x", "api_key": "x-review-key"}
	}
	y := func() map[string]any {
		return map[string]any{"name": "y-prov", "base_url": "http://127.0.0.1:9/y", "api_key": "y-review-key"}
	}
	before := func() map[string]any {
		return map[string]any{"model": map[string]any{"provider": "custom:x-prov", "default": "original"}, "custom_providers": []any{x()},
			"fallback_providers": []any{map[string]any{"provider": "custom:x-prov", "model": "original"}}}
	}
	after := func() map[string]any {
		cfg := before()
		cfg["custom_providers"] = []any{y(), x()}
		return cfg
	}
	got := pickAfterEdit(t, before, after, "custom:y-prov")
	for _, runtime := range []map[string]any{got.warm, got.cold} {
		if runtime["base_url"] != "http://127.0.0.1:9/y" || runtime["api_key"] != "y-review-key" {
			t.Fatalf("runtime %v", runtime)
		}
	}
	for _, cfg := range []map[string]any{got.warmCfg, got.coldCfg} {
		if key := customEntry(t, cfg, "x-prov")["api_key"]; key != "x-review-key" {
			t.Fatalf("x-prov key %v", key)
		}
	}
}

func TestRespelledProvidersKeyReplacesTheStaleSpelling(t *testing.T) {
	before := func() map[string]any {
		return map[string]any{"model": map[string]any{"provider": "review-prov", "default": "original"},
			"providers": map[string]any{"Review Prov": map[string]any{"base_url": "http://127.0.0.1:9/a", "api_key": "a-review-key",
				"models": map[string]any{"switched": map[string]any{}}}}}
	}
	after := func() map[string]any {
		cfg := before()
		cfg["providers"] = map[string]any{"review-prov": map[string]any{"base_url": "http://127.0.0.1:9/b", "api_key": "b-review-key",
			"models": map[string]any{"switched": map[string]any{}}}}
		return cfg
	}
	got := pickAfterEdit(t, before, after, "review-prov")
	for _, runtime := range []map[string]any{got.warm, got.cold} {
		if runtime["base_url"] != "http://127.0.0.1:9/b" || runtime["api_key"] != "b-review-key" {
			t.Fatalf("runtime %v", runtime)
		}
	}
	want := []string{"review-prov"}
	for _, cfg := range []map[string]any{got.frozen, got.warmCfg, got.coldCfg} {
		if keys := providerKeys(cfg); !reflect.DeepEqual(keys, want) {
			t.Fatalf("providers %v", keys)
		}
	}
}
