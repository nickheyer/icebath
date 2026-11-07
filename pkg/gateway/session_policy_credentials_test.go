package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

type secretOwnerDouble struct {
	dbPath, profile string
	slots           *launchSecretSlots
}

func (o *secretOwnerDouble) launchOwnerDBPath() string             { return o.dbPath }
func (o *secretOwnerDouble) launchOwnerProfileID() string          { return o.profile }
func (o *secretOwnerDouble) launchSecretSlots() *launchSecretSlots { return o.slots }

const boundProfileConfig = `{"model": {"provider": "custom", "api_key": "route-token", "base_url": "http://127.0.0.1:1234/v1"},
 "mcp_servers": {"docs": {"command": "docs-mcp", "env": {"DOCS_TOKEN": "mcp-token"}}}}`

func frozenPolicy(configJSON string) *LocalSessionPolicy {
	model := "frozen"
	return &LocalSessionPolicy{Source: "cli", Platform: "cli", Cwd: "/work", Model: &model, Toolsets: []string{"terminal"},
		ConfigJSON: configJSON, RequestJSON: `{"source": "cli"}`, TerminalJSON: `{"TERMINAL_ENV": "local", "TERMINAL_CWD": "/work"}`,
		EditApprovalPolicy: "ask"}
}

func boundSecretPolicy(t *testing.T) (string, func(path, profile string) *secretOwnerDouble, *LocalSessionPolicy) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(boundProfileConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	owner := func(path, profile string) *secretOwnerDouble {
		if path == "" {
			path = home
		}
		if profile == "" {
			profile = "owned"
		}
		return &secretOwnerDouble{dbPath: filepath.Join(path, "state.db"), profile: profile, slots: &launchSecretSlots{}}
	}
	policy := frozenPolicy(`{"model": {"provider": "custom", "api_key": null, "base_url": "http://127.0.0.1:1234/v1"}, ` +
		`"mcp_servers": {"docs": {"command": "docs-mcp", "env": null}}}`)
	secrets := &RecoveredSecrets{}
	secrets.Set(secretPath{"model", "api_key"}, "route-token")
	secrets.Set(secretPath{"mcp_servers", "docs", "env"}, map[string]any{"DOCS_TOKEN": "mcp-token"})
	ref, err := configReference(context.Background(), owner("", ""), "session", policy, secrets)
	if err != nil {
		t.Fatal(err)
	}
	policy.ConfigSecretRef = &ref
	return home, owner, policy
}

func refusedField(t *testing.T, err error) (string, string) {
	t.Helper()
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) || refusal.Reason != "launch_credentials_unavailable" {
		t.Fatalf("want launch_credentials_unavailable, got %v", err)
	}
	field, detail := "", ""
	if refusal.Field != nil {
		field = *refusal.Field
	}
	if refusal.Detail != nil {
		detail = *refusal.Detail
	}
	return field, detail
}

func TestNonSecretPolicyChangeKeepsTheReference(t *testing.T) {
	ctx := context.Background()
	_, owner, policy := boundSecretPolicy(t)
	config := policyObject(policy.ConfigJSON)
	config["agent"] = map[string]any{"reasoning_effort": "low"}
	config["mcp_servers"].(map[string]any)["extra"] = map[string]any{"command": "extra-mcp"}
	text, _ := pyjson.Dumps(config)
	moved := *policy
	other, later := "other", "later field"
	moved.Model, moved.Toolsets, moved.SkillsPrompt, moved.ConfigJSON = &other, []string{"web"}, &later, text
	recovered, err := recoverConfigSecrets(ctx, owner("", ""), &moved)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := recovered.Get(secretPath{"model", "api_key"}); v != "route-token" {
		t.Fatalf("route key %v", v)
	}
	if v, _ := recovered.Get(secretPath{"mcp_servers", "docs", "env"}); v.(map[string]any)["DOCS_TOKEN"] != "mcp-token" {
		t.Fatalf("mcp env %v", v)
	}
}

