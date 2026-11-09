package config

import (
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Clears the process worker policy and runtime marks for one test and again afterwards
func freshWorkerPolicy(t *testing.T) {
	t.Helper()
	reset := ResetWorkerPolicyForTests
	reset()
	t.Cleanup(reset)
}

func TestPrivateWorkerBindingIsOneShotTypedAndNotEnvSelected(t *testing.T) {
	freshWorkerPolicy(t)
	t.Setenv("ICEBATH_SAFE_MODE", "1")
	if SafeWorkerEnabled() || WorkerConfigSnapshot() != nil || CurrentSafeWorkerPolicy() != nil {
		t.Fatal("env selected a policy")
	}
	if _, err := BindSafeWorkerPolicy(false, false, map[string]any{}); err == nil || !strings.Contains(err.Error(), "ordinary execution binds its config") {
		t.Fatalf("ordinary bypass %v", err)
	}
	if _, err := BindSafeWorkerPolicy(true, true, nil); err == nil || err.Error() != "worker config must be a JSON object" {
		t.Fatalf("nil config %v", err)
	}
	if _, err := BindSafeWorkerPolicy(true, true, map[string]any{"x": math.NaN()}); err == nil {
		t.Fatal("NaN accepted")
	}
	policy, err := BindSafeWorkerPolicy(true, false, map[string]any{})
	if err != nil || !policy.IgnoreUserConfig() || !policy.SafeMode() || !SafeWorkerEnabled() || policy.PID() != os.Getpid() {
		t.Fatalf("bound %+v %v", policy, err)
	}
	if _, err := BindSafeWorkerPolicy(false, true, map[string]any{}); err == nil || err.Error() != "worker policy is already bound" {
		t.Fatalf("rebound %v", err)
	}
	if _, err := BindWorkerConfig(map[string]any{}); err == nil {
		t.Fatal("ordinary rebind accepted")
	}
	if snap := WorkerConfigSnapshot(); snap == nil || len(snap) != 0 {
		t.Fatalf("snapshot %v", snap)
	}
}

func TestOrdinaryWorkerConfigIsFrozenAndDetached(t *testing.T) {
	freshWorkerPolicy(t)
	cfg := map[string]any{"agent": map[string]any{"max_turns": 3, "ratio": 0.5}, "tags": []any{"a"}}
	policy, err := BindWorkerConfig(cfg)
	if err != nil || policy.SafeMode() || policy.IgnoreUserConfig() || SafeWorkerEnabled() {
		t.Fatalf("ordinary %+v %v", policy, err)
	}
	cfg["agent"].(map[string]any)["max_turns"] = 99
	first := WorkerConfigSnapshot()
	first["tags"] = nil
	second := WorkerConfigSnapshot()
	want := map[string]any{"agent": map[string]any{"max_turns": 3, "ratio": 0.5}, "tags": []any{"a"}}
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("snapshot %#v", second)
	}
}

func TestPrivatePolicyRefusesLateBindingAndForkInheritance(t *testing.T) {
	freshWorkerPolicy(t)
	NoteRuntimeImport("unrelated.module")
	Load(t.TempDir(), nil)
	if _, err := BindSafeWorkerPolicy(true, true, map[string]any{}); err == nil || !strings.Contains(err.Error(), "before runtime imports") {
		t.Fatalf("late binding %v", err)
	}
	freshWorkerPolicy(t)
	workerPolicy.Store(&SafeWorkerPolicy{pid: os.Getpid() + 1, safeMode: true, ignoreUserConfig: true, configJSON: "{}"})
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "fresh exec") {
			t.Fatalf("fork inheritance %v", r)
		}
	}()
	SafeWorkerEnabled()
}
