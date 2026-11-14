package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Field of a refusal, empty when it names none or the error is no refusal
func refusalField(err error) string {
	refusal, ok := err.(*state.RuntimeStoreError)
	if !ok || refusal.Field == nil {
		return ""
	}
	return *refusal.Field
}

// API key the policy's effective model section carries for an owner
func policyModelKey(t *testing.T, ctx context.Context, policy *LocalSessionPolicy, owner launchSecretOwner) any {
	t.Helper()
	config, err := policy.config(ctx, owner, true)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := config["model"].(map[string]any)
	return model["api_key"]
}

func TestExplicitKeyIsPrivateAndMissingAfterRestartFailsClosed(t *testing.T) {
	ctx := policyBuildHome(t)
	dir := t.TempDir()
	authority := &SessionAuthority{InstanceID: "owned", ProfileID: "profile", Epoch: 1}
	sibling := &SessionAuthority{InstanceID: "sibling", ProfileID: "profile", Epoch: 1}
	raw := "UNIQUE-PRIVATE-LAUNCH-KEY"
	before := os.Environ()
	params := func() map[string]any { return map[string]any{"cwd": dir, "model": "fixture", "api_key": raw} }
	build := func(cfg map[string]any) (*LocalSessionPolicy, *RecoveredSecrets) {
		private := &RecoveredSecrets{}
		policy, err := buildPolicy(ctx, params(), cfg, buildPolicyOptions{PrivateSecrets: private})
		if err != nil {
			t.Fatal(err)
		}
		return policy, private
	}
	built, private := build(map[string]any{"model": map[string]any{"api_key": raw}})
	policy, err := bindLaunchKey(ctx, authority, "session-a", built, &raw, private)
	if err != nil {
		t.Fatal(err)
	}
	if key := policyModelKey(t, ctx, policy, authority); key != raw {
		t.Fatalf("effective model key %v", key)
	}
	encoded, err := pyjson.Dumps(policy.asDict())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, raw) {
		t.Fatal("the durable policy carries the launch key")
	}
	held, err := launchKey(authority, policy)
	if err != nil || held == nil || *held != raw {
		t.Fatalf("launch key %v %v", held, err)
	}
	retry, retryPrivate := build(map[string]any{"model": map[string]any{"api_key": raw}})
	again, err := bindLaunchKey(ctx, authority, "session-a", retry, &raw, retryPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.asDict(), policy.asDict()) {
		t.Fatalf("retry bound %v, want %v", again.asDict(), policy.asDict())
	}
	for _, other := range []*SessionAuthority{sibling, {InstanceID: "owned", ProfileID: "profile", Epoch: 2}} {
		if _, err := launchKey(other, policy); refusalReason(err) != "launch_credentials_unavailable" {
			t.Fatalf("launch key of another owner: %v", err)
		}
		if _, err := policy.config(ctx, other, true); refusalReason(err) != "launch_credentials_unavailable" {
			t.Fatalf("config of another owner: %v", err)
		}
	}
	plain, err := buildPolicy(ctx, params(), map[string]any{}, buildPolicyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	different := "different"
	if _, err := bindLaunchKey(ctx, authority, "session-a", plain, &different, nil); refusalReason(err) != "admission_conflict" {
		t.Fatalf("a different key on a held slot: %v", err)
	}
	if !slices.Equal(os.Environ(), before) {
		t.Fatal("binding a launch key changed the process environment")
	}
}

