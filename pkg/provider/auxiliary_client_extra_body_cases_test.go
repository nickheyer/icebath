package provider

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Installs a fixed profile projection for one test
func auxFixedProjection(t *testing.T, p auxProfileProjection) {
	t.Helper()
	prev := auxProjectProviderProfileFn
	auxProjectProviderProfileFn = func(context.Context, string, string, string, string, map[string]any, map[string]bool) auxProfileProjection {
		return p
	}
	t.Cleanup(func() { auxProjectProviderProfileFn = prev })
}

// Builds call kwargs for one user hello message and fails the test on error
func auxExtraBodyKwargs(t *testing.T, ctx context.Context, provider, model string, opts auxCallKwargsOptions) (map[string]any, map[string]any) {
	t.Helper()
	opts.Timeout = 30
	kw, err := auxBuildCallKwargs(ctx, provider, model, []any{map[string]any{"role": "user", "content": "hello"}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := kw["extra_body"].(map[string]any)
	return kw, body
}

func TestAuxiliaryTaskExtraBodyTaskReasoningDisableUsesDeepseekThinkingWire(t *testing.T) {
	ctx := auxMainTestHome(t, "auxiliary:\n  compression:\n    reasoning_effort: none\n")
	taskBody, err := auxGetTaskExtraBody(ctx, "compression")
	if err != nil {
		t.Fatal(err)
	}
	_, body := auxExtraBodyKwargs(t, ctx, "deepseek", "deepseek-v4-flash", auxCallKwargsOptions{ExtraBody: taskBody, Task: "compression"})
	if !reflect.DeepEqual(body["thinking"], map[string]any{"type": "disabled"}) {
		t.Fatalf("thinking wire %v", body)
	}
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("reasoning shipped beside the native disable %v", body)
	}
}

func TestAuxiliaryTaskExtraBodyExplicitDeepseekThinkingDisableBeatsProfileDefault(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	_, body := auxExtraBodyKwargs(t, ctx, "deepseek", "deepseek-v4-flash", auxCallKwargsOptions{ExtraBody: map[string]any{"thinking": map[string]any{"type": "disabled"}}, Task: "compression"})
	if !reflect.DeepEqual(body["thinking"], map[string]any{"type": "disabled"}) {
		t.Fatalf("explicit vendor control lost %v", body)
	}
}

func TestAuxiliaryTaskExtraBodyDisabledCallerReasoningSuppressesTaskReasoningForProfileWire(t *testing.T) {
	ctx := auxMainTestHome(t, "auxiliary:\n  title_generation:\n    reasoning_effort: low\n    extra_body:\n      metadata:\n        task: title\n")
	auxFixedProjection(t, auxProfileProjection{Body: map[string]any{}, ReasoningExtra: map[string]any{}, TopLevel: map[string]any{"reasoning_effort": "none"}, HandlesReasoning: true})
	taskBody, err := auxGetTaskExtraBody(ctx, "title_generation")
	if err != nil {
		t.Fatal(err)
	}
	kw, body := auxExtraBodyKwargs(t, ctx, "custom", "test-model", auxCallKwargsOptions{ExtraBody: taskBody, ReasoningConfig: map[string]any{"enabled": false}, Task: "title_generation"})
	if kw["reasoning_effort"] != "none" {
		t.Fatalf("profile owned disable missing %v", kw)
	}
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("task reasoning shipped with a disabled caller %v", body)
	}
	if !reflect.DeepEqual(body["metadata"], map[string]any{"task": "title"}) {
		t.Fatalf("task extra body lost %v", body)
	}
}

func TestAuxiliaryTaskExtraBodyDisabledCallerReasoningKeepsProfileOwnedDisableShape(t *testing.T) {
	ctx := auxMainTestHome(t, "auxiliary:\n  title_generation:\n    reasoning_effort: low\n")
	auxFixedProjection(t, auxProfileProjection{Body: map[string]any{}, ReasoningExtra: map[string]any{"reasoning": map[string]any{"enabled": false}}, TopLevel: map[string]any{}, HandlesReasoning: true})
	taskBody, err := auxGetTaskExtraBody(ctx, "title_generation")
	if err != nil {
		t.Fatal(err)
	}
	kw, body := auxExtraBodyKwargs(t, ctx, "openrouter", "test-model", auxCallKwargsOptions{ExtraBody: taskBody, ReasoningConfig: map[string]any{"enabled": false}, Task: "title_generation"})
	if !reflect.DeepEqual(body["reasoning"], map[string]any{"enabled": false}) {
		t.Fatalf("profile disable shape lost %v", body)
	}
	if _, ok := kw["reasoning_effort"]; ok {
		t.Fatalf("top level effort leaked %v", kw)
	}
}

func TestAuxiliaryTaskExtraBodyGenericReasoningFallbackClampsUltraForAuxiliaryAndMoaCalls(t *testing.T) {
	for _, task := range []string{"session_search", "moa_reference", "moa_aggregator"} {
		ctx := auxMainTestHome(t, "")
		auxFixedProjection(t, auxProfileProjection{Body: map[string]any{}, ReasoningExtra: map[string]any{}, TopLevel: map[string]any{}})
		_, body := auxExtraBodyKwargs(t, ctx, "custom", "test-model", auxCallKwargsOptions{ReasoningConfig: map[string]any{"enabled": true, "effort": "ultra"}, Task: task})
		if !reflect.DeepEqual(body["reasoning"], map[string]any{"enabled": true, "effort": "max"}) {
			t.Fatalf("%s: ultra reached the wire %v", task, body)
		}
	}
}

