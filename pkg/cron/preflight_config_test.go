package cron

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

type runtimeResolve = func(ctx context.Context, h, requested, targetModel, baseURL, apiKey string) (*provider.Runtime, error)

// Openrouter runtime the standard cron cases resolve to
func preflightRuntime(providerName string) *provider.Runtime {
	return &provider.Runtime{APIKey: "test-key", BaseURL: "https://example.invalid/v1", Provider: providerName, APIMode: "chat_completions"}
}

// Missing key refusal of the openrouter provider
func missingKeyResolve(context.Context, string, string, string, string, string) (*provider.Runtime, error) {
	return nil, &provider.AuthError{Message: "No API key configured for provider 'openrouter'"}
}

// Preflight job record with the given overrides
func preflightJobRecord(overrides map[string]any) map[string]any {
	rec := map[string]any{
		"id": "pf-test", "name": "preflight test", "prompt": "hello", "enabled": true, "state": "scheduled",
		"schedule": map[string]any{"kind": "interval", "minutes": 5, "display": "every 5m"},
		"deliver":  "local", "model": "test-model", "provider": nil, "base_url": nil,
	}
	maps.Copy(rec, overrides)
	return rec
}

// Job decoded from a raw record the way a Python dict reaches run_job
func jobFromRecord(t *testing.T, rec map[string]any) *Job {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{}
	if err := json.Unmarshal(raw, job); err != nil {
		t.Fatal(err)
	}
	return job
}

// Resolver used by both the run and its preflight for one test
func patchRuntimeResolve(t *testing.T, resolve runtimeResolve) {
	t.Helper()
	prevRun, prevPreflight := resolveRuntimeProviderSeam, preflightResolveRuntime
	resolveRuntimeProviderSeam, preflightResolveRuntime = resolve, resolve
	t.Cleanup(func() { resolveRuntimeProviderSeam, preflightResolveRuntime = prevRun, prevPreflight })
}

// Agent factory answering ok, reporting whether any agent was constructed
func installOKAgent(t *testing.T) *bool {
	t.Helper()
	constructed := false
	installFakeAgent(t, func(context.Context, AgentOptions) (CronAgent, error) {
		constructed = true
		return &fakeCronAgent{result: map[string]any{"final_response": "ok"}}, nil
	})
	return &constructed
}

// Outcome of one run_job with the standard cron seams patched
type patchedRun struct {
	out         jobRunOutcome
	constructed bool
}

func runJobPatched(t *testing.T, s *Store, job *Job, resolve runtimeResolve, skillView func(context.Context, string, config.Tree, string) string) patchedRun {
	t.Helper()
	if resolve == nil {
		resolve = func(context.Context, string, string, string, string, string) (*provider.Runtime, error) {
			return preflightRuntime("openrouter"), nil
		}
	}
	patchRuntimeResolve(t, resolve)
	if skillView != nil {
		prev := preflightSkillView
		preflightSkillView = skillView
		t.Cleanup(func() { preflightSkillView = prev })
	}
	constructed := installOKAgent(t)
	out, err := s.runJob(context.Background(), job, runJobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return patchedRun{out: out, constructed: *constructed}
}

// Store with the preflight job saved under the owner fixture
func preflightStore(t *testing.T, rec map[string]any) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	t.Setenv("ICEBATH_MODEL", "test-cron-default-model")
	resetDeliver(t)
	installCronOwner(t)
	seedJobs(t, s, rec)
	return s
}

func TestMissingProviderKeyBlocksMissingKeyBlockedConfigNoAgent(t *testing.T) {
	rec := preflightJobRecord(nil)
	s := preflightStore(t, rec)
	run := runJobPatched(t, s, jobFromRecord(t, rec), missingKeyResolve, nil)
	if run.constructed || run.out.Success || run.out.Error == nil || !strings.Contains(*run.out.Error, "[blocked_config]") {
		t.Fatalf("run = %+v", run)
	}
	if !strings.Contains(strings.ToLower(run.out.Output), "blocked") && !strings.Contains(run.out.Output, "BLOCKED") {
		t.Fatalf("output = %q", run.out.Output)
	}
}

