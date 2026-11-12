package gateway

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestSessionPolicySurfacesAndLabels(t *testing.T) {
	want := map[string]bool{"cli": true, "tui": true, "gui": true, "acp": true}
	if !reflect.DeepEqual(storedAsLocal, want) {
		t.Fatalf("stored as local %v", storedAsLocal)
	}
	if !reflect.DeepEqual(SurfaceGroups["gui"], map[string]bool{"gui": true, "desktop": true}) ||
		!reflect.DeepEqual(SurfaceGroups["desktop"], SurfaceGroups["gui"]) {
		t.Fatalf("gui group %v", SurfaceGroups["gui"])
	}
	if _, ok := SurfaceGroups["tool"]; ok {
		t.Fatal("tool is not an alias of cli")
	}
	for label, want := range map[any]any{nil: nil, "": nil, "local": nil, "unknown": nil, "gui": "desktop", "cli": "cli", "tool": "tool", "oneshot": "oneshot", "telegram": "telegram"} {
		if got := state.CreatedSurface(label); got != want {
			t.Errorf("%v: %v", label, got)
		}
	}
}

func TestSessionPinnedAndPinReasoningEffort(t *testing.T) {
	config := map[string]any{"agent": map[string]any{"reasoning_overrides": map[string]any{"m": "low"}}}
	if sessionPinned(config, "reasoning", true) != true || sessionPinned(config, "reasoning", false) != false {
		t.Fatal("no pin reads the launch state")
	}
	if err := pinReasoningEffort(config, "high", false); err != nil {
		t.Fatal(err)
	}
	if _, pinned := config[SessionPins]; pinned || config["agent"].(map[string]any)["reasoning_effort"] != "high" {
		t.Fatalf("creation pin %v", config)
	}
	if _, ok := config["agent"].(map[string]any)["reasoning_overrides"]; ok {
		t.Fatal("overrides kept")
	}
	config[SessionPins] = map[string]any{"service_tier": true}
	if err := pinReasoningEffort(config, "low", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config[SessionPins], map[string]any{"service_tier": true, "reasoning": true}) || !sessionPinned(config, "reasoning", false) {
		t.Fatalf("session pin %v", config[SessionPins])
	}
	if err := pinReasoningEffort(map[string]any{"agent": "x"}, "low", false); err == nil {
		t.Fatal("scalar agent section accepted")
	}
}

func TestRefusePolicyNamesTheField(t *testing.T) {
	detail := "must be absolute"
	err := refusePolicy("cwd", &detail)
	var rse *state.RuntimeStoreError
	if !errors.As(err, &rse) || rse.Reason != "invalid_params" || rse.Field == nil || *rse.Field != "cwd" || rse.Detail == nil || *rse.Detail != detail {
		t.Fatalf("refusal %#v", err)
	}
}

func TestProfileTargetAndModelRoute(t *testing.T) {
	for i, tc := range []struct {
		config          map[string]any
		model, provider string
	}{
		{map[string]any{"model": map[string]any{"default": " m1 ", "provider": "Auto"}}, "m1", ""},
		{map[string]any{"model": map[string]any{"default": "m2", "provider": " openrouter "}}, "m2", "openrouter"},
		{map[string]any{"model": " scalar "}, "scalar", ""},
		{map[string]any{}, "", ""},
	} {
		if m, p := profileTarget(tc.config); m != tc.model || p != tc.provider {
			t.Errorf("case %d: %q %q", i, m, p)
		}
	}
	if p, u := modelRoute(map[string]any{"provider": " OpenRouter ", "base_url": "https://openrouter.ai/api/v1/"}); p != "openrouter" || u == "" {
		t.Fatalf("route %q %q", p, u)
	}
	if p, u := modelRoute("not a block"); p != "" || u != "" {
		t.Fatalf("scalar route %q %q", p, u)
	}
}

func TestBoundRouteHoldsAndEntryServes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	team := routeTeamConfig()
	moved := map[string]any{"providers": map[string]any{"team": map[string]any{"base_url": routeB}}}
	if ok, err := boundRouteHolds(ctx, map[string]any{"provider": "team"}, team, map[string]any{"provider": "custom:team"}, team); err != nil || !ok {
		t.Fatalf("respelled provider %v %v", ok, err)
	}
	if ok, err := boundRouteHolds(ctx, map[string]any{"provider": "team"}, team, map[string]any{"provider": "team"}, moved); err != nil || ok {
		t.Fatalf("moved endpoint held %v %v", ok, err)
	}
	if ok, err := boundRouteHolds(ctx, map[string]any{"provider": "auto"}, team, map[string]any{"provider": "auto"}, team); err != nil || !ok {
		t.Fatalf("auto literal %v %v", ok, err)
	}
	if !entryServes(ctx, map[string]any{"provider": "team", "base_url": routeA}, team) {
		t.Fatal("entry serving its endpoint refused")
	}
	if entryServes(ctx, map[string]any{"provider": "team", "base_url": routeA}, moved) {
		t.Fatal("moved entry still serves")
	}
	aliased := map[string]any{"model_aliases": map[string]any{"fast": map[string]any{"base_url": routeB, "api_key": "k"}}}
	if entryServes(ctx, map[string]any{"base_url": routeA, "api_key_from": []any{"model_aliases", "fast", "api_key"}}, aliased) {
		t.Fatal("alias entry on another endpoint serves")
	}
	if !entryServes(ctx, map[string]any{"provider": "openrouter"}, team) {
		t.Fatal("block without url refused")
	}
}