func TestConfigCredentialsRecoverOnlyExactSource(t *testing.T) {
	base := t.TempDir()
	profileHome := filepath.Join(base, "profile")
	if err := os.MkdirAll(profileHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profileHome)
	ctx := context.Background()
	write := func(key string) {
		text := `{"model": {"provider": "custom", "api_key": "` + key + `", "base_url": "http://127.0.0.1:1234/v1"}}`
		if err := os.WriteFile(filepath.Join(profileHome, "config.yaml"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("owned-config-token")
	cfg := map[string]any{"model": map[string]any{"provider": "custom", "api_key": "owned-config-token", "base_url": "http://127.0.0.1:1234/v1"}}
	owner := func(path, profile string) *secretOwnerDouble {
		return &secretOwnerDouble{dbPath: filepath.Join(path, "state.db"), profile: profile, slots: &launchSecretSlots{}}
	}
	quietHandleHealth(t)
	db, err := state.NewSessionDB(ctx, filepath.Join(profileHome, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	private := &RecoveredSecrets{}
	built, err := buildPolicy(ctx, map[string]any{"cwd": profileHome, "model": "frozen"}, cfg, buildPolicyOptions{PrivateSecrets: private})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := bindLaunchKey(ctx, &SessionAuthority{InstanceID: "old", ProfileID: "owned", Epoch: 2, DB: db}, "session", built, nil, private)
	if err != nil {
		t.Fatal(err)
	}
	if key := policyModelKey(t, ctx, policy, owner(profileHome, "owned")); key != "owned-config-token" {
		t.Fatalf("recovered key %v", key)
	}
	encoded, err := pyjson.Dumps(policy.asDict())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "owned-config-token") {
		t.Fatal("the durable policy carries the config secret")
	}
	for mismatched, wrong := range map[string]*secretOwnerDouble{"profile": owner(profileHome, "wrong"), "home": owner(base, "owned")} {
		_, err := policy.config(ctx, wrong, true)
		if refusalReason(err) != "launch_credentials_unavailable" || refusalField(err) != mismatched {
			t.Fatalf("%s mismatch: %v field %q", mismatched, err, refusalField(err))
		}
	}
	stale := *policy
	other := "other-policy"
	stale.Model = &other
	if key := policyModelKey(t, ctx, &stale, owner(profileHome, "owned")); key != "owned-config-token" {
		t.Fatalf("stale stamp recovered %v", key)
	}
	write("changed-unrelated-token")
	if _, err := stale.config(ctx, owner(profileHome, "owned"), true); refusalReason(err) != "launch_credentials_unavailable" {
		t.Fatalf("stale stamp after a rotated key: %v", err)
	}
	if _, err := policy.config(ctx, owner(profileHome, "owned"), true); refusalReason(err) != "launch_credentials_unavailable" {
		t.Fatalf("policy after a rotated key: %v", err)
	}
}

// Policy bound over a profile whose config holds a route secret and an MCP server secret
func launchBoundPolicy(t *testing.T) (context.Context, string, string, func(path, profile string) *secretOwnerDouble, *LocalSessionPolicy) {
	t.Helper()
	base := t.TempDir()
	profileHome := filepath.Join(base, "profile")
	if err := os.MkdirAll(profileHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profileHome)
	ctx := context.Background()
	text := `{"model": {"provider": "custom", "api_key": "route-token", "base_url": "http://127.0.0.1:1234/v1"}, ` +
		`"mcp_servers": {"docs": {"command": "docs-mcp", "env": {"DOCS_TOKEN": "mcp-token"}}}}`
	if err := os.WriteFile(filepath.Join(profileHome, "config.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"model":       map[string]any{"provider": "custom", "api_key": "route-token", "base_url": "http://127.0.0.1:1234/v1"},
		"mcp_servers": map[string]any{"docs": map[string]any{"command": "docs-mcp", "env": map[string]any{"DOCS_TOKEN": "mcp-token"}}},
	}
	owner := func(path, profile string) *secretOwnerDouble {
		return &secretOwnerDouble{dbPath: filepath.Join(path, "state.db"), profile: profile, slots: &launchSecretSlots{}}
	}
	quietHandleHealth(t)
	db, err := state.NewSessionDB(ctx, filepath.Join(profileHome, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	private := &RecoveredSecrets{}
	built, err := buildPolicy(ctx, map[string]any{"cwd": profileHome, "model": "frozen"}, cfg, buildPolicyOptions{PrivateSecrets: private})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := bindLaunchKey(ctx, &SessionAuthority{InstanceID: "new", ProfileID: "owned", Epoch: 2, DB: db}, "session", built, nil, private)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, base, profileHome, owner, policy
}

// Frozen config of a policy as a mutable decoded object
func frozenConfigObject(t *testing.T, policy *LocalSessionPolicy) *pyjson.Object {
	t.Helper()
	decoded, err := pyjson.Loads(policy.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	return decoded.(*pyjson.Object)
}

// Policy copy whose frozen config is replaced by the given object
func withFrozenConfig(t *testing.T, policy *LocalSessionPolicy, config *pyjson.Object) *LocalSessionPolicy {
	t.Helper()
	text, err := pyjson.Dumps(config)
	if err != nil {
		t.Fatal(err)
	}
	moved := *policy
	moved.ConfigJSON = text
	return &moved
}

func TestANonSecretPolicyChangeKeepsTheReference(t *testing.T) {
	ctx, _, profileHome, owner, policy := launchBoundPolicy(t)
	config := frozenConfigObject(t, policy)
	agent, _ := config.Get("agent")
	section, ok := agent.(*pyjson.Object)
	if !ok {
		section = pyjson.NewObject()
		config.Set("agent", section)
	}
	section.Set("reasoning_effort", "low")
	servers, _ := config.Get("mcp_servers")
	servers.(*pyjson.Object).Set("extra", pyjson.ObjectOf("command", "extra-mcp"))
	moved := withFrozenConfig(t, policy, config)
	other, skills := "other", "later field"
	moved.Model, moved.Toolsets, moved.SkillsPrompt = &other, []string{"web"}, &skills
	recovered, err := moved.config(ctx, owner(profileHome, "owned"), true)
	if err != nil {
		t.Fatal(err)
	}
	if key := recovered["model"].(map[string]any)["api_key"]; key != "route-token" {
		t.Fatalf("route key %v", key)
	}
	env := recovered["mcp_servers"].(map[string]any)["docs"].(map[string]any)["env"]
	if !reflect.DeepEqual(env, map[string]any{"DOCS_TOKEN": "mcp-token"}) {
		t.Fatalf("server env %v", env)
	}
}

func TestARevokedRouteSecretIsStillRefused(t *testing.T) {
	ctx, _, profileHome, owner, policy := launchBoundPolicy(t)
	text := `{"model": {"provider": "custom", "base_url": "http://127.0.0.1:1234/v1"}, ` +
		`"mcp_servers": {"docs": {"command": "docs-mcp", "env": {"DOCS_TOKEN": "mcp-token"}}}}`
	if err := os.WriteFile(filepath.Join(profileHome, "config.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := policy.config(ctx, owner(profileHome, "owned"), true)
	if refusalReason(err) != "launch_credentials_unavailable" || refusalField(err) != "model.api_key" {
		t.Fatalf("revoked route secret: %v field %q", err, refusalField(err))
	}
}

func TestASecretPairedWithAMovedEndpointIsRefusedByName(t *testing.T) {
	for _, moved := range []string{"model", "mcp"} {
		t.Run(moved, func(t *testing.T) {
			ctx, _, profileHome, owner, policy := launchBoundPolicy(t)
			config := frozenConfigObject(t, policy)
			field := "model.api_key"
			if moved == "model" {
				model, _ := config.Get("model")
				model.(*pyjson.Object).Set("base_url", "http://attacker.invalid/v1")
			} else {
				servers, _ := config.Get("mcp_servers")
				docs, _ := servers.(*pyjson.Object).Get("docs")
				docs.(*pyjson.Object).Set("command", "other-mcp")
				field = "mcp_servers.docs.env"
			}
			_, err := withFrozenConfig(t, policy, config).config(ctx, owner(profileHome, "owned"), true)
			refusal, _ := err.(*state.RuntimeStoreError)
			if refusalReason(err) != "launch_credentials_unavailable" || refusalField(err) != field ||
				refusal.Detail == nil || !strings.Contains(*refusal.Detail, "endpoint") {
				t.Fatalf("moved endpoint: %v field %q", err, refusalField(err))
			}
		})
	}
}

// Binds a policy built over the given profile config, returning a fresh owner per read and the bound policy
func bindConfigPolicy(t *testing.T, configJSON string) (context.Context, func() *secretOwnerDouble, *LocalSessionPolicy) {
	t.Helper()
	ctx, _, owner, policy, _ := bindConfigPolicyAt(t, configJSON)
	return ctx, owner, policy
}

// Binds a session frozen on a profile config as the gateway loads it from disk
func bindLoadedSession(t *testing.T, configJSON string) (context.Context, string, func() *secretOwnerDouble, *LocalSessionPolicy) {
	t.Helper()
	ctx, profileHome, owner, policy, _ := bindProfilePolicy(t, configJSON, true)
	return ctx, profileHome, owner, policy
}

// Binds a policy over a profile config, also returning the profile home and the secret paths it froze
func bindConfigPolicyAt(t *testing.T, configJSON string) (context.Context, string, func() *secretOwnerDouble, *LocalSessionPolicy, []string) {
	t.Helper()
	return bindProfilePolicy(t, configJSON, false)
}

// Binds a policy over a profile config taken as written or as the gateway loads it
func bindProfilePolicy(t *testing.T, configJSON string, loaded bool) (context.Context, string, func() *secretOwnerDouble, *LocalSessionPolicy, []string) {
	t.Helper()
	profileHome := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(profileHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profileHome)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(profileHome, "config.yaml"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	decoded, err := pyjson.Loads(configJSON)
	if err != nil {
		t.Fatal(err)
	}
	cfg := pyjson.Plain(decoded).(map[string]any)
	if loaded {
		cfg = loadGatewayConfig(ctx, filepath.Join(profileHome, "config.yaml"))
	}
	owner := func() *secretOwnerDouble {
		return &secretOwnerDouble{dbPath: filepath.Join(profileHome, "state.db"), profile: "owned", slots: &launchSecretSlots{}}
	}
	quietHandleHealth(t)
	db, err := state.NewSessionDB(ctx, filepath.Join(profileHome, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	private := &RecoveredSecrets{}
	built, err := buildPolicy(ctx, map[string]any{"cwd": profileHome, "model": "frozen"}, cfg, buildPolicyOptions{PrivateSecrets: private})
	if err != nil {
		t.Fatal(err)
	}
	var frozen []string
	for _, path := range private.paths {
		frozen = append(frozen, pathKey(path))
	}
	slices.Sort(frozen)
	policy, err := bindLaunchKey(ctx, &SessionAuthority{InstanceID: "new", ProfileID: "owned", Epoch: 2, DB: db}, "session", built, nil, private)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, profileHome, owner, policy, frozen
}

// Sets the value at a path of object keys and list indexes inside a decoded config
func setConfigPath(t *testing.T, config *pyjson.Object, value any, path ...any) {
	t.Helper()
	var node any = config
	for _, part := range path[:len(path)-1] {
		switch key := part.(type) {
		case string:
			node, _ = node.(*pyjson.Object).Get(key)
		case int:
			node = node.([]any)[key]
		}
	}
	node.(*pyjson.Object).Set(path[len(path)-1].(string), value)
}

// Policy whose frozen config had one value repointed without a rebind
func repointedPolicy(t *testing.T, policy *LocalSessionPolicy, value any, path ...any) *LocalSessionPolicy {
	t.Helper()
	config := frozenConfigObject(t, policy)
	setConfigPath(t, config, value, path...)
	return withFrozenConfig(t, policy, config)
}

type routedSecretCase struct {
	name   string
	config string
	path   []any
	field  string
}

const (
	evilEndpoint = "http://evil.invalid/v1"
	baseRoute    = `"model": {"provider": "custom", "default": "m", "base_url": "http://good.invalid/v1"}`
)

// Runs each case, the unmoved policy recovering and the repointed one refused by the secret's field
func refuseRepointed(t *testing.T, cases []routedSecretCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, owner, policy := bindConfigPolicy(t, c.config)
			if recovered, err := policy.config(ctx, owner(), true); err != nil || len(recovered) == 0 {
				t.Fatalf("the unmoved policy did not recover: %v %v", recovered, err)
			}
			_, err := repointedPolicy(t, policy, evilEndpoint, c.path...).config(ctx, owner(), true)
			refusal, _ := err.(*state.RuntimeStoreError)
			if refusalReason(err) != "launch_credentials_unavailable" || refusalField(err) != c.field ||
				refusal.Detail == nil || !strings.Contains(*refusal.Detail, "endpoint") {
				t.Fatalf("repointed route: %v field %q", err, refusalField(err))
			}
		})
	}
}

func TestASecretIsBoundToTheEndpointItsRouteResolvesTo(t *testing.T) {
	refuseRepointed(t, []routedSecretCase{
		{"entry_api_url", `{"model": {"provider": "p2", "default": "m"}, "providers": {"p2": {"api": "http://good.invalid/v1", "api_key": "K-p2"}}}`,
			[]any{"providers", "p2", "api"}, "providers.p2.api_key"},
		{"fallback_key_via_entry", `{"model": {"provider": "custom", "default": "m", "base_url": "http://good.invalid/v1"}, ` +
			`"providers": {"fb": {"base_url": "http://good-fb.invalid/v1"}}, "fallback_providers": [{"provider": "fb", "model": "x", "api_key": "K-fb"}]}`,
			[]any{"providers", "fb", "base_url"}, "fallback_providers.0.api_key"},
		{"model_key_keyless_entry", `{"model": {"provider": "myprov", "default": "m", "api_key": "K-model"}, "providers": {"myprov": {"base_url": "http://good.invalid/v1"}}}`,
			[]any{"providers", "myprov", "base_url"}, "model.api_key"},
	})
}

func TestEveryRouteShapeBindsItsSecretToTheOwningRoute(t *testing.T) {
	refuseRepointed(t, []routedSecretCase{
		{"alias_key_via_api_key_from", `{"model": {"provider": "custom", "default": "m", "base_url": "http://good.invalid/v1", "api_key_from": ["model_aliases", "alt", "api_key"]}, ` +
			`"model_aliases": {"alt": {"provider": "custom", "model": "x", "api_key": "K-alias"}}}`,
			[]any{"model", "base_url"}, "model_aliases.alt.api_key"},
		{"aux_fallback_chain_via_entry", `{` + baseRoute + `, "providers": {"team": {"base_url": "http://good.invalid/v1"}}, ` +
			`"auxiliary": {"compression": {"provider": "custom", "model": "m", "base_url": "http://good.invalid/v1", "fallback_chain": [{"provider": "team", "model": "m", "api_key": "K-chain"}]}}}`,
			[]any{"providers", "team", "base_url"}, "auxiliary.compression.fallback_chain.0.api_key"},
		{"aux_task_on_main", `{` + baseRoute + `, "auxiliary": {"compression": {"provider": "main", "api_key": "K-aux"}}}`,
			[]any{"model", "base_url"}, "auxiliary.compression.api_key"},
		{"aux_task_own_url", `{` + baseRoute + `, "auxiliary": {"vision": {"provider": "custom", "model": "m", "base_url": "http://good.invalid/v1", "api_key": "K-aux"}}}`,
			[]any{"auxiliary", "vision", "base_url"}, "auxiliary.vision.api_key"},
		{"aux_task_via_entry", `{` + baseRoute + `, "providers": {"team": {"base_url": "http://good.invalid/v1"}}, ` +
			`"auxiliary": {"compression": {"provider": "team", "model": "m", "api_key": "K-aux"}}}`,
			[]any{"providers", "team", "base_url"}, "auxiliary.compression.api_key"},
		{"fallback_model_dict_via_entry", `{` + baseRoute + `, "providers": {"fb": {"base_url": "http://good.invalid/v1"}}, ` +
			`"fallback_model": {"provider": "fb", "model": "x", "api_key": "K-fb"}}`,
			[]any{"providers", "fb", "base_url"}, "fallback_model.api_key"},
		{"fallback_providers_dict", `{` + baseRoute + `, "fallback_providers": {"provider": "custom", "model": "x", "base_url": "http://good.invalid/v1", "api_key": "K-fb"}}`,
			[]any{"fallback_providers", "base_url"}, "fallback_providers.api_key"},
		{"fallback_providers_dict_via_entry", `{` + baseRoute + `, "providers": {"fb": {"base_url": "http://good.invalid/v1"}}, ` +
			`"fallback_providers": {"provider": "fb", "model": "x", "api_key": "K-fb"}}`,
			[]any{"providers", "fb", "base_url"}, "fallback_providers.api_key"},
		{"model_alias_own_url", `{"model": {"provider": "custom", "default": "m", "base_url": "http://good.invalid/v1", ` +
			`"aliases": {"alt": {"provider": "custom", "model": "x", "base_url": "http://good.invalid/v1", "api_key": "K-alias"}}}}`,
			[]any{"model", "aliases", "alt", "base_url"}, "model.aliases.alt.api_key"},
		{"nested_alias_key_via_api_key_from", `{"model": {"provider": "custom", "default": "m", "base_url": "http://good.invalid/v1", "api_key_from": ["model", "aliases", "alt", "api_key"], ` +
			`"aliases": {"alt": {"provider": "custom", "model": "x", "api_key": "K-alias"}}}}`,
			[]any{"model", "base_url"}, "model.aliases.alt.api_key"},
	})
}

func TestAnUnrelatedRouteEditKeepsAnAuxiliaryAndFallbackKey(t *testing.T) {
	ctx, owner, policy := bindConfigPolicy(t, `{`+baseRoute+`, "providers": {"team": {"base_url": "http://good.invalid/v1"}, "other": {"base_url": "http://good.invalid/v1"}}, `+
		`"fallback_providers": {"provider": "team", "model": "x", "api_key": "K-fb"}, `+
		`"auxiliary": {"compression": {"provider": "team", "model": "m", "api_key": "K-aux"}, "vision": {"provider": "other", "model": "m"}}}`)
	config := frozenConfigObject(t, policy)
	setConfigPath(t, config, evilEndpoint, "providers", "other", "base_url")
	setConfigPath(t, config, 30, "auxiliary", "compression", "timeout")
	setConfigPath(t, config, "y", "fallback_providers", "model")
	recovered, err := withFrozenConfig(t, policy, config).config(ctx, owner(), true)
	if err != nil {
		t.Fatal(err)
	}
	if key := recovered["auxiliary"].(map[string]any)["compression"].(map[string]any)["api_key"]; key != "K-aux" {
		t.Fatalf("auxiliary key %v", key)
	}
	if key := recovered["fallback_providers"].(map[string]any)["api_key"]; key != "K-fb" {
		t.Fatalf("fallback key %v", key)
	}
}

func TestACosmeticUrlRespellingKeepsTheKey(t *testing.T) {
	for _, spelling := range []string{"http://GOOD.invalid/v1", "http://good.invalid/v1/", "HTTP://good.invalid:80/v1"} {
		t.Run(spelling, func(t *testing.T) {
			ctx, owner, policy := bindConfigPolicy(t, `{"model": {"provider": "custom", "default": "m", "base_url": "http://good.invalid/v1", "api_key": "K-model"}, `+
				`"providers": {"team": {"base_url": "http://good.invalid/v1", "api_key": "K-team"}}, `+
				`"auxiliary": {"compression": {"provider": "team", "model": "m", "api_key": "K-aux"}}}`)
			config := frozenConfigObject(t, policy)
			setConfigPath(t, config, spelling, "model", "base_url")
			setConfigPath(t, config, spelling, "providers", "team", "base_url")
			recovered, err := withFrozenConfig(t, policy, config).config(ctx, owner(), true)
			if err != nil {
				t.Fatal(err)
			}
			if key := recovered["model"].(map[string]any)["api_key"]; key != "K-model" {
				t.Fatalf("model key %v", key)
			}
			if key := recovered["providers"].(map[string]any)["team"].(map[string]any)["api_key"]; key != "K-team" {
				t.Fatalf("entry key %v", key)
			}
			if key := recovered["auxiliary"].(map[string]any)["compression"].(map[string]any)["api_key"]; key != "K-aux" {
				t.Fatalf("auxiliary key %v", key)
			}
		})
	}
}

// Rewrites the profile config file of a home
func writeProfileConfig(t *testing.T, profileHome, configJSON string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(profileHome, "config.yaml"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDroppedTopLevelSettingDoesNotWedgeTheSession(t *testing.T) {
	route := `"model": {"provider": "custom", "api_key": "route-token", "base_url": "http://127.0.0.1:1234/v1"}`
	ctx, profileHome, owner, policy, frozen := bindConfigPolicyAt(t, `{"API_SERVER_KEY": "owned-api-server-token", `+route+`}`)
	if !slices.Equal(frozen, []string{`["API_SERVER_KEY"]`, `["model", "api_key"]`}) {
		t.Fatalf("frozen secret paths %v", frozen)
	}
	encoded, err := pyjson.Dumps(policy.asDict())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "owned-api-server-token") {
		t.Fatal("the durable policy carries the top level secret")
	}
	config, err := policy.config(ctx, owner(), true)
	if err != nil || config["API_SERVER_KEY"] != "owned-api-server-token" {
		t.Fatalf("top level secret %v %v", config["API_SERVER_KEY"], err)
	}
	writeProfileConfig(t, profileHome, `{`+route+`}`)
	config, err = policy.config(ctx, owner(), true)
	if err != nil {
		t.Fatal(err)
	}
	if key := config["model"].(map[string]any)["api_key"]; key != "route-token" || config["API_SERVER_KEY"] != nil {
		t.Fatalf("after the setting moved: route key %v, top level %v", key, config["API_SERVER_KEY"])
	}
	writeProfileConfig(t, profileHome, `{"model": {"provider": "custom", "base_url": "http://127.0.0.1:1234/v1"}}`)
	if _, err := policy.config(ctx, owner(), true); refusalReason(err) != "launch_credentials_unavailable" {
		t.Fatalf("a gone route credential: %v", err)
	}
	settings, err := policy.config(ctx, owner(), false)
	if err != nil {
		t.Fatal(err)
	}
	if model := settings["model"].(map[string]any); model["provider"] != "custom" || model["api_key"] != nil {
		t.Fatalf("settings only read %v", model)
	}
}

func TestAnUnrelatedSecretEditKeepsTheRouteAndAGoneRouteKeyIsNamed(t *testing.T) {
	config := func(routeKey, env string) string {
		model := `"model": {"provider": "custom", ` + routeKey + `"base_url": "http://127.0.0.1:1234/v1"}`
		return `{` + model + `, "mcp_servers": {"docs": {"command": "docs-mcp"` + env + `}}}`
	}
	for _, edit := range []string{"change", "remove"} {
		t.Run(edit, func(t *testing.T) {
			ctx, profileHome, owner, policy, _ := bindConfigPolicyAt(t, config(`"api_key": "route-token", `, `, "env": {"DOCS_TOKEN": "old-mcp-token"}`))
			env := ""
			if edit == "change" {
				env = `, "env": {"DOCS_TOKEN": "rotated-mcp-token"}`
			}
			writeProfileConfig(t, profileHome, config(`"api_key": "route-token", `, env))
			recovered, err := policy.config(ctx, owner(), true)
			if err != nil {
				t.Fatal(err)
			}
			if key := recovered["model"].(map[string]any)["api_key"]; key != "route-token" {
				t.Fatalf("route key %v", key)
			}
			if held := recovered["mcp_servers"].(map[string]any)["docs"].(map[string]any)["env"]; held != nil {
				t.Fatalf("server env %v", held)
			}
			routeKey := ""
			if edit == "change" {
				routeKey = `"api_key": "another-route-token", `
			}
			writeProfileConfig(t, profileHome, config(routeKey, env))
			_, err = policy.config(ctx, owner(), true)
			refusal, _ := err.(*state.RuntimeStoreError)
			if refusalReason(err) != "launch_credentials_unavailable" || refusalField(err) != "model.api_key" {
				t.Fatalf("gone route key: %v field %q", err, refusalField(err))
			}
			if refusal.Detail != nil && (strings.Contains(*refusal.Detail, "route-token") || strings.Contains(*refusal.Detail, "another")) {
				t.Fatalf("the refusal detail leaks a key: %s", *refusal.Detail)
			}
		})
	}
}

// Secret a recovered config holds at a path of keys and list indexes
func configAt(t *testing.T, config map[string]any, path ...any) any {
	t.Helper()
	var node any = config
	for _, part := range path {
		switch key := part.(type) {
		case string:
			node = node.(map[string]any)[key]
		case int:
			node = node.([]any)[key]
		}
	}
	return node
}

// Recovered config of a policy for a freshly restarted owner
func recoveredConfig(t *testing.T, ctx context.Context, policy *LocalSessionPolicy, owner func() *secretOwnerDouble) map[string]any {
	t.Helper()
	config, err := policy.config(ctx, owner(), true)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestAnUnchangedCustomProvidersKeySurvivesARestart(t *testing.T) {
	config := func(key string) string {
		return `{"model": {"provider": "custom:local", "default": "m"}, ` +
			`"custom_providers": [{"name": "Local", "base_url": "http://127.0.0.1:1234/v1", "api_key": "` + key + `"}]}`
	}
	ctx, profileHome, owner, policy := bindLoadedSession(t, config("cp-token"))
	if key := configAt(t, recoveredConfig(t, ctx, policy, owner), "custom_providers", 0, "api_key"); key != "cp-token" {
		t.Fatalf("recovered key %v", key)
	}
	writeProfileConfig(t, profileHome, config("rotated-token"))
	_, err := policy.config(ctx, owner(), true)
	refusal, _ := err.(*state.RuntimeStoreError)
	if refusal == nil || refusalField(err) != "custom_providers.0.api_key" || refusal.Detail == nil || !strings.Contains(*refusal.Detail, "changed in") {
		t.Fatalf("rotated key: %v field %q", err, refusalField(err))
	}
}

func TestOnlyTheEntryTheRuntimeSelectsIsTheRoute(t *testing.T) {
	config := func(first, second string) string {
		return `{"model": {"provider": "custom:shared", "default": "m"}, "custom_providers": [` +
			`{"name": "Shared", "base_url": "http://127.0.0.1:1111/v1", "api_key": "` + first + `"}, ` +
			`{"name": "Shared", "base_url": "http://127.0.0.1:2222/v1", "api_key": "` + second + `"}]}`
	}
	ctx, profileHome, owner, policy := bindLoadedSession(t, config("first-token", "second-token"))
	writeProfileConfig(t, profileHome, config("first-token", "rotated-second"))
	recovered := recoveredConfig(t, ctx, policy, owner)
	if key := configAt(t, recovered, "custom_providers", 0, "api_key"); key != "first-token" {
		t.Fatalf("selected entry key %v", key)
	}
	if key := configAt(t, recovered, "custom_providers", 1, "api_key"); key != nil {
		t.Fatalf("unused entry key %v", key)
	}
	writeProfileConfig(t, profileHome, config("rotated-first", "rotated-second"))
	if _, err := policy.config(ctx, owner(), true); refusalReason(err) == "" || refusalField(err) != "custom_providers.0.api_key" {
		t.Fatalf("rotated selected key: %v field %q", err, refusalField(err))
	}
}

func TestACustomPrefixedProvidersKeyIsTheRoute(t *testing.T) {
	config := func(key string) string {
		return `{"model": {"provider": "custom:foo", "default": "m"}, ` +
			`"providers": {"custom:foo": {"base_url": "http://127.0.0.1:1234/v1", "api_key": "` + key + `"}}}`
	}
	ctx, profileHome, owner, policy := bindLoadedSession(t, config("foo-token"))
	if key := configAt(t, recoveredConfig(t, ctx, policy, owner), "providers", "custom:foo", "api_key"); key != "foo-token" {
		t.Fatalf("recovered key %v", key)
	}
	writeProfileConfig(t, profileHome, config("rotated-foo"))
	if _, err := policy.config(ctx, owner(), true); refusalReason(err) == "" || refusalField(err) != "providers.custom:foo.api_key" {
		t.Fatalf("rotated key: %v field %q", err, refusalField(err))
	}
}

func TestABareCustomKeySelectedByUrlIsTheRoute(t *testing.T) {
	config := func(other, local string) string {
		return `{"model": {"provider": "custom", "base_url": "http://127.0.0.1:1234/v1", "default": "m"}, "custom_providers": [` +
			`{"name": "Other", "base_url": "http://127.0.0.1:9999/v1", "api_key": "` + other + `"}, ` +
			`{"name": "Local", "base_url": "http://127.0.0.1:1234/v1/", "api_key": "` + local + `"}]}`
	}
	ctx, profileHome, owner, policy := bindLoadedSession(t, config("other-token", "url-token"))
	if key := configAt(t, recoveredConfig(t, ctx, policy, owner), "custom_providers", 1, "api_key"); key != "url-token" {
		t.Fatalf("recovered key %v", key)
	}
	writeProfileConfig(t, profileHome, config("rotated-other", "url-token"))
	if key := configAt(t, recoveredConfig(t, ctx, policy, owner), "custom_providers", 0, "api_key"); key != nil {
		t.Fatalf("unrelated entry key %v", key)
	}
	writeProfileConfig(t, profileHome, config("rotated-other", "rotated-url-token"))
	_, err := policy.config(ctx, owner(), true)
	refusal, _ := err.(*state.RuntimeStoreError)
	if refusal == nil || refusalField(err) != "custom_providers.1.api_key" {
		t.Fatalf("rotated route key: %v field %q", err, refusalField(err))
	}
	if refusal.Detail != nil && strings.Contains(*refusal.Detail, "rotated") {
		t.Fatalf("the refusal detail leaks a key: %s", *refusal.Detail)
	}
}

// Policy carrying the reference the first release of frozen config secrets wrote for it
func legacyReferenced(t *testing.T, policy *LocalSessionPolicy) *LocalSessionPolicy {
	t.Helper()
	decoded, err := pyjson.Loads(strings.TrimPrefix(*policy.ConfigSecretRef, configSecretPrefix))
	if err != nil {
		t.Fatal(err)
	}
	source := decoded.(*pyjson.Object)
	data := policy.asDict()
	for _, later := range []string{"credential_ref", "config_secret_ref", "kanban_json", "safe_mode", "ignore_user_config", "skills_prompt",
		"editor_mcp_json", "coding_posture", "client_env_json", "client_env_remote", "client_env_rules_json", "edit_approval_policy"} {
		delete(data, later)
	}
	hashed, err := pyjson.Dumps(data, pyjson.SortKeys(true))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(hashed))
	source.Set("policy", hex.EncodeToString(sum[:]))
	source.Delete("bound")
	text, err := pyjson.Dumps(source, pyjson.SortKeys(true))
	if err != nil {
		t.Fatal(err)
	}
	legacy := *policy
	ref := configSecretPrefix + text
	legacy.ConfigSecretRef = &ref
	return &legacy
}

func TestALegacyReferenceKeepsTheEndpointGuard(t *testing.T) {
	ctx, _, profileHome, owner, policy := launchBoundPolicy(t)
	legacy := legacyReferenced(t, policy)
	if key := policyModelKey(t, ctx, legacy, owner(profileHome, "owned")); key != "route-token" {
		t.Fatalf("legacy reference recovered %v", key)
	}
	stamped := *legacy
	later := "set after the stamp"
	stamped.SkillsPrompt = &later
	for name, moved := range map[string]*LocalSessionPolicy{
		"endpoint": repointedPolicy(t, legacy, "http://evil.invalid/v1", "model", "base_url"), "policy": &stamped} {
		_, err := moved.config(ctx, owner(profileHome, "owned"), true)
		refusal, _ := err.(*state.RuntimeStoreError)
		if refusalReason(err) != "launch_credentials_unavailable" || refusalField(err) != "config_secret_ref" ||
			refusal.Detail == nil || *refusal.Detail == "" {
			t.Fatalf("%s rewrite: %v field %q", name, err, refusalField(err))
		}
	}
}
