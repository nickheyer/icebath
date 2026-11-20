package plugins

import (
	"context"
	"strings"
	"testing"
)

// Caller recording its call and reporting the slot model when a task is routed, the main model otherwise
func slotCaller(captured **pluginLlmCall) pluginLlmCaller {
	return func(_ context.Context, kw pluginLlmCall) (string, string, any, error) {
		call := kw
		*captured = &call
		if kw.Task != nil && *kw.Task != "" {
			return "aux-provider", "aux-model", fakeLlmResponse("ok", 3, 5), nil
		}
		return "main-provider", "main-model", fakeLlmResponse("ok", 3, 5), nil
	}
}

// Context of a home whose registry holds the given task owners and whose builtins are the given keys
func routingCtx(t *testing.T, owners map[string]string, builtins []string, client PluginLLMClient) context.Context {
	t.Helper()
	installLlmServices(t, builtins, client)
	return homeCtx(llmTaskHome(t, owners).home)
}

var routingHi = []any{map[string]any{"role": "user", "content": "hi"}}

// Port of TestCheckTask test_none_returns_none
func TestCheckTaskNoneReturnsNone(t *testing.T) {
	ctx := routingCtx(t, nil, nil, nil)
	if got, err := checkTask(ctx, trustPolicy{pluginID: "my-plugin"}, "my-plugin", nil); got != nil || err != nil {
		t.Fatalf("%v %v", got, err)
	}
}

// Port of TestCheckTask test_auto_and_blank_return_none
func TestCheckTaskAutoAndBlankReturnNone(t *testing.T) {
	ctx := routingCtx(t, map[string]string{"classifier": "my-plugin"}, nil, nil)
	for _, raw := range []string{"auto", "AUTO", "  auto  ", "", "   "} {
		if got, err := checkTask(ctx, trustPolicy{pluginID: "my-plugin"}, "my-plugin", sp(raw)); got != nil || err != nil {
			t.Fatalf("%q gave %v %v", raw, got, err)
		}
	}
}

// Port of TestCheckTask test_own_registered_key_allowed and test_own_key_stripped
func TestCheckTaskOwnRegisteredKeyAllowedAndStripped(t *testing.T) {
	ctx := routingCtx(t, map[string]string{"classifier": "my-plugin"}, []string{"vision"}, nil)
	for _, raw := range []string{"classifier", "  classifier "} {
		if got, err := checkTask(ctx, trustPolicy{pluginID: "my-plugin"}, "my-plugin", sp(raw)); err != nil || got == nil || *got != "classifier" {
			t.Fatalf("%q gave %v %v", raw, got, err)
		}
	}
}

// Port of TestCheckTask test_foreign_key_rejected_and_named
func TestCheckTaskForeignKeyRejectedAndNamed(t *testing.T) {
	ctx := routingCtx(t, map[string]string{"classifier": "other-plugin"}, []string{"vision"}, nil)
	logs := captureLogs(t)
	_, err := checkTask(ctx, trustPolicy{pluginID: "my-plugin"}, "my-plugin", sp("classifier"))
	if !isTrustErr(err, "my-plugin") || !isTrustErr(err, "classifier") {
		t.Fatalf("error %v", err)
	}
	found := false
	for _, line := range strings.Split(logs.String(), "\n") {
		found = found || (strings.Contains(line, "my-plugin") && strings.Contains(line, "classifier"))
	}
	if !found {
		t.Fatalf("no warning names the plugin and key: %s", logs)
	}
}

// Port of TestCheckTask test_unknown_key_rejected
func TestCheckTaskUnknownKeyRejected(t *testing.T) {
	ctx := routingCtx(t, nil, []string{"vision"}, nil)
	if _, err := checkTask(ctx, trustPolicy{pluginID: "my-plugin"}, "my-plugin", sp("nope")); !isTrustErr(err, "") {
		t.Fatalf("error %v", err)
	}
}

// Port of TestCheckTask test_builtin_key_denied_without_flag and test_builtin_key_allowed_with_flag
func TestCheckTaskBuiltinKeyNeedsTheOverrideFlag(t *testing.T) {
	ctx := routingCtx(t, nil, []string{"vision", "compression"}, nil)
	logs := captureLogs(t)
	if _, err := checkTask(ctx, trustPolicy{pluginID: "my-plugin"}, "my-plugin", sp("vision")); !isTrustErr(err, "allow_task_override") || !strings.Contains(logs.String(), "vision") {
		t.Fatalf("denied %v %s", err, logs)
	}
	if got, err := checkTask(ctx, trustPolicy{pluginID: "my-plugin", allowTaskOverride: true}, "my-plugin", sp("vision")); err != nil || *got != "vision" {
		t.Fatalf("allowed %v %v", got, err)
	}
}

// Port of TestCheckTask test_own_key_wins_over_builtin_flag_requirement
func TestCheckTaskOwnKeyWinsOverBuiltinFlagRequirement(t *testing.T) {
	ctx := routingCtx(t, map[string]string{"shared": "my-plugin"}, nil, nil)
	installLlmServices(t, []string{"shared"}, nil)
	if got, err := checkTask(ctx, trustPolicy{pluginID: "my-plugin"}, "my-plugin", sp("shared")); err != nil || got == nil || *got != "shared" {
		t.Fatalf("%v %v", got, err)
	}
}