func TestAnotherHomeOrProfileIsRefusedByName(t *testing.T) {
	ctx := context.Background()
	_, owner, policy := boundSecretPolicy(t)
	for scope, wrong := range map[string]*secretOwnerDouble{"home": owner(t.TempDir(), ""), "profile": owner("", "other")} {
		_, err := recoverConfigSecrets(ctx, wrong, policy)
		if field, detail := refusedField(t, err); field != scope || detail == "" {
			t.Fatalf("%s refusal %q %q", scope, field, detail)
		}
	}
}

func TestRevokedRouteSecretIsStillRefused(t *testing.T) {
	home, owner, policy := boundSecretPolicy(t)
	cfg := strings.Replace(boundProfileConfig, `"api_key": "route-token", `, "", 1)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := recoverConfigSecrets(context.Background(), owner("", ""), policy)
	if field, detail := refusedField(t, err); field != "model.api_key" || detail != "removed from config.yaml since this session started" {
		t.Fatalf("refusal %q %q", field, detail)
	}
}

func TestSecretPairedWithAMovedEndpointIsRefusedByName(t *testing.T) {
	ctx := context.Background()
	_, owner, policy := boundSecretPolicy(t)
	for moved, field := range map[string]string{"model": "model.api_key", "mcp": "mcp_servers.docs.env"} {
		config := policyObject(policy.ConfigJSON)
		if moved == "model" {
			config["model"].(map[string]any)["base_url"] = "http://attacker.invalid/v1"
		} else {
			config["mcp_servers"].(map[string]any)["docs"].(map[string]any)["command"] = "other-mcp"
		}
		repointed := *policy
		repointed.ConfigJSON, _ = pyjson.Dumps(config)
		_, err := recoverConfigSecrets(ctx, owner("", ""), &repointed)
		if got, detail := refusedField(t, err); got != field || !strings.Contains(detail, "endpoint") {
			t.Fatalf("%s refusal %q %q", moved, got, detail)
		}
	}
}

