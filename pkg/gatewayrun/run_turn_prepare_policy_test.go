package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Rewrites the profile config.yaml as the JSON of the mapping
func writeJSONConfig(t *testing.T, h string, cfg map[string]any) {
	t.Helper()
	text, err := pyjson.Dumps(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

// Policy frozen from the profile's effective user config with its launch key bound under the authority
func frozenPolicy(t *testing.T, h string, authority *gateway.SessionAuthority, sid string, params map[string]any, launch *string) *gateway.LocalSessionPolicy {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.LoadUserEffective(filepath.Join(h, "config.yaml"), config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	private := &gateway.RecoveredSecrets{}
	policy, err := gateway.BuildPolicy(ctx, params, cfg, private)
	if err != nil {
		t.Fatal(err)
	}
	policy, err = gateway.BindLaunchKey(ctx, authority, sid, policy, launch, private)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// Runner holding the launch authority, as a namespace runner with session_authority set
func authorityRunner(authority *gateway.SessionAuthority) *GatewayRunner {
	r := allocGatewayRunner()
	r.SetSessionAuthority(authority)
	return r
}

// Runtime kwargs of a turn on the frozen policy
func policyRuntime(t *testing.T, r *GatewayRunner, policy *gateway.LocalSessionPolicy) map[string]any {
	t.Helper()
	_, runtime, err := resolvePolicyAgentRuntime(context.Background(), r, policy, true)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestFrozenRouteKeepsItsEndpointAfterLiveConfigEdit(t *testing.T) {
	h := runtimeTestHome(t, "{}\n", nil)
	write := func(url, key string) {
		writeJSONConfig(t, h, map[string]any{"model": map[string]any{"provider": "custom", "base_url": url, "api_key": key, "default": "m"}})
	}
	write("http://127.0.0.1:1/v1", "sk-frozen-endpoint-one")
	authority := &gateway.SessionAuthority{InstanceID: "i", Epoch: 1, ProfileID: "p"}
	policy := frozenPolicy(t, h, authority, "sid", map[string]any{"cwd": t.TempDir(), "model": "m"}, nil)
	write("http://127.0.0.1:2/v1", "sk-new-endpoint-two")
	runtime := policyRuntime(t, authorityRunner(authority), policy)
	if runtime["base_url"] != "http://127.0.0.1:1/v1" || runtime["api_key"] != "sk-frozen-endpoint-one" {
		t.Fatalf("runtime %v", runtime)
	}
}

func TestFrozenBareCustomRouteKeepsItsEndpointPoolAndLaunchKeyWins(t *testing.T) {
	url := "http://127.0.0.1:1/v1"
	h := runtimeTestHome(t, "{}\n", nil)
	writeJSONConfig(t, h, map[string]any{"model": map[string]any{"provider": "custom", "base_url": url, "api_key": "sk-frozen-config", "default": "m"},
		"custom_providers": []any{map[string]any{"name": "Pooled", "base_url": url}}})
	auth := `{"version": 1, "providers": {}, "credential_pool": {"custom:pooled": [{"id": "k1", "label": "a", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-pooled"}]}}`
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	authority := &gateway.SessionAuthority{InstanceID: "i", Epoch: 1, ProfileID: "p"}
	r := authorityRunner(authority)
	var runtimes []map[string]any
	launchKey := "sk-launch-explicit"
	for _, tc := range []struct {
		sid    string
		launch *string
	}{{"config-keyed", nil}, {"launch-keyed", &launchKey}} {
		params := map[string]any{"cwd": t.TempDir(), "model": "m"}
		if tc.launch != nil {
			params["api_key"] = *tc.launch
		}
		runtimes = append(runtimes, policyRuntime(t, r, frozenPolicy(t, h, authority, tc.sid, params, tc.launch)))
	}
	if runtimes[0]["base_url"] != url || runtimes[0]["credential_pool"] == nil || runtimes[0]["api_key"] != "sk-pooled" {
		t.Fatalf("config keyed runtime %v", runtimes[0])
	}
	if runtimes[1]["api_key"] != "sk-launch-explicit" || runtimes[1]["credential_pool"] != nil {
		t.Fatalf("launch keyed runtime %v", runtimes[1])
	}
}

// Freezes the Fixture entry at endpoint A with a pool only key, one route per launch base_url, then moves the live entry to B
func frozenFixtureRoutes(t *testing.T, launches ...string) []map[string]any {
	t.Helper()
	h := runtimeTestHome(t, "{}\n", nil)
	write := func(url string) {
		writeJSONConfig(t, h, map[string]any{"model": map[string]any{"provider": "fixture", "default": "m"},
			"custom_providers": []any{map[string]any{"name": "Fixture", "base_url": url}}})
	}
	write("https://a.fixture.example/v1")
	auth := `{"version": 1, "providers": {}, "credential_pool": {"custom:fixture": [{"id": "k1", "label": "a", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-pool-key-for-endpoint-a"}]}}`
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	authority := &gateway.SessionAuthority{InstanceID: "i", Epoch: 1, ProfileID: "p"}
	var policies []*gateway.LocalSessionPolicy
	for n, url := range launches {
		params := map[string]any{"cwd": t.TempDir(), "model": "m", "provider": "fixture"}
		if url != "" {
			params["base_url"] = url
		}
		policies = append(policies, frozenPolicy(t, h, authority, "s"+string(rune('0'+n)), params, nil))
	}
	write("https://b.fixture.example/v1")
	r := authorityRunner(authority)
	var runtimes []map[string]any
	for _, policy := range policies {
		runtimes = append(runtimes, policyRuntime(t, r, policy))
	}
	return runtimes
}

func TestFrozenNamedRouteKeepsItsPoolAfterLiveEndpointEdit(t *testing.T) {
	runtime := frozenFixtureRoutes(t, "")[0]
	if runtime["base_url"] != "https://a.fixture.example/v1" || runtime["api_key"] != "sk-pool-key-for-endpoint-a" {
		t.Fatalf("runtime %v", runtime)
	}
	if runtime["credential_pool"] == nil {
		t.Fatal("the frozen route lost its pool")
	}
}

func TestFrozenLaunchUrlOverrideNeverBorrowsTheNamedPool(t *testing.T) {
	runtime := frozenFixtureRoutes(t, "https://b.fixture.example/v1")[0]
	if runtime["base_url"] != "https://b.fixture.example/v1" {
		t.Fatalf("runtime %v", runtime)
	}
	if runtime["api_key"] != "no-key-required" || runtime["credential_pool"] != nil {
		t.Fatalf("runtime %v", runtime)
	}
}