func TestHeldPickAndPresentSections(t *testing.T) {
	frozen := map[string]any{"provider": "openrouter", "default": "pick", "api_key": "old"}
	live := map[string]any{"provider": "openrouter", "default": "profile", "api_key": "rotated", "context_length": 9}
	if got := heldPick(frozen, live); !reflect.DeepEqual(got, map[string]any{"provider": "openrouter", "default": "pick", "api_key": "rotated", "context_length": 9}) {
		t.Fatalf("same endpoint %v", got)
	}
	other := map[string]any{"provider": "anthropic", "default": "profile"}
	if got := heldPick(frozen, other); !reflect.DeepEqual(got, frozen) {
		t.Fatalf("other endpoint %v", got)
	}
	got := presentSections(map[string]any{"gateway": nil, "model": "m", "agent": map[string]any{}})
	if !reflect.DeepEqual(got, map[string]any{"model": map[string]any{"default": "m"}, "agent": map[string]any{}}) {
		t.Fatalf("sections %v", got)
	}
	if got := presentSections(map[string]any{"model": "  "}); !reflect.DeepEqual(got, map[string]any{"model": map[string]any{}}) {
		t.Fatalf("blank model %v", got)
	}
}

func TestTerminalUsesHostCwdAndAutoPick(t *testing.T) {
	for i, tc := range []struct {
		terminal map[string]any
		want     bool
	}{
		{map[string]any{}, true},
		{map[string]any{"TERMINAL_ENV": " Local "}, true},
		{map[string]any{"TERMINAL_ENV": "docker"}, false},
		{map[string]any{"TERMINAL_ENV": "docker", "TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE": "Yes"}, true},
		{map[string]any{"TERMINAL_ENV": "ssh", "TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE": "1"}, false},
	} {
		if got := terminalUsesHostCwd(tc.terminal); got != tc.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
	if got := withoutAutoPick(map[string]any{"provider": " AUTO ", "model": "m"}); !reflect.DeepEqual(got, map[string]any{"model": "m"}) {
		t.Fatalf("auto pick %v", got)
	}
	params := map[string]any{"provider": "openrouter"}
	if got := withoutAutoPick(params); !reflect.DeepEqual(got, params) {
		t.Fatalf("real pick %v", got)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	cfg := map[string]any{"model": map[string]any{"provider": "custom", "base_url": routeA}}
	if ok, err := keepsProfileRoute(ctx, "vllm", cfg); err != nil || !ok {
		t.Fatalf("vllm over custom %v %v", ok, err)
	}
	if ok, err := keepsProfileRoute(ctx, "anthropic", cfg); err != nil || ok {
		t.Fatalf("anthropic over custom %v %v", ok, err)
	}
	if ok, err := keepsProfileRoute(ctx, "vllm", map[string]any{}); err != nil || ok {
		t.Fatalf("auto profile %v %v", ok, err)
	}
}

func TestLocalSessionPolicyAccessors(t *testing.T) {
	env := `{"PATH": "/usr/bin"}`
	policy := &LocalSessionPolicy{Source: "cli", Platform: "cli", Cwd: "/work", RequestJSON: `{"yolo": true, "verbose": 1, "pass_session_id": false}`,
		ConfigJSON: `{"follow_adopted": true}`, TerminalJSON: `{}`, ClientEnvJSON: &env, EditApprovalPolicy: "ask"}
	if !policy.FollowsProfile() || !policy.Yolo() || !policy.Verbose() || policy.PassSessionID() || policy.IgnoreRules() {
		t.Fatalf("accessors %+v", policy)
	}
	if !reflect.DeepEqual(policy.ClientEnv(), map[string]any{"PATH": "/usr/bin"}) {
		t.Fatalf("client env %v", policy.ClientEnv())
	}
	safe := &LocalSessionPolicy{RequestJSON: `{"follow_profile_config": "yes"}`, ConfigJSON: `{}`, SafeMode: true}
	if safe.FollowsProfile() || !safe.IgnoreRules() || safe.ClientEnv() != nil {
		t.Fatalf("safe policy %+v", safe)
	}
	if platform, cwd, ok := ActiveSessionPolicy(WithActivePolicy(context.Background(), policy)); !ok || platform != "cli" || cwd != "/work" {
		t.Fatalf("active policy %q %q %v", platform, cwd, ok)
	}
}

func TestValidClientEnv(t *testing.T) {
	for i, tc := range []struct {
		value any
		rules bool
		want  bool
	}{
		{nil, false, true},
		{`{"A": "1"}`, false, true},
		{`{"A": 1}`, false, false},
		{`["A"]`, false, false},
		{`{"allow": ["A", "B"]}`, true, true},
		{`{"allow": ["A", 2]}`, true, false},
		{`{"allow": "A"}`, true, false},
		{42, false, false},
	} {
		if got := validClientEnv(tc.value, tc.rules); got != tc.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}