func TestDroppedNonRouteSecretIsSkippedNotSubstituted(t *testing.T) {
	home, owner, policy := boundSecretPolicy(t)
	cfg := strings.Replace(boundProfileConfig, "mcp-token", "rotated", 1)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverConfigSecrets(context.Background(), owner("", ""), policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := recovered.Get(secretPath{"mcp_servers", "docs", "env"}); held || len(recovered.dropped) != 1 || recovered.session != "session" {
		t.Fatalf("recovered %+v", recovered)
	}
}

func TestLegacyReferenceKeepsTheWholePolicyGuard(t *testing.T) {
	ctx := context.Background()
	_, owner, policy := boundSecretPolicy(t)
	source := policyObject(strings.TrimPrefix(*policy.ConfigSecretRef, configSecretPrefix))
	delete(source, "bound")
	data := policy.asDict()
	for _, name := range []string{"credential_ref", "config_secret_ref", "kanban_json", "safe_mode", "ignore_user_config", "skills_prompt",
		"editor_mcp_json", "coding_posture", "client_env_json", "client_env_remote", "client_env_rules_json", "edit_approval_policy"} {
		delete(data, name)
	}
	source["policy"] = sortedDigest(data)
	text, _ := pyjson.Dumps(source, pyjson.SortKeys(true))
	legacy := *policy
	ref := configSecretPrefix + text
	legacy.ConfigSecretRef = &ref
	if _, err := recoverConfigSecrets(ctx, owner("", ""), &legacy); err != nil {
		t.Fatalf("older gateway reference refused: %v", err)
	}
	other := "other"
	legacy.Model = &other
	if field, _ := refusedField(t, func() error { _, err := recoverConfigSecrets(ctx, owner("", ""), &legacy); return err }()); field != "config_secret_ref" {
		t.Fatalf("legacy guard field %q", field)
	}
}

func TestInMemoryReferenceAndUnreadableReference(t *testing.T) {
	ctx := context.Background()
	_, owner, policy := boundSecretPolicy(t)
	slotRef := "inst:2:session"
	held := &RecoveredSecrets{}
	held.Set(secretPath{"model", "api_key"}, "k")
	o := owner("", "")
	o.slots.configs = map[string]*RecoveredSecrets{slotRef: held}
	memory := *policy
	memory.ConfigSecretRef = &slotRef
	if got, err := recoverConfigSecrets(ctx, o, &memory); err != nil || got.Len() != 1 {
		t.Fatalf("in memory %v %v", got, err)
	}
	if _, err := recoverConfigSecrets(ctx, owner("", ""), &memory); err == nil {
		t.Fatal("lost in memory slot must refuse")
	}
	bad := configSecretPrefix + "{not json"
	memory.ConfigSecretRef = &bad
	if field, detail := refusedField(t, func() error { _, err := recoverConfigSecrets(ctx, o, &memory); return err }()); field != "config_secret_ref" || detail != "unreadable (JSONDecodeError)" {
		t.Fatalf("unreadable %q %q", field, detail)
	}
}

func TestSecretsMovedComparesFingerprintsOfTheSections(t *testing.T) {
	_, _, policy := boundSecretPolicy(t)
	same := &RecoveredSecrets{}
	same.Set(secretPath{"model", "api_key"}, "route-token")
	if moved, err := secretsMoved(policy.ConfigSecretRef, same, []string{"model"}); err != nil || moved {
		t.Fatalf("unchanged %v %v", moved, err)
	}
	rotated := &RecoveredSecrets{}
	rotated.Set(secretPath{"model", "api_key"}, "new")
	if moved, _ := secretsMoved(policy.ConfigSecretRef, rotated, []string{"model"}); !moved {
		t.Fatal("rotation not seen")
	}
	memory := "inst:1:s"
	if moved, _ := secretsMoved(&memory, rotated, []string{"model"}); moved {
		t.Fatal("in memory reference is unknown")
	}
}

func TestPolicyIdentityPreservesPreEditorCredentialBinding(t *testing.T) {
	policy := frozenPolicy("{}")
	legacy := policy.asDict()
	for _, name := range []string{"credential_ref", "config_secret_ref", "editor_mcp_json", "coding_posture", "client_env_json",
		"client_env_remote", "client_env_rules_json", "edit_approval_policy"} {
		delete(legacy, name)
	}
	text, _ := pyjson.Dumps(legacy, pyjson.SortKeys(true))
	sum := sha256.Sum256([]byte(text))
	if got := policyIdentity(policy, nil); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("identity %s", got)
	}
	if len(legacyIdentities(policy)) != 16 {
		t.Fatalf("legacy identities %d", len(legacyIdentities(policy)))
	}
}

func TestDisableDroppedSecretsReturnsTasksAndDropsFallbacks(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	config := map[string]any{
		"auxiliary":          map[string]any{"vision": map[string]any{"provider": "openrouter", "api_key": nil, "timeout": int64(30), "fallback_chain": []any{}}},
		"providers":          map[string]any{"team": map[string]any{"base_url": "http://team/v1", "api_key": nil}},
		"fallback_providers": []any{map[string]any{"provider": "team", "model": "m"}, map[string]any{"provider": "openrouter", "model": "x"}},
		"fallback_model":     map[string]any{"provider": "openrouter", "model": "y", "api_key": nil},
	}
	dropped := []secretPath{{"auxiliary", "vision", "api_key"}, {"providers", "team", "api_key"}, {"fallback_model", "api_key"}}
	out := disableDroppedSecrets(context.Background(), config, dropped, "s")
	vision := out["auxiliary"].(map[string]any)["vision"].(map[string]any)
	if vision["provider"] != "main" || vision["timeout"] != int64(30) || vision["api_key"] != nil || vision["fallback_chain"] != nil {
		t.Fatalf("vision %v", vision)
	}
	if chain := out["fallback_providers"].([]any); len(chain) != 1 || chain[0].(map[string]any)["provider"] != "openrouter" {
		t.Fatalf("chain %v", chain)
	}
	if _, kept := out["fallback_model"]; kept {
		t.Fatal("fallback_model must be removed")
	}
}