// Runs two run_one_job ticks of the stored job with the missing key resolver, the agent never built
func twoFailingTicks(t *testing.T, s *Store, id string) {
	t.Helper()
	patchRuntimeResolve(t, missingKeyResolve)
	constructed := installOKAgent(t)
	for range 2 {
		ok, err := runOneJob(context.Background(), s, mustGet(t, s, id), runOneJobOptions{})
		if err != nil || !ok {
			t.Fatalf("tick = %v, %v", ok, err)
		}
		if *constructed {
			t.Fatal("agent constructed")
		}
	}
}

func TestMissingProviderKeyBlocksSingleAlertAcrossTwoTicksAndBlockedStatus(t *testing.T) {
	rec := preflightJobRecord(map[string]any{"failure_deliver": "slack:alerts"})
	s := preflightStore(t, rec)
	registerHomePlatforms(t, "slack")
	twoFailingTicks(t, s, "pf-test")
	if got := deref(mustGet(t, s, "pf-test").LastStatus); got != "blocked_config" {
		t.Fatalf("last_status = %s", got)
	}
	if n := queuedDeliveries(t, s); n != 1 {
		t.Fatalf("expected exactly one alert across two ticks, got %d", n)
	}
}

const fallbackChainYAML = "fallback_providers:\n  - provider: openrouter\n    model: z-ai/glm-5.2\n"

func TestMissingProviderKeyBlocksFallbackChainRescuesMissingPrimaryKey(t *testing.T) {
	rec := preflightJobRecord(map[string]any{"model": nil})
	s := preflightStore(t, rec)
	writePinConfig(t, s, fallbackChainYAML)
	resolve := func(_ context.Context, _, requested, _, _, _ string) (*provider.Runtime, error) {
		if requested == "" {
			return nil, &provider.AuthError{Message: "no key"}
		}
		return preflightRuntime("openrouter"), nil
	}
	run := runJobPatched(t, s, jobFromRecord(t, rec), resolve, nil)
	if !run.constructed || !run.out.Success || run.out.Error != nil {
		t.Fatalf("run = %+v", run)
	}
}

func TestMissingProviderKeyBlocksGlobalChainDoesNotRescueAPinnedJob(t *testing.T) {
	rec := preflightJobRecord(map[string]any{"provider": "anthropic", "model": "claude-sonnet-5"})
	s := preflightStore(t, rec)
	writePinConfig(t, s, fallbackChainYAML)
	var calls []string
	resolve := func(_ context.Context, _, requested, _, _, _ string) (*provider.Runtime, error) {
		calls = append(calls, requested)
		if requested == "anthropic" {
			return nil, &provider.AuthError{Message: "no key"}
		}
		return preflightRuntime(requested), nil
	}
	run := runJobPatched(t, s, jobFromRecord(t, rec), resolve, nil)
	if run.constructed || run.out.Success || !strings.Contains(deref(run.out.Error), "provider credential missing") {
		t.Fatalf("run = %+v", run)
	}
	for _, c := range calls {
		if c == "openrouter" {
			t.Fatalf("the global chain was walked: %v", calls)
		}
	}
}

func TestHealthyJobUnaffectedHealthyJobRunsNormally(t *testing.T) {
	rec := preflightJobRecord(nil)
	s := preflightStore(t, rec)
	run := runJobPatched(t, s, jobFromRecord(t, rec), nil, nil)
	if !run.out.Success || run.out.Error != nil || run.out.FinalResponse != "ok" || !run.constructed {
		t.Fatalf("run = %+v", run)
	}
}

func TestHealthyJobUnaffectedRecoveryClearsAlertMarker(t *testing.T) {
	rec := preflightJobRecord(nil)
	s := preflightStore(t, rec)
	runJobPatched(t, s, jobFromRecord(t, rec), missingKeyResolve, nil)
	if !mustGet(t, s, "pf-test").PreflightAlerted {
		t.Fatal("blocked tick left no alert marker")
	}
	run := runJobPatched(t, s, mustGet(t, s, "pf-test"), nil, nil)
	if !run.out.Success || !run.constructed {
		t.Fatalf("run = %+v", run)
	}
	if mustGet(t, s, "pf-test").PreflightAlerted {
		t.Fatal("healthy tick kept the alert marker")
	}
}

