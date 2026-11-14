package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func failedSurfaceResult(reason, errText string, extra map[string]any) map[string]any {
	r := map[string]any{"completed": false, "failed": true, "error": errText}
	if reason != "" {
		r["failure_reason"] = reason
	}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func TestErrorSurfaceNilWithoutFailureSignal(t *testing.T) {
	for _, r := range []any{"boom", nil, map[string]any{"completed": true, "final_response": "hi"}} {
		if s := buildErrorSurfaceFromResult(r, "", ""); s != nil {
			t.Fatalf("%v -> %v", r, s)
		}
	}
}

func TestErrorSurfaceResultLayers(t *testing.T) {
	cases := []struct {
		reason, errText, provider string
		extra                     map[string]any
		want                      map[string]any
	}{
		{"auth", "provider exploded", "", nil, map[string]any{"layer": LayerAuth, "code": "auth", "retryable": false}},
		{"billing", "provider exploded", "", nil, map[string]any{"layer": LayerBilling, "code": "billing", "retryable": false}},
		{"", "something odd", "", nil, map[string]any{"layer": LayerProvider, "code": "unknown", "retryable": true}},
		{"", "The provider's stream connection keeps dropping", "", nil, map[string]any{"layer": LayerStreaming, "code": "stream_drop", "retryable": true}},
		{"server_error", "OSError: [Errno 28] No space left on device", "", nil, map[string]any{"layer": LayerDisk, "code": "disk_full", "retryable": false}},
		{"rate_limit", "provider exploded", "", map[string]any{"billing_block": map[string]any{"provider": "nous"}}, map[string]any{"layer": LayerBilling, "code": "rate_limit", "retryable": false}},
		{"unknown", "provider exploded", "", map[string]any{"failure_retryable": false}, map[string]any{"layer": LayerProvider, "code": "unknown", "retryable": false}},
		{"format_error", "provider exploded", "", map[string]any{"failure_retryable": true}, map[string]any{"layer": LayerProvider, "code": "format_error", "retryable": true}},
	}
	for _, c := range cases {
		got := buildErrorSurfaceFromResult(failedSurfaceResult(c.reason, c.errText, c.extra), c.provider, "")
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%q %q: %v", c.reason, c.errText, got)
		}
	}
	for _, reason := range []string{"rate_limit", "server_error", "overloaded", "unknown", "format_error"} {
		if s := buildErrorSurfaceFromResult(failedSurfaceResult(reason, "provider exploded", nil), "", ""); s["layer"] != LayerProvider || s["code"] != reason {
			t.Fatalf("%s -> %v", reason, s)
		}
	}
	for _, reason := range []string{"auth", "format_error", "content_policy_blocked", "model_not_found", "ssl_cert_verification"} {
		if s := buildErrorSurfaceFromResult(failedSurfaceResult(reason, "provider exploded", nil), "", ""); s["retryable"] != false {
			t.Fatalf("%s retryable", reason)
		}
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("auth_permanent", "x", nil), "", ""); s["layer"] != LayerAuth || s["retryable"] != false {
		t.Fatalf("auth_permanent %v", s)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("timeout", "x", nil), "custom", ""); s["layer"] != LayerEndpoint {
		t.Fatalf("custom timeout %v", s)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("timeout", "x", nil), "anthropic", ""); s["layer"] != LayerProvider {
		t.Fatalf("anthropic timeout %v", s)
	}
}

func TestErrorSurfaceIdentityLabelsAndAuthKind(t *testing.T) {
	oauth := buildErrorSurfaceFromResult(failedSurfaceResult("auth", "x", nil), "nous", "")
	if oauth["auth_kind"] != "oauth" || oauth["provider_label"] != "Nous Portal" {
		t.Fatalf("nous %v", oauth)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("timeout", "x", nil), "opencode-go", ""); s["provider_label"] != "OpenCode Go" {
		t.Fatalf("label %v", s)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("timeout", "x", nil), "", ""); s["provider_label"] != nil {
		t.Fatalf("label without provider %v", s)
	}
	s := buildErrorSurfaceFromResult(failedSurfaceResult("rate_limit", "x", nil), "openrouter", "test/m1")
	if s["provider"] != "openrouter" || s["model"] != "test/m1" {
		t.Fatalf("identity %v", s)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("rate_limit", "x", nil), "", ""); s["provider"] != nil || s["model"] != nil {
		t.Fatalf("identity without provider %v", s)
	}
}