func TestCurrentCustomPathFollowsTheEntryIdentity(t *testing.T) {
	frozen := map[string]any{"custom_providers": []any{map[string]any{"name": "Team", "base_url": "http://a/v1"}}}
	moved := map[string]any{"custom_providers": []any{map[string]any{"name": "Other", "base_url": "http://b/v1"},
		map[string]any{"name": "Team", "base_url": "http://a/v1"}}}
	if got := currentCustomPath(frozen, moved, secretPath{"custom_providers", 0, "api_key"}, 0); !pathEqual(got, []any{"custom_providers", 1, "api_key"}) {
		t.Fatalf("moved %v", got)
	}
	renamed := map[string]any{"custom_providers": []any{map[string]any{"name": "Renamed", "base_url": "http://a/v1/"}}}
	if got := currentCustomPath(frozen, renamed, secretPath{"custom_providers", 0, "api_key"}, 0); !pathEqual(got, []any{"custom_providers", 0, "api_key"}) {
		t.Fatalf("renamed %v", got)
	}
	gone := map[string]any{"custom_providers": []any{map[string]any{"name": "Else", "base_url": "http://c/v1"}}}
	if got := currentCustomPath(frozen, gone, secretPath{"custom_providers", 0, "api_key"}, 0); !pathEqual(got, []any{"custom_providers", 1, "api_key"}) {
		t.Fatalf("gone %v", got)
	}
}

func TestLaunchKeyAndRouteLaunchKey(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	policy := frozenPolicy(`{"model": {"provider": "custom", "base_url": "http://127.0.0.1:1234/v1"}}`)
	owner := &secretOwnerDouble{slots: &launchSecretSlots{keys: map[string]string{"i:1:s": "sk"}}}
	if key, err := launchKey(owner, policy); key != nil || err != nil {
		t.Fatalf("no ref %v %v", key, err)
	}
	ref := "i:1:s#sha256:abc"
	policy.CredentialRef = &ref
	if key, err := launchKey(owner, policy); err != nil || *key != "sk" {
		t.Fatalf("held %v %v", key, err)
	}
	same := map[string]any{"model": map[string]any{"provider": "custom", "base_url": "http://127.0.0.1:1234/v1"}}
	if key, err := routeLaunchKey(ctx, owner, policy, same); err != nil || key == nil {
		t.Fatalf("same route %v %v", key, err)
	}
	moved := map[string]any{"model": map[string]any{"provider": "custom", "base_url": "http://other:1/v1"}}
	if key, err := routeLaunchKey(ctx, owner, policy, moved); err != nil || key != nil {
		t.Fatalf("moved route %v %v", key, err)
	}
	if _, err := launchKey(&secretOwnerDouble{slots: &launchSecretSlots{}}, policy); err == nil {
		t.Fatal("lost key must refuse")
	}
}

