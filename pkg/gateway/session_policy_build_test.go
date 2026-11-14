package gateway

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func policyBuildHome(t *testing.T) context.Context {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return context.Background()
}

func mustBuildPolicy(t *testing.T, ctx context.Context, params, cfg map[string]any) *LocalSessionPolicy {
	t.Helper()
	policy, err := buildPolicy(ctx, params, cfg, buildPolicyOptions{})
	if err != nil {
		t.Fatalf("buildPolicy(%v): %v", params, err)
	}
	return policy
}

func refusedInvalid(t *testing.T, ctx context.Context, params, cfg map[string]any) {
	t.Helper()
	_, err := buildPolicy(ctx, params, cfg, buildPolicyOptions{})
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) || refusal.Reason != "invalid_params" {
		t.Fatalf("buildPolicy(%v) = %v, want invalid_params", params, err)
	}
}

func policyModel(t *testing.T, ctx context.Context, policy *LocalSessionPolicy) map[string]any {
	t.Helper()
	cfg, err := policy.config(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := cfg["model"].(map[string]any)
	return model
}

func TestPolicySelectsSurfaceAndRefusesBadCreateFields(t *testing.T) {
	ctx := policyBuildHome(t)
	cfg := map[string]any{"platform_toolsets": map[string]any{"cli": []any{"terminal"}}}
	var policies []*LocalSessionPolicy
	for _, source := range []string{"cli", "tui", "gui"} {
		policies = append(policies, mustBuildPolicy(t, ctx, map[string]any{"source": source, "cwd": t.TempDir(), "model": source}, cfg))
	}
	if policies[2].Platform != "desktop" || !slices.Contains(policies[2].Toolsets, "desktop_ui") {
		t.Fatalf("gui policy %q %v", policies[2].Platform, policies[2].Toolsets)
	}
	if !slices.Contains(policies[2].Toolsets, "catalog") {
		t.Fatalf("gui toolsets %v lack catalog", policies[2].Toolsets)
	}
	for _, p := range policies[:2] {
		if slices.Contains(p.Toolsets, "desktop_ui") || slices.Contains(p.Toolsets, "catalog") {
			t.Fatalf("%s toolsets %v carry a desktop surface", p.Source, p.Toolsets)
		}
	}
	cfg["platform_toolsets"].(map[string]any)["cli"] = []any{}
	if !slices.Contains(policies[0].Toolsets, "terminal") {
		t.Fatalf("frozen cli toolsets %v lost terminal", policies[0].Toolsets)
	}
	for _, params := range []map[string]any{{"source": "cron"}, {"toolsets": []any{"not-a-toolset"}}, {"cwd": "."},
		{"provider": 12}, {"skills": "x"}, {"skills": []any{}}, {"pass_session_id": 1},
		{"toolsets": []any{"desktop_ui"}, "source": "cli"}} {
		refusedInvalid(t, ctx, params, map[string]any{})
	}
}

func TestLaunchOptionsAreFrozenAndValidated(t *testing.T) {
	ctx := policyBuildHome(t)
	dir := t.TempDir()
	cfg := map[string]any{"agent": map[string]any{"max_turns": 8, "reasoning_effort": "low"}}
	params := map[string]any{"cwd": dir, "provider": "custom", "base_url": "http://127.0.0.1:1234/v1",
		"model": "fixture", "reasoning": "high", "max_turns": 3, "ignore_rules": true}
	policy := mustBuildPolicy(t, ctx, params, cfg)
	if policy.Provider() != "custom" || policy.BaseURL() != params["base_url"] {
		t.Fatalf("route %v %v", policy.Provider(), policy.BaseURL())
	}
	if !policy.IgnoreRules() || policy.MaxTurns() != 3 {
		t.Fatalf("ignore_rules %v max_turns %d", policy.IgnoreRules(), policy.MaxTurns())
	}
	high := config.ParseReasoningEffort("high")
	if !reflect.DeepEqual(policy.ReasoningConfig(), high) {
		t.Fatalf("reasoning %v", policy.ReasoningConfig())
	}
	cfg["agent"].(map[string]any)["reasoning_effort"] = "none"
	if !reflect.DeepEqual(policy.ReasoningConfig(), high) {
		t.Fatalf("reasoning followed live config %v", policy.ReasoningConfig())
	}
	if mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "ignore_rules": false}, cfg).IgnoreRules() {
		t.Fatal("ignore_rules false read as true")
	}
	for _, bad := range []map[string]any{{"max_turns": true}, {"max_turns": "garbage"}, {"max_turns": 2.5},
		{"reasoning": "garbage"}, {"ignore_rules": "false"}, {"base_url": "http://user:secret@localhost/v1"}} {
		bad["cwd"] = dir
		refusedInvalid(t, ctx, bad, cfg)
	}
}

