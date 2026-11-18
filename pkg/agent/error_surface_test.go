package agent

import (
	"reflect"
	"testing"
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
		{"rate_limit", "provider exploded", "", map[string]any{"billing_block": map[string]any{"provider": "openrouter"}}, map[string]any{"layer": LayerBilling, "code": "rate_limit", "retryable": false}},
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
	oauth := buildErrorSurfaceFromResult(failedSurfaceResult("auth", "x", nil), "xai-oauth", "")
	if oauth["auth_kind"] != "oauth" || oauth["provider_label"] != "xAI Grok OAuth (SuperGrok / Premium+)" {
		t.Fatalf("xai-oauth %v", oauth)
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
