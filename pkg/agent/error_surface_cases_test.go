package agent

import (
	"reflect"
	"testing"
)

func TestResultNoneForNonDict(t *testing.T) {
	if s := buildErrorSurfaceFromResult("boom", "", ""); s != nil {
		t.Fatalf("%v", s)
	}
	if s := buildErrorSurfaceFromResult(nil, "", ""); s != nil {
		t.Fatalf("%v", s)
	}
}

func TestResultNoneForHealthyResult(t *testing.T) {
	if s := buildErrorSurfaceFromResult(map[string]any{"completed": true, "final_response": "hi"}, "", ""); s != nil {
		t.Fatalf("%v", s)
	}
}

func TestResultAuthReasonsMapToAuthLayer(t *testing.T) {
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("auth", "provider exploded", nil), "", ""); !reflect.DeepEqual(s, map[string]any{"layer": LayerAuth, "code": "auth", "retryable": false}) {
		t.Fatalf("auth %v", s)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("auth_permanent", "provider exploded", nil), "", ""); s["layer"] != LayerAuth || s["retryable"] != false {
		t.Fatalf("auth_permanent %v", s)
	}
}

func TestAuthSurfaceNamesOauthVsApiKeyRecovery(t *testing.T) {
	oauth := buildErrorSurfaceFromResult(failedSurfaceResult("auth", "provider exploded", nil), "xai-oauth", "")
	if oauth["auth_kind"] != "oauth" || oauth["provider_label"] != "xAI Grok OAuth (SuperGrok / Premium+)" {
		t.Fatalf("xai-oauth %v", oauth)
	}
	key := buildErrorSurfaceFromResult(failedSurfaceResult("auth", "provider exploded", nil), "openrouter", "")
	if key["auth_kind"] != "api_key" || key["api_key_env"] != "OPENROUTER_API_KEY" {
		t.Fatalf("openrouter %v", key)
	}
	if _, has := oauth["api_key_env"]; has {
		t.Fatalf("oauth carries api_key_env %v", oauth)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("auth", "provider exploded", nil), "opencode-go", ""); s["api_key_env"] != "OPENCODE_GO_API_KEY" {
		t.Fatalf("opencode %v", s)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("rate_limit", "provider exploded", nil), "xai-oauth", ""); s["auth_kind"] != nil {
		t.Fatalf("non auth layer carries auth_kind %v", s)
	}
}

func TestEveryProviderSurfaceCarriesTheDisplayLabel(t *testing.T) {
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("timeout", "provider exploded", nil), "opencode-go", ""); s["provider_label"] != "OpenCode Go" {
		t.Fatalf("%v", s)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("timeout", "provider exploded", nil), "", ""); s["provider_label"] != nil {
		t.Fatalf("%v", s)
	}
}

func TestResultBillingBlockWins(t *testing.T) {
	s := buildErrorSurfaceFromResult(failedSurfaceResult("rate_limit", "provider exploded", map[string]any{"billing_block": map[string]any{"provider": "openrouter"}}), "", "")
	if s["layer"] != LayerBilling || s["retryable"] != false {
		t.Fatalf("%v", s)
	}
}

func TestResultBillingReasonWithoutBlock(t *testing.T) {
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("billing", "provider exploded", nil), "", ""); !reflect.DeepEqual(s, map[string]any{"layer": LayerBilling, "code": "billing", "retryable": false}) {
		t.Fatalf("%v", s)
	}
}

func TestResultProviderDefaultForClassifiedReasons(t *testing.T) {
	for _, reason := range []string{"rate_limit", "server_error", "overloaded", "unknown", "format_error"} {
		if s := buildErrorSurfaceFromResult(failedSurfaceResult(reason, "provider exploded", nil), "", ""); s["layer"] != LayerProvider || s["code"] != reason {
			t.Fatalf("%s -> %v", reason, s)
		}
	}
}

func TestResultNonRetryableReasons(t *testing.T) {
	for _, reason := range []string{"auth", "format_error", "content_policy_blocked", "model_not_found", "ssl_cert_verification"} {
		if s := buildErrorSurfaceFromResult(failedSurfaceResult(reason, "provider exploded", nil), "", ""); s["retryable"] != false {
			t.Fatalf("%s retryable %v", reason, s)
		}
	}
}

func TestResultPrefersClassifierRetryVerdict(t *testing.T) {
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("unknown", "provider exploded", map[string]any{"failure_retryable": false}), "", ""); s["retryable"] != false {
		t.Fatalf("%v", s)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("format_error", "provider exploded", map[string]any{"failure_retryable": true}), "", ""); s["retryable"] != true {
		t.Fatalf("%v", s)
	}
}

func TestResultStampsFailingSessionIdentity(t *testing.T) {
	s := buildErrorSurfaceFromResult(failedSurfaceResult("rate_limit", "provider exploded", nil), "openrouter", "test/m1")
	if s["provider"] != "openrouter" || s["model"] != "test/m1" {
		t.Fatalf("%v", s)
	}
	s = buildErrorSurfaceFromResult(failedSurfaceResult("rate_limit", "provider exploded", nil), "", "")
	if _, has := s["provider"]; has {
		t.Fatalf("%v", s)
	}
	if _, has := s["model"]; has {
		t.Fatalf("%v", s)
	}
}

func TestResultTimeoutOnCustomEndpointIsEndpointLayer(t *testing.T) {
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("timeout", "provider exploded", nil), "custom", ""); s["layer"] != LayerEndpoint {
		t.Fatalf("custom %v", s)
	}
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("timeout", "provider exploded", nil), "anthropic", ""); s["layer"] != LayerProvider {
		t.Fatalf("anthropic %v", s)
	}
}

func TestResultStreamDropTextMapsToStreaming(t *testing.T) {
	s := buildErrorSurfaceFromResult(failedSurfaceResult("", "The provider's stream connection keeps dropping", nil), "", "")
	if s["layer"] != LayerStreaming || s["code"] != "stream_drop" || s["retryable"] != true {
		t.Fatalf("%v", s)
	}
}

func TestResultUnclassifiedFailureDefaultsToProviderUnknown(t *testing.T) {
	if s := buildErrorSurfaceFromResult(failedSurfaceResult("", "something odd", nil), "", ""); !reflect.DeepEqual(s, map[string]any{"layer": LayerProvider, "code": "unknown", "retryable": true}) {
		t.Fatalf("%v", s)
	}
}

func TestResultDiskFullWinsOverReason(t *testing.T) {
	s := buildErrorSurfaceFromResult(failedSurfaceResult("server_error", "OSError: [Errno 28] No space left on device", nil), "", "")
	if s["layer"] != LayerDisk || s["retryable"] != false {
		t.Fatalf("%v", s)
	}
}