func TestProviderOverrideDropsThePreviousProvidersRoute(t *testing.T) {
	ctx := policyBuildHome(t)
	dir := t.TempDir()
	cfg := map[string]any{"model": map[string]any{"default": "deepseek-chat", "provider": "deepseek",
		"base_url": "https://api.deepseek.com/v1", "api_mode": "chat_completions",
		"key_env": "ICEBATH_CUSTOM_QWENAI_API_KEY"}}
	policy := mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "model": "mimo-v2.6-flash", "provider": "xiaomi"}, cfg)
	if got := policyModel(t, ctx, policy); !reflect.DeepEqual(got, map[string]any{"default": "deepseek-chat", "provider": "xiaomi"}) {
		t.Fatalf("provider pick kept the old route %v", got)
	}
	explicit := mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "model": "m", "provider": "xiaomi",
		"base_url": "https://proxy.example/v1"}, cfg)
	want := map[string]any{"default": "deepseek-chat", "provider": "xiaomi", "base_url": "https://proxy.example/v1"}
	if got := policyModel(t, ctx, explicit); !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit url pick %v", got)
	}
	same := mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "model": "deepseek-r2", "provider": "deepseek"}, cfg)
	if got := policyModel(t, ctx, same)["base_url"]; got != "https://api.deepseek.com/v1" {
		t.Fatalf("same provider pick dropped its endpoint %v", got)
	}
	unrecorded := mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "provider": "xiaomi"},
		map[string]any{"model": map[string]any{"default": "m", "base_url": "https://api.deepseek.com/v1", "api_key": "sk-inline"}})
	if got := policyModel(t, ctx, unrecorded); !reflect.DeepEqual(got, map[string]any{"default": "m", "provider": "xiaomi"}) {
		t.Fatalf("unrecorded profile route %v", got)
	}
}

func TestProviderChangeDropsTheOldProvidersContextWindow(t *testing.T) {
	ctx := policyBuildHome(t)
	dir := t.TempDir()
	cfg := map[string]any{"model": map[string]any{"default": "qwen", "provider": "custom",
		"base_url": "http://localhost:1234/v1", "context_length": 8192}}
	moved := mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "model": "claude-opus-4.6", "provider": "anthropic"}, cfg)
	if _, kept := policyModel(t, ctx, moved)["context_length"]; kept {
		t.Fatal("provider change kept the old context window")
	}
	alias := mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "model": "qwen", "provider": "vllm"}, cfg)
	if got, _, _ := launchNumber(policyModel(t, ctx, alias)["context_length"]); got != 8192 {
		t.Fatalf("alias pick context window %v", got)
	}
}

func TestProviderAliasSpellingAndAutoPickKeepTheProfilesRoute(t *testing.T) {
	ctx := policyBuildHome(t)
	dir := t.TempDir()
	for _, pair := range [][2]string{{"deepinfra", "deep-infra"}, {"deep-infra", "deepinfra"}} {
		cfg := map[string]any{"model": map[string]any{"default": "m", "provider": pair[0],
			"base_url": "http://localhost:4321/v1", "api_mode": "chat_completions"}}
		model := policyModel(t, ctx, mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "model": "m", "provider": pair[1]}, cfg))
		if model["base_url"] != "http://localhost:4321/v1" || model["api_mode"] != "chat_completions" {
			t.Fatalf("%s pick dropped the profile route %v", pair[1], model)
		}
	}
	cfg := map[string]any{"model": map[string]any{"default": "qwen", "provider": "custom",
		"base_url": "http://localhost:1234/v1", "key_env": "LOCAL_KEY"}}
	policy := mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "model": "qwen", "provider": "auto"}, cfg)
	if got := policyModel(t, ctx, policy); got["provider"] != "custom" || got["base_url"] != "http://localhost:1234/v1" ||
		got["key_env"] != "LOCAL_KEY" {
		t.Fatalf("auto pick replaced the profile route %v", got)
	}
}