func TestRestorePolicyRejectsIncompletePolicy(t *testing.T) {
	good := frozenPolicy("{}").asDict()
	if p, err := restorePolicy(good); err != nil || p.Source != "cli" || len(p.Toolsets) != 1 {
		t.Fatalf("restore %v %v", p, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"unknown field":   func(d map[string]any) { d["extra"] = 1 },
		"missing model":   func(d map[string]any) { delete(d, "model") },
		"relative cwd":    func(d map[string]any) { d["cwd"] = "work" },
		"blank model":     func(d map[string]any) { d["model"] = " " },
		"bad platform":    func(d map[string]any) { d["platform"] = "tui" },
		"safe w/o ignore": func(d map[string]any) { d["safe_mode"] = true },
		"edit outside acp": func(d map[string]any) {
			d["edit_approval_policy"] = "session"
		},
		"list config":   func(d map[string]any) { d["config_json"] = "[]" },
		"host terminal": func(d map[string]any) { d["terminal_json"] = `{"TERMINAL_ENV": "local", "TERMINAL_CWD": "/elsewhere"}` },
	} {
		data := frozenPolicy("{}").asDict()
		mutate(data)
		_, err := restorePolicy(data)
		var refusal *state.RuntimeStoreError
		if !errors.As(err, &refusal) || refusal.Reason != "storage_unavailable" {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestForwardPolicyAndAuthor(t *testing.T) {
	policy := frozenPolicy("{}")
	policy.Source, policy.Platform = "a2a", "a2a"
	policy.RequestJSON = `{"source": "a2a", "a2a_identity": ["agent", "tenant", "peer", "ctx"]}`
	if !isForwardPolicy(policy) {
		t.Fatal("forward policy not recognised")
	}
	if author := forwardAuthor(policy); author["id"] != "peer" || author["name"] != "peer" || author["is_bot"] != true {
		t.Fatalf("author %v", author)
	}
	if p, err := restorePolicy(policy.asDict()); err != nil || p.Source != "a2a" {
		t.Fatalf("restore forward %v %v", p, err)
	}
	policy.RequestJSON = `{"source": "a2a", "a2a_identity": ["agent", "tenant", "peer"]}`
	if isForwardPolicy(policy) || forwardAuthor(policy) != nil || isForwardPolicy(nil) {
		t.Fatal("three part identity is not a forward policy")
	}
}

func TestUnpinReasoningFollowsTheProfile(t *testing.T) {
	config := map[string]any{"agent": map[string]any{"reasoning_effort": "high"}, SessionPins: map[string]any{"service_tier": true}}
	profile := map[string]any{"agent": map[string]any{"reasoning_overrides": map[string]any{"m": "low"}}}
	if err := unpinReasoning(config, profile); err != nil {
		t.Fatal(err)
	}
	agent := config["agent"].(map[string]any)
	if _, kept := agent["reasoning_effort"]; kept || agent["reasoning_overrides"] == nil {
		t.Fatalf("agent %v", agent)
	}
	if pins := config[SessionPins].(map[string]any); pins["reasoning"] != false || pins["service_tier"] != true {
		t.Fatalf("pins %v", pins)
	}
}

func TestEffectiveRecoversFrozenSecretsForTheOwner(t *testing.T) {
	ctx := context.Background()
	_, owner, policy := boundSecretPolicy(t)
	config, err := policy.config(ctx, owner("", ""), true)
	if err != nil {
		t.Fatal(err)
	}
	if config["model"].(map[string]any)["api_key"] != "route-token" {
		t.Fatalf("model %v", config["model"])
	}
	if env := config["mcp_servers"].(map[string]any)["docs"].(map[string]any)["env"]; env.(map[string]any)["DOCS_TOKEN"] != "mcp-token" {
		t.Fatalf("env %v", env)
	}
	if frozen := policy.frozenConfig(); frozen["model"].(map[string]any)["api_key"] != nil {
		t.Fatalf("no owner must recover nothing %v", frozen["model"])
	}
	if policy.Provider() != "custom" || policy.BaseURL() != "http://127.0.0.1:1234/v1" {
		t.Fatalf("route %v %v", policy.Provider(), policy.BaseURL())
	}
}

func TestEffectiveFollowingSessionRunsTheProfile(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	live := `{"model": {"default": "live-model", "provider": "openrouter"}, "agent": {"reasoning_effort": "low"}}`
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}
	owner := &secretOwnerDouble{dbPath: filepath.Join(home, "state.db"), profile: "owned", slots: &launchSecretSlots{}}
	policy := frozenPolicy(`{"model": {"default": "frozen", "provider": "anthropic"}, "agent": {"reasoning_effort": "high"}}`)
	policy.RequestJSON = `{"source": "cli", "follow_profile_config": true}`
	config, model, err := policy.effective(ctx, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	if *model != "live-model" || config["agent"].(map[string]any)["reasoning_effort"] != "low" || config["model"].(map[string]any)["provider"] != "openrouter" {
		t.Fatalf("followed %v %v", *model, config)
	}
	policy.RequestJSON = `{"source": "cli", "follow_profile_config": true, "reasoning": "high"}`
	if config, _ = policy.config(ctx, owner, false); config["agent"].(map[string]any)["reasoning_effort"] != "high" {
		t.Fatalf("pinned reasoning %v", config["agent"])
	}
	held := map[string]any{"model": map[string]any{"default": "held-model"}}
	if got, _ := policy.currentModel(withTurnProfile(ctx, policy, held), owner); *got != "held-model" {
		t.Fatalf("turn scope read %v", *got)
	}
	if got, _ := policy.readProfile(ctx, nil); got != nil {
		t.Fatal("no owner follows nothing")
	}
}

func TestFrozenAccessors(t *testing.T) {
	policy := frozenPolicy(`{"checkpoints": true, "agent": {"max_turns": 7, "reasoning_effort": "high"}}`)
	if !policy.CheckpointsEnabled() || policy.MaxTurns() != 7 {
		t.Fatalf("checkpoints %v turns %d", policy.CheckpointsEnabled(), policy.MaxTurns())
	}
	if rc := policy.ReasoningConfig(); rc["effort"] != "high" {
		t.Fatalf("reasoning %v", rc)
	}
	bare := frozenPolicy(`{"checkpoints": {"enabled": false}}`)
	if bare.CheckpointsEnabled() || bare.MaxTurns() != config.TurnLimitUnlimited {
		t.Fatalf("bare %v %d", bare.CheckpointsEnabled(), bare.MaxTurns())
	}
}

func TestClientEnvScopeBindsTheSessionEnvironment(t *testing.T) {
	policy := frozenPolicy("{}")
	env := `{"PATH": "/client/bin"}`
	policy.ClientEnvJSON = &env
	ctx, err := policy.ClientEnvScope(context.Background())
	if err != nil || state.BoundClientEnvBinding(ctx) == nil {
		t.Fatalf("binding %v", err)
	}
}

func TestExtractConfigSecretsLeavesNoneInTheFrozenTree(t *testing.T) {
	cfg := map[string]any{
		"model":              map[string]any{"provider": "custom", "api_key": "route-token", "base_url": "http://x/v1"},
		"mcp_servers":        map[string]any{"docs": map[string]any{"command": "docs-mcp", "env": map[string]any{"DOCS_TOKEN": "t"}, "headers": "{}"}},
		"fallback_providers": []any{map[string]any{"provider": "team", "OPENAI_API_KEY": "fk"}},
		"auxiliary":          map[string]any{"vision": map[string]any{"token": ""}},
	}
	private := &RecoveredSecrets{}
	if err := extractConfigSecrets(cfg, private, nil); err != nil {
		t.Fatal(err)
	}
	want := []secretPath{{"fallback_providers", 0, "OPENAI_API_KEY"}, {"mcp_servers", "docs", "env"}, {"model", "api_key"}}
	if private.Len() != len(want) {
		t.Fatalf("private %v", private.paths)
	}
	for i, path := range want {
		if !pathEqual(private.paths[i], path) {
			t.Fatalf("path %d %v", i, private.paths[i])
		}
	}
	if cfg["model"].(map[string]any)["api_key"] != nil || cfg["mcp_servers"].(map[string]any)["docs"].(map[string]any)["headers"] != "{}" {
		t.Fatalf("frozen tree %v", cfg)
	}
	if err := extractConfigSecrets(map[string]any{"api_key": "k"}, nil, nil); err == nil {
		t.Fatal("no private holder must refuse")
	}
}

func TestWorkspaceReadyAndProfileReadScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	policy := frozenPolicy(`{"model": {"default": "frozen"}}`)
	policy.Cwd = home
	if !workspaceReady(policy) {
		t.Fatal("existing workspace not ready")
	}
	policy.Cwd = filepath.Join(home, "gone")
	if workspaceReady(policy) {
		t.Fatal("missing workspace ready")
	}
	policy.RequestJSON = `{"source": "cli", "follow_profile_config": true}`
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte(`{"model": {"default": "first"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	owner := &secretOwnerDouble{dbPath: filepath.Join(home, "state.db"), profile: "owned", slots: &launchSecretSlots{}}
	ctx, err := profileReadScope(context.Background(), policy, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"model": {"default": "second"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := policy.currentModel(ctx, owner); *got != "first" {
		t.Fatalf("scope read %v", *got)
	}
	if got, _ := policy.currentModel(context.Background(), owner); *got != "second" {
		t.Fatalf("fresh read %v", *got)
	}
}
