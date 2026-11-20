package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner whose session runtime resolves to no model and an empty runtime
type aliasRestartRunner struct {
	*GatewayRunner
}

func (aliasRestartRunner) ResolveSessionAgentRuntime(context.Context, *gateway.SessionSource, string, map[string]any, bool) (string, map[string]any, error) {
	return "", map[string]any{}, nil
}

func TestLiteralAliasKeySurvivesOwnerRestartAndStaysInProfile(t *testing.T) {
	ctx := context.Background()
	oldURL, farURL := "http://127.0.0.1:9/v1", "http://127.0.0.3:9/v1"
	h := runtimeTestHome(t, "{}\n", nil)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(
		"model_aliases:\n  Far: {model: far-model, provider: custom, base_url: \""+farURL+"\", api_key: sk-far-literal}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	saved := provider.DirectAliases.Entries()
	provider.DirectAliases.Set("far", provider.DirectAlias{Model: "far-model", Provider: "custom", BaseURL: farURL, APIKey: "sk-other-profile"})
	t.Cleanup(func() {
		provider.DirectAliases.Clear()
		for _, entry := range saved {
			provider.DirectAliases.Set(entry.Name, entry.Alias)
		}
	})
	realSwitch := provider.SwitchModel
	t.Cleanup(func() { provider.SwitchModel = realSwitch })
	switchTo := func(alias, baseURL string) {
		provider.SwitchModel = func(context.Context, provider.SwitchModelOptions) (provider.ModelSwitchResult, error) {
			return provider.ModelSwitchResult{Success: true, NewModel: alias + "-model", TargetProvider: "custom", BaseURL: baseURL,
				APIKey: "resolved-at-switch", ResolvedViaAlias: alias}, nil
		}
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	owner := func() (*gateway.SessionAuthority, aliasRestartRunner) {
		authority := &gateway.SessionAuthority{InstanceID: "i", Epoch: 1, ProfileID: "p", DB: db}
		runner := aliasRestartRunner{GatewayRunner: authorityRunner(authority)}
		authority.Runner = runner
		return authority, runner
	}
	apiKey := func(runner aliasRestartRunner, policy *gateway.LocalSessionPolicy) any {
		_, runtime, err := runner.ResolvePolicyAgentRuntime(ctx, policy, true)
		if err != nil {
			t.Fatal(err)
		}
		return runtime["api_key"]
	}
	first, firstRunner := owner()
	policy, err := gateway.BuildPolicy(ctx, map[string]any{"cwd": h, "model": "a", "provider": "custom", "base_url": oldURL},
		map[string]any{"model": map[string]any{"provider": "custom", "default": "a", "base_url": oldURL}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared := pyjson.ObjectOf("snapshot", pyjson.ObjectOf("receipt", pyjson.ObjectOf("session_id", "sid", "policy", policy.AsDict())))
	switchTo("far", farURL)
	result, err := gateway.PrepareModel(ctx, first, &gateway.LiveSession{Route: "r"}, pyjson.ObjectOf("model", "far"), prepared)
	if err != nil {
		t.Fatal(err)
	}
	committed, _ := result.Get("policy")
	text, err := pyjson.Dumps(committed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "sk-far-literal") || strings.Contains(text, "sk-other-profile") {
		t.Fatalf("committed policy carries a key %s", text)
	}
	switched, err := gateway.RestorePolicy(committed)
	if err != nil {
		t.Fatal(err)
	}
	if got := apiKey(firstRunner, switched); got != "sk-far-literal" {
		t.Fatalf("api key %v", got)
	}
	restarted, restartedRunner := owner()
	if got := apiKey(restartedRunner, switched); got != "sk-far-literal" {
		t.Fatalf("api key after restart %v", got)
	}
	switchTo("back", oldURL)
	again, err := gateway.PrepareModel(ctx, restarted, &gateway.LiveSession{Route: "r"}, pyjson.ObjectOf("model", "back"),
		pyjson.ObjectOf("snapshot", pyjson.ObjectOf("receipt", pyjson.ObjectOf("session_id", "sid", "policy", committed))))
	if err != nil {
		t.Fatal(err)
	}
	againPolicy, _ := again.Get("policy")
	back, err := gateway.RestorePolicy(againPolicy)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := back.EffectiveFor(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := cfg["model"].(map[string]any)
	if _, held := model["api_key_from"]; back.BaseURL() != any(oldURL) || held {
		t.Fatalf("back route %v model %v", back.BaseURL(), model)
	}
}