func TestBarePickOfAShadowedCustomEntryKeepsTheProfilesRoute(t *testing.T) {
	ctx := policyBuildHome(t)
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	newStyle := map[string]any{"providers": map[string]any{"deepseek": map[string]any{"name": "deepseek",
		"api": "https://api.deepseek.com", "key_env": "DEEPSEEK_ENTRY_KEY", "transport": "chat_completions",
		"default_model": "deepseek-flash"}}}
	legacy := map[string]any{"custom_providers": []any{map[string]any{"name": "deepseek", "base_url": "https://api.deepseek.com",
		"key_env": "DEEPSEEK_ENTRY_KEY", "model": "deepseek-flash"}}}
	for _, extra := range []map[string]any{newStyle, legacy} {
		cfg := map[string]any{"model": map[string]any{"default": "deepseek-flash", "provider": "custom:deepseek",
			"base_url": "https://api.deepseek.com", "key_env": "DEEPSEEK_ENTRY_KEY"}}
		for k, v := range extra {
			cfg[k] = v
		}
		model := policyModel(t, ctx, mustBuildPolicy(t, ctx, map[string]any{"cwd": t.TempDir(), "model": "deepseek-flash",
			"provider": "deepseek"}, cfg))
		if model["provider"] != "custom:deepseek" || model["base_url"] != "https://api.deepseek.com" ||
			model["key_env"] != "DEEPSEEK_ENTRY_KEY" {
			t.Fatalf("shadowed entry pick %v under %v", model, extra)
		}
	}
}

func TestUnlimitedMaxTurnsSpellingsCreateAnUncappedSession(t *testing.T) {
	ctx := policyBuildHome(t)
	dir := t.TempDir()
	cfg := map[string]any{"agent": map[string]any{"max_turns": 8}}
	for _, spelling := range []any{0, -1, "none", "unlimited", "0"} {
		if got := mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "max_turns": spelling}, cfg).MaxTurns(); got != math.MaxInt {
			t.Fatalf("max_turns %v froze %d", spelling, got)
		}
	}
	if got := mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "max_turns": "12"}, cfg).MaxTurns(); got != 12 {
		t.Fatalf("max_turns 12 froze %d", got)
	}
}