func TestErrorSurfaceFreeTierBlocks(t *testing.T) {
	s := buildErrorSurfaceFromResult(failedSurfaceResult("auth_permanent", "HTTP 403: no permissions", map[string]any{
		"free_tier": map[string]any{"kind": "disabled", "message": "Using Hermes without signing in is switched off."}}), "nous", "nous/welcome")
	if s["layer"] != LayerProvider || s["code"] != "free_tier_disabled" || s["retryable"] != false || s["auth_kind"] != nil ||
		s["message"] != "Using Hermes without signing in is switched off." {
		t.Fatalf("free tier %v", s)
	}
	kinds := map[string]bool{"rate_limited": true, "at_capacity": true, "outage": true, "disabled": false, "model_not_free": false, "route": false, "refused": false}
	for kind, retryable := range kinds {
		s := buildErrorSurfaceFromResult(failedSurfaceResult("rate_limit", "x", map[string]any{"free_tier": map[string]any{"kind": kind}}), "nous", "")
		if s["code"] != "free_tier_"+kind || s["retryable"] != retryable || s["message"] != nil {
			t.Fatalf("%s %v", kind, s)
		}
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("auth_permanent", "x", map[string]any{"free_tier": map[string]any{}}), "nous", ""); s["code"] != "auth_permanent" || s["layer"] != LayerAuth {
		t.Fatalf("kindless free tier %v", s)
	}
}

func TestSiteFailureCodesNeverCollapseToUnknown(t *testing.T) {
	for code := range SiteFailureCodes {
		t.Run(code, func(t *testing.T) {
			surface := buildErrorSurfaceFromResult(map[string]any{"failed": true, "error": "x", "failure_reason": code}, "", "")
			if surface["code"] != code {
				t.Fatalf("code %v", surface["code"])
			}
			if _, ok := surfaceReasonToLayer[code]; !ok {
				t.Fatalf("%s missing from the layer table", code)
			}
			if surface["layer"] != LayerGateway && surface["layer"] != LayerProvider {
				t.Fatalf("layer %v", surface["layer"])
			}
		})
	}
}

func TestModelCausedCodesStayOnTheProviderLayerAndRuntimeCodesOnGateway(t *testing.T) {
	layers := map[string]any{}
	for _, c := range []string{"truncated", "empty_response", "invalid_response", "session_busy", "loop_error"} {
		layers[c] = buildErrorSurfaceFromResult(map[string]any{"failed": true, "error": "x", "failure_reason": c}, "", "")["layer"]
	}
	if layers["truncated"] != LayerProvider || layers["empty_response"] != LayerProvider || layers["invalid_response"] != LayerProvider {
		t.Fatalf("model caused layers %v", layers)
	}
	if layers["session_busy"] != LayerGateway || layers["loop_error"] != LayerGateway {
		t.Fatalf("runtime layers %v", layers)
	}
}

func TestResultAndErrorSurfaceCarryTheUsageWindowSentence(t *testing.T) {
	caseHome(t)
	saved := billingOrEntitlementMessage
	t.Cleanup(func() { billingOrEntitlementMessage = saved })
	billingOrEntitlementMessage = func(context.Context, string, string, string, string, bool) string {
		return "Add credits to keep going now: https://x"
	}
	classified := &provider.Classified{Reason: "billing"}
	build := func(window UsageWindowRefusal) map[string]any {
		return billingFailureResult(context.Background(), classified, "HTTP 402", nil, 1, "nous", "https://inference-api.nousresearch.com/v1", "m", nil, window)
	}
	sentence := "You've hit your daily limit · resets 15:56 · /topup to add credits"
	limit := map[string]any{"gauge_id": "day", "scope": "all", "message": sentence, "credits": map[string]any{"action": "add_credits"},
		"suggestions": []any{map[string]any{"model": "a/b:free", "label": "A", "note": "free", "provider": "nous"}}}
	resets := 1_800_000_000.0
	result := build(UsageWindowRefusal{Message: sentence, ResetsAt: &resets, Limit: limit})
	if result["final_response"] != sentence || result["failure_reason"] != "billing" || !reflect.DeepEqual(result["usage_limit"], limit) {
		t.Fatalf("result %v", result)
	}
	surface := buildErrorSurfaceFromResult(result, "nous", "m")
	if surface["layer"] != "billing" || surface["code"] != "usage_window" || surface["message"] != sentence || !reflect.DeepEqual(surface["usage_limit"], limit) {
		t.Fatalf("surface %v", surface)
	}
	if surface["retryable"] != true || surface["resets_at"] != resets {
		t.Fatalf("retry at reset %v %v", surface["retryable"], surface["resets_at"])
	}
	two := build(UsageWindowRefusal{Message: sentence, ResetsAt: &resets})
	if _, ok := two["usage_limit"]; ok {
		t.Fatalf("two item window carried a limit %v", two)
	}
	if _, ok := buildErrorSurfaceFromResult(two, "nous", "m")["usage_limit"]; ok {
		t.Fatal("two item surface carried a limit")
	}
	plain := build(UsageWindowRefusal{})
	if _, ok := plain["usage_window_message"]; ok || !strings.HasPrefix(plain["final_response"].(string), "Billing or credits exhausted") {
		t.Fatalf("plain %v", plain)
	}
	if buildErrorSurfaceFromResult(plain, "nous", "m")["code"] != "billing" {
		t.Fatal("plain surface code")
	}
}