func TestAuxiliaryTaskExtraBodyTaskExtraBodyReasoningEffortUltraIsClamped(t *testing.T) {
	ctx := auxMainTestHome(t, "auxiliary:\n  session_search:\n    reasoning_effort: ultra\n")
	auxFixedProjection(t, auxProfileProjection{Body: map[string]any{}, ReasoningExtra: map[string]any{}, TopLevel: map[string]any{}})
	taskBody, err := auxGetTaskExtraBody(ctx, "session_search")
	if err != nil {
		t.Fatal(err)
	}
	_, body := auxExtraBodyKwargs(t, ctx, "custom", "test-model", auxCallKwargsOptions{ExtraBody: taskBody, Task: "session_search"})
	if !reflect.DeepEqual(body["reasoning"], map[string]any{"enabled": true, "effort": "max"}) {
		t.Fatalf("folded ultra not clamped %v", body)
	}
}

func TestAuxiliaryTaskExtraBodyProfileProjectionReceivesWireClampedEffort(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	var seen map[string]any
	real := auxProjectProviderProfileFn
	auxProjectProviderProfileFn = func(ctx context.Context, provider, providerNorm, model, effectiveBase string, reasoningConfig map[string]any, dataUse map[string]bool) auxProfileProjection {
		seen = reasoningConfig
		return real(ctx, provider, providerNorm, model, effectiveBase, reasoningConfig, dataUse)
	}
	t.Cleanup(func() { auxProjectProviderProfileFn = real })
	kw, body := auxExtraBodyKwargs(t, ctx, "openrouter", "deepseek/deepseek-v4.1-flash", auxCallKwargsOptions{ReasoningConfig: map[string]any{"enabled": true, "effort": "ultra"}, Task: "moa_aggregator"})
	if !reflect.DeepEqual(seen, map[string]any{"enabled": true, "effort": "max"}) {
		t.Fatalf("projection saw %v", seen)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "ultra") || kw["reasoning_effort"] == "ultra" {
		t.Fatalf("ultra reached the wire %v %v", body, kw["reasoning_effort"])
	}
}

func TestAuxiliaryTaskExtraBodyMoaTasksRejectTaskLevelReasoningEffort(t *testing.T) {
	for _, task := range []string{"moa_reference", "moa_aggregator"} {
		ctx := auxMainTestHome(t, "auxiliary:\n  "+task+":\n    reasoning_effort: xhigh\n")
		logs := auxCaptureLogs(t)
		body, err := auxGetTaskExtraBody(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := body["reasoning"]; ok {
			t.Fatalf("%s: task level effort accepted %v", task, body)
		}
		warned := false
		for _, r := range logs.matching("") {
			if r.level >= slog.LevelWarn {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("%s: no warning about the ignored shorthand", task)
		}
	}
}

func TestAuxiliaryTaskExtraBodyAnthropicAuxClientForwardsExtraBodyReasoning(t *testing.T) {
	var seen *anthReasoningConfig
	prev := auxAnthBuildKwargsFn
	auxAnthBuildKwargsFn = func(in anthBuildInput) (map[string]any, error) {
		seen = in.reasoning
		return prev(in)
	}
	t.Cleanup(func() { auxAnthBuildKwargsFn = prev })
	auxCaptureAnthropicKwargs(t, map[string]any{"reasoning": map[string]any{"enabled": true, "effort": "low"}}, false, nil)
	if seen == nil || seen.enabled == nil || !*seen.enabled || seen.effort != "low" {
		t.Fatalf("builder reasoning %+v", seen)
	}
}

func TestAuxiliaryTaskExtraBodyAnthropicAuxExtraBodyPassthrough(t *testing.T) {
	extra := map[string]any{"thinking": map[string]any{"type": "disabled"}, "metadata": map[string]any{"user_id": "u1"}}
	api := auxCaptureAnthropicKwargs(t, extra, false, nil)
	if !reflect.DeepEqual(api["extra_body"], extra) {
		t.Fatalf("extra_body %v", api["extra_body"])
	}
}

func TestAuxiliaryTaskExtraBodyBareCustomAuthErrorDoesNotFallBackToEnvBaseUrl(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENAI_BASE_URL", "https://old-proxy.example/v1")
	t.Setenv("OPENAI_API_KEY", "")
	os.Unsetenv("OPENAI_API_KEY")
	prev := auxResolveRuntimeProviderFn
	auxResolveRuntimeProviderFn = func(context.Context, string) (*Runtime, error) {
		return nil, &AuthError{Message: "no creds", Provider: "custom", Code: "missing_api_key"}
	}
	t.Cleanup(func() { auxResolveRuntimeProviderFn = prev })
	base, key, mode, err := auxResolveCustomRuntime(ctx)
	if err != nil || base != "" || key != "" || mode != "" {
		t.Fatalf("resolved %q %q %q %v", base, key, mode, err)
	}
}