func TestNullConfigSectionsReadAsAbsent(t *testing.T) {
	ctx := policyBuildHome(t)
	cfg := map[string]any{"gateway": nil, "display": nil, "platform_toolsets": map[string]any{"cli": []any{"terminal"}}}
	policy := mustBuildPolicy(t, ctx, map[string]any{"source": "cli", "cwd": t.TempDir()}, cfg)
	frozen, err := policy.config(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if gateway, _ := frozen["gateway"].(map[string]any); gateway["managed_workers"] != nil {
		t.Fatalf("null gateway section %v", frozen["gateway"])
	}
	legacy := *policy
	legacy.ConfigJSON = `{"gateway": null, "display": null, "platform_toolsets": {"cli": ["terminal"]}}`
	old, err := legacy.config(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := old["display"]; present {
		t.Fatalf("persisted null display section read as present %v", old)
	}
}

func TestRunBudgetIsAFrozenLaunchOption(t *testing.T) {
	ctx := policyBuildHome(t)
	held := NormalizeRunBudgetSeconds
	NormalizeRunBudgetSeconds = func(value any) *float64 {
		if _, isBool := value.(bool); isBool {
			return nil
		}
		if seconds, _, ok := launchNumber(value); ok && seconds > 0 {
			return &seconds
		}
		return nil
	}
	t.Cleanup(func() { NormalizeRunBudgetSeconds = held })
	dir := t.TempDir()
	cfg := map[string]any{"agent": map[string]any{"run_budget_seconds": 300}}
	budget := func(params, cfg map[string]any) *float64 {
		got, err := mustBuildPolicy(t, ctx, params, cfg).RunBudgetSeconds(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := budget(map[string]any{"cwd": dir, "run_budget": 120}, cfg); got == nil || *got != 120 {
		t.Fatalf("run_budget 120 froze %v", got)
	}
	if got := budget(map[string]any{"cwd": dir}, cfg); got == nil || *got != 300 {
		t.Fatalf("profile budget %v", got)
	}
	if got := budget(map[string]any{"cwd": dir}, map[string]any{}); got != nil {
		t.Fatalf("no budget read as %v", *got)
	}
	for _, bad := range []any{0, -5, true, "soon"} {
		refusedInvalid(t, ctx, map[string]any{"cwd": dir, "run_budget": bad}, cfg)
	}
}

func TestVerboseIsAFrozenLaunchOption(t *testing.T) {
	ctx := policyBuildHome(t)
	dir := t.TempDir()
	if !mustBuildPolicy(t, ctx, map[string]any{"cwd": dir, "verbose": true}, map[string]any{}).Verbose() {
		t.Fatal("verbose true lost")
	}
	if mustBuildPolicy(t, ctx, map[string]any{"cwd": dir}, map[string]any{}).Verbose() {
		t.Fatal("verbose defaulted on")
	}
	refusedInvalid(t, ctx, map[string]any{"cwd": dir, "verbose": "yes"}, map[string]any{})
}

func TestForwardPolicyIsMintedOnlyUnderAPeerIdentity(t *testing.T) {
	ctx := policyBuildHome(t)
	params := map[string]any{"source": "a2a", "request_id": "r1"}
	refusedInvalid(t, ctx, params, map[string]any{})
	forwarded := withForwardIdentity(ctx, []string{"owner", "profile", "peer", "conversation"})
	if _, err := buildPolicy(forwarded, map[string]any{"source": "a2a", "request_id": "r1", "cwd": "/"}, map[string]any{}, buildPolicyOptions{}); err == nil {
		t.Fatal("forward create with extra fields accepted")
	}
	policy, err := buildPolicy(forwarded, params, map[string]any{}, buildPolicyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !isForwardPolicy(policy) || forwardAuthor(policy)["id"] != "peer" {
		t.Fatalf("forward policy %+v", policy)
	}
}

func focusConfig() map[string]any {
	return map[string]any{"agent": map[string]any{"coding_context": "focus"},
		"platform_toolsets": map[string]any{"cli": []any{"terminal"}, "acp": []any{"terminal"}}}
}

func postureProject(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[project]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func postureCreate(t *testing.T, ctx context.Context, params, cfg map[string]any, defaultTerminal bool) *LocalSessionPolicy {
	t.Helper()
	policy, err := buildPolicy(ctx, params, cfg, buildPolicyOptions{WorkspacePosture: true, DefaultTerminal: defaultTerminal})
	if err != nil {
		t.Fatalf("buildPolicy(%v): %v", params, err)
	}
	return policy
}

func plainNotes(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "notes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFocusInACodeWorkspaceSelectsTheCodingPosture(t *testing.T) {
	ctx := policyBuildHome(t)
	project := postureProject(t)
	for _, source := range []string{"cli", "tui", "gui", "acp", "oneshot"} {
		policy := postureCreate(t, ctx, map[string]any{"source": source, "cwd": project}, focusConfig(), false)
		if !slices.Contains(policy.Toolsets, "coding") || slices.Contains(policy.Toolsets, "terminal") {
			t.Fatalf("%s toolsets %v", source, policy.Toolsets)
		}
	}
}

func TestSurfaceToolsetsStillFoldInOverThePosture(t *testing.T) {
	ctx := policyBuildHome(t)
	project := postureProject(t)
	if tui := postureCreate(t, ctx, map[string]any{"source": "tui", "cwd": project}, focusConfig(), false); !slices.Contains(tui.Toolsets, "project") {
		t.Fatalf("tui toolsets %v", tui.Toolsets)
	}
	gui := postureCreate(t, ctx, map[string]any{"source": "gui", "cwd": project}, focusConfig(), false).Toolsets
	for _, name := range []string{"coding", "desktop_ui", "catalog"} {
		if !slices.Contains(gui, name) {
			t.Fatalf("gui toolsets %v lack %s", gui, name)
		}
	}
}

func TestProfileDefaultOutsideAWorkspaceOrWithoutFocus(t *testing.T) {
	ctx := policyBuildHome(t)
	project := postureProject(t)
	if got := postureCreate(t, ctx, map[string]any{"source": "cli", "cwd": plainNotes(t)}, focusConfig(), false).Toolsets; !slices.Equal(got, []string{"terminal"}) {
		t.Fatalf("plain folder toolsets %v", got)
	}
	auto := focusConfig()
	auto["agent"] = map[string]any{"coding_context": "auto"}
	if got := postureCreate(t, ctx, map[string]any{"source": "cli", "cwd": project}, auto, false).Toolsets; !slices.Equal(got, []string{"terminal"}) {
		t.Fatalf("auto profile toolsets %v", got)
	}
}

func TestAnExplicitPickWinsOverThePosture(t *testing.T) {
	ctx := policyBuildHome(t)
	policy := postureCreate(t, ctx, map[string]any{"source": "cli", "cwd": postureProject(t), "toolsets": []any{"terminal"}}, focusConfig(), false)
	if !slices.Equal(policy.Toolsets, []string{"terminal"}) {
		t.Fatalf("explicit pick toolsets %v", policy.Toolsets)
	}
}

func TestAFollowingSessionKeepsThePostureForItsOwnCwd(t *testing.T) {
	ctx := policyBuildHome(t)
	policy := postureCreate(t, ctx, map[string]any{"source": "cli", "cwd": postureProject(t), "follow_profile_config": true}, focusConfig(), false)
	current, err := policy.currentToolsets(ctx, focusConfig())
	if err != nil || !slices.Contains(current, "coding") {
		t.Fatalf("current toolsets %v %v", current, err)
	}
}

func TestAProjectFileWrittenMidSessionDoesNotSwitchToolsets(t *testing.T) {
	ctx := policyBuildHome(t)
	plain := plainNotes(t)
	policy := postureCreate(t, ctx, map[string]any{"source": "cli", "cwd": plain, "follow_profile_config": true}, focusConfig(), false)
	if err := os.WriteFile(filepath.Join(plain, "pyproject.toml"), []byte("[project]\nname = \"late\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	current, err := policy.currentToolsets(ctx, focusConfig())
	if err != nil || !slices.Equal(current, []string{"terminal"}) {
		t.Fatalf("current toolsets %v %v", current, err)
	}
}

func TestPluginMCPServersStayInThePosture(t *testing.T) {
	ctx := policyBuildHome(t)
	held := tools.PluginToolsetCatalog.PortableNowait
	tools.PluginToolsetCatalog.PortableNowait = func(context.Context) map[string]bool { return map[string]bool{"plug": true} }
	t.Cleanup(func() { tools.PluginToolsetCatalog.PortableNowait = held })
	got := postureCreate(t, ctx, map[string]any{"source": "gui", "cwd": postureProject(t)}, focusConfig(), false).Toolsets
	if !slices.Contains(got, "coding") || !slices.Contains(got, "plug") {
		t.Fatalf("gui posture toolsets %v", got)
	}
}

func TestTheDaemonsOwnDirectoryIsNotAWorkspace(t *testing.T) {
	ctx := policyBuildHome(t)
	t.Chdir(postureProject(t))
	for _, source := range []string{"tool", "gui"} {
		if got := postureCreate(t, ctx, map[string]any{"source": source}, focusConfig(), true).Toolsets; slices.Contains(got, "coding") {
			t.Fatalf("%s daemon directory toolsets %v", source, got)
		}
	}
}

func TestAProfileDefaultListingCodingIsNotReplayedAsAWorkspace(t *testing.T) {
	ctx := policyBuildHome(t)
	cfg := focusConfig()
	cfg["platform_toolsets"] = map[string]any{"cli": []any{"coding", "web"}}
	policy := postureCreate(t, ctx, map[string]any{"source": "cli", "cwd": plainNotes(t), "follow_profile_config": true}, cfg, false)
	if policy.CodingPosture || !slices.Contains(policy.Toolsets, "web") {
		t.Fatalf("posture %v toolsets %v", policy.CodingPosture, policy.Toolsets)
	}
	current, err := policy.currentToolsets(ctx, cfg)
	if err != nil || !slices.Equal(current, policy.Toolsets) {
		t.Fatalf("current toolsets %v %v", current, err)
	}
}

func TestToolSourcesAndPicksKeepTheirToolsetsInAWorkspace(t *testing.T) {
	ctx := policyBuildHome(t)
	project := postureProject(t)
	for _, params := range []map[string]any{{"source": "tool"}, {"source": "cli", "toolsets": []any{"terminal"}}} {
		params["cwd"] = project
		policy := postureCreate(t, ctx, params, focusConfig(), false)
		if policy.CodingPosture || !slices.Equal(policy.Toolsets, []string{"terminal"}) {
			t.Fatalf("%v posture %v toolsets %v", params, policy.CodingPosture, policy.Toolsets)
		}
	}
}

func TestOnlySessionCreateOptsIntoTheWorkspacePosture(t *testing.T) {
	ctx := policyBuildHome(t)
	if got := mustBuildPolicy(t, ctx, map[string]any{"source": "cli", "cwd": postureProject(t)}, focusConfig()).Toolsets; !slices.Equal(got, []string{"terminal"}) {
		t.Fatalf("plain build toolsets %v", got)
	}
}
