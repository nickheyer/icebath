package gateway

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Store refusal reason of err, empty for any other error
func runtimeStoreReason(err error) string {
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}

// Stored policy fields with the given overrides applied
func storedWith(policy *LocalSessionPolicy, overrides map[string]any) map[string]any {
	data := policy.asDict()
	for k, v := range overrides {
		data[k] = v
	}
	return data
}

func TestBypassPolicyIsTypedFrozenAndSafeImpliesIgnore(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	tmp := t.TempDir()
	if !CreateFields["safe_mode"] || !CreateFields["ignore_user_config"] {
		t.Fatal("create fields miss the bypass flags")
	}
	cfg := map[string]any{"platform_toolsets": map[string]any{"cli": []any{"terminal"}}}
	build := func(params map[string]any) *LocalSessionPolicy {
		t.Helper()
		policy, err := buildPolicy(ctx, params, cfg, buildPolicyOptions{})
		if err != nil {
			t.Fatalf("buildPolicy(%v): %v", params, err)
		}
		return policy
	}
	ordinary := build(map[string]any{"cwd": tmp, "model": "m"})
	if ordinary.SafeMode || ordinary.IgnoreUserConfig {
		t.Fatalf("ordinary %v %v", ordinary.SafeMode, ordinary.IgnoreUserConfig)
	}
	safe := build(map[string]any{"cwd": tmp, "model": "m", "safe_mode": true})
	if !safe.SafeMode || !safe.IgnoreUserConfig {
		t.Fatalf("safe %v %v", safe.SafeMode, safe.IgnoreUserConfig)
	}
	configOnly := build(map[string]any{"cwd": tmp, "model": "m", "ignore_user_config": true})
	if configOnly.SafeMode || !configOnly.IgnoreUserConfig {
		t.Fatalf("config only %v %v", configOnly.SafeMode, configOnly.IgnoreUserConfig)
	}
	request, err := pyjson.Loads(safe.RequestJSON)
	if err != nil {
		t.Fatal(err)
	}
	wantRequest := map[string]any{"cwd": tmp, "model": "m", "safe_mode": true, "source": "cli"}
	if !safe.IgnoreRules() || !reflect.DeepEqual(pyjson.Plain(request), wantRequest) {
		t.Fatalf("ignore rules %v request %v", safe.IgnoreRules(), pyjson.Plain(request))
	}
	restored, err := restorePolicy(safe.asDict())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, safe) || !restored.SafeMode {
		t.Fatalf("restored %+v, want %+v", restored, safe)
	}
	for _, bad := range []map[string]any{{"safe_mode": 1}, {"safe_mode": "true"}, {"ignore_user_config": nil}} {
		params := map[string]any{"cwd": tmp}
		for k, v := range bad {
			params[k] = v
		}
		if _, err := buildPolicy(ctx, params, cfg, buildPolicyOptions{}); runtimeStoreReason(err) != "invalid_params" {
			t.Fatalf("buildPolicy(%v) = %v, want invalid_params", bad, err)
		}
	}
	if _, err := restorePolicy(storedWith(safe, map[string]any{"safe_mode": "yes"})); runtimeStoreReason(err) != "storage_unavailable" {
		t.Fatalf("restore with safe_mode yes = %v", err)
	}
	if _, err := restorePolicy(storedWith(safe, map[string]any{"ignore_user_config": false})); runtimeStoreReason(err) != "storage_unavailable" {
		t.Fatalf("restore without ignore_user_config = %v", err)
	}
}