func TestOptOutPreflightFalseRestoresOldBehavior(t *testing.T) {
	rec := preflightJobRecord(map[string]any{"failure_deliver": "slack:alerts"})
	s := preflightStore(t, rec)
	registerHomePlatforms(t, "slack")
	writePinConfig(t, s, "cron:\n  preflight: false\n")
	twoFailingTicks(t, s, "pf-test")
	if got := deref(mustGet(t, s, "pf-test").LastStatus); got != "error" {
		t.Fatalf("last_status = %s", got)
	}
	if n := queuedDeliveries(t, s); n != 2 {
		t.Fatalf("old behavior alerts every tick, got %d", n)
	}
}

// Skill view answering one fixed payload for every name
func fixedSkillView(payload map[string]any) func(context.Context, string, config.Tree, string) string {
	raw, _ := json.Marshal(payload)
	return func(context.Context, string, config.Tree, string) string { return string(raw) }
}

func TestSkillReadinessUnreadySkillBlocks(t *testing.T) {
	rec := preflightJobRecord(map[string]any{"skills": []any{"needy-skill"}})
	s := preflightStore(t, rec)
	view := fixedSkillView(map[string]any{"success": true, "content": "# needy skill\nbody", "readiness_status": "setup_needed",
		"setup_needed": true, "missing_required_environment_variables": []any{"NEEDY_API_KEY"}, "missing_required_commands": []any{}})
	run := runJobPatched(t, s, jobFromRecord(t, rec), nil, view)
	if run.constructed || run.out.Success || !strings.Contains(deref(run.out.Error), "[blocked_config]") {
		t.Fatalf("run = %+v", run)
	}
	if !strings.Contains(deref(run.out.Error)+" "+run.out.Output, "NEEDY_API_KEY") {
		t.Fatalf("run = %+v", run)
	}
}

func TestSkillReadinessReadySkillRuns(t *testing.T) {
	rec := preflightJobRecord(map[string]any{"skills": []any{"ready-skill"}})
	s := preflightStore(t, rec)
	view := fixedSkillView(map[string]any{"success": true, "content": "# ready skill\nbody", "readiness_status": "available",
		"setup_needed": false, "missing_required_environment_variables": []any{}})
	run := runJobPatched(t, s, jobFromRecord(t, rec), nil, view)
	if !run.out.Success || !run.constructed {
		t.Fatalf("run = %+v", run)
	}
}

func TestDeliveryPlatformUnknownDeliveryPlatformBlocks(t *testing.T) {
	rec := preflightJobRecord(map[string]any{"deliver": "notaplatform"})
	s := preflightStore(t, rec)
	patchKnownDeliveryPlatform(t, func(context.Context, string) bool { return false })
	run := runJobPatched(t, s, jobFromRecord(t, rec), nil, nil)
	if run.constructed || run.out.Success || !strings.Contains(deref(run.out.Error), "[blocked_config]") {
		t.Fatalf("run = %+v", run)
	}
	if !strings.Contains(deref(run.out.Error)+" "+run.out.Output, "notaplatform") {
		t.Fatalf("run = %+v", run)
	}
}

func TestDeliveryPlatformLocalDeliveryNeverTouchesGatewayConfig(t *testing.T) {
	rec := preflightJobRecord(map[string]any{"deliver": "local"})
	s := preflightStore(t, rec)
	prev := loadGatewayConfig
	loadGatewayConfig = func(context.Context) (*gateway.GatewayConfig, error) { return nil, errors.New("gateway config loaded") }
	t.Cleanup(func() { loadGatewayConfig = prev })
	run := runJobPatched(t, s, jobFromRecord(t, rec), nil, nil)
	if !run.out.Success || !run.constructed {
		t.Fatalf("run = %+v", run)
	}
}