// Port of TestRouting test_default_call_passes_task_none
func TestRoutingDefaultCallPassesTaskNone(t *testing.T) {
	ctx := routingCtx(t, nil, nil, nil)
	var captured *pluginLlmCall
	r, err := makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, slotCaller(&captured), nil).Complete(ctx, routingHi, PluginLlmOptions{})
	if err != nil || captured == nil || captured.Task != nil || r.Provider != "main-provider" || r.Model != "main-model" || r.Audit["task"] != "" {
		t.Fatalf("%+v %v", r, err)
	}
}

// Port of TestRouting test_registered_task_routes_and_reports_slot_model
func TestRoutingRegisteredTaskRoutesAndReportsSlotModel(t *testing.T) {
	ctx := routingCtx(t, map[string]string{"classifier": "my-plugin"}, []string{"vision"}, nil)
	var captured *pluginLlmCall
	r, err := makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, slotCaller(&captured), nil).Complete(ctx, routingHi, PluginLlmOptions{Task: sp("classifier")})
	if err != nil || *captured.Task != "classifier" || r.Provider != "aux-provider" || r.Model != "aux-model" || r.Audit["task"] != "classifier" {
		t.Fatalf("%+v %v", r, err)
	}
}

// Port of TestRouting test_foreign_task_raises_before_invoking_caller and test_unknown_task_raises_before_invoking_caller
func TestRoutingForeignAndUnknownTasksRaiseBeforeInvokingCaller(t *testing.T) {
	for _, tc := range []struct {
		owners map[string]string
		task   string
	}{{map[string]string{"classifier": "other-plugin"}, "classifier"}, {nil, "unknown"}} {
		ctx := routingCtx(t, tc.owners, nil, nil)
		var captured *pluginLlmCall
		_, err := makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, slotCaller(&captured), nil).Complete(ctx, routingHi, PluginLlmOptions{Task: sp(tc.task)})
		if !isTrustErr(err, "") || captured != nil {
			t.Fatalf("%s: %v caller reached %v", tc.task, err, captured)
		}
	}
}

// Port of TestRouting test_structured_routes_task
func TestRoutingStructuredRoutesTask(t *testing.T) {
	ctx := routingCtx(t, map[string]string{"classifier": "my-plugin"}, nil, nil)
	var captured *pluginLlmCall
	r, err := makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, slotCaller(&captured), nil).CompleteStructured(ctx,
		PluginLlmStructuredRequest{Instructions: "classify this", Input: []any{PluginLlmTextInput{Text: "payload"}}}, PluginLlmOptions{Task: sp("classifier")})
	if err != nil || *captured.Task != "classifier" || r.Audit["task"] != "classifier" {
		t.Fatalf("%+v %v", r, err)
	}
}

// Port of TestRouting test_async_routes_task and test_async_structured_routes_task
func TestRoutingAsyncRoutesTask(t *testing.T) {
	ctx := routingCtx(t, map[string]string{"classifier": "my-plugin"}, nil, nil)
	var captured *pluginLlmCall
	llm := makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, nil, slotCaller(&captured))
	r, err := llm.AComplete(ctx, routingHi, PluginLlmOptions{Task: sp("classifier")})
	if err != nil || *captured.Task != "classifier" || r.Audit["task"] != "classifier" {
		t.Fatalf("async %+v %v", r, err)
	}
	captured = nil
	s, err := llm.ACompleteStructured(ctx, PluginLlmStructuredRequest{Instructions: "classify this", Input: []any{PluginLlmTextInput{Text: "payload"}}}, PluginLlmOptions{Task: sp("classifier")})
	if err != nil || *captured.Task != "classifier" || s.Audit["task"] != "classifier" {
		t.Fatalf("async structured %+v %v", s, err)
	}
}

// Port of TestForwardsToCallLlm, the production path through the auxiliary client service
func TestForwardsToCallLlm(t *testing.T) {
	client := &fakeAuxClient{route: map[string]string{"provider": "aux-provider", "model": "aux-model"}, response: fakeLlmResponse("ok", 3, 5)}
	ctx := routingCtx(t, map[string]string{"classifier": "my-plugin"}, nil, client)
	llm := makePluginLlmForTest("my-plugin", trustPolicy{pluginID: "my-plugin"}, nil, nil)
	r, err := llm.Complete(ctx, routingHi, PluginLlmOptions{Task: sp("classifier")})
	if err != nil || *client.seen[0].Task != "classifier" || client.async[0] || r.Provider != "aux-provider" || r.Model != "aux-model" {
		t.Fatalf("sync task %+v %v", r, err)
	}
	if _, err := llm.Complete(ctx, routingHi, PluginLlmOptions{}); err != nil || client.seen[1].Task != nil {
		t.Fatalf("sync default %v", err)
	}
	r, err = llm.AComplete(ctx, routingHi, PluginLlmOptions{Task: sp("classifier")})
	if err != nil || *client.seen[2].Task != "classifier" || !client.async[2] || r.Provider != "aux-provider" || r.Model != "aux-model" {
		t.Fatalf("async task %+v %v", r, err)
	}
}
