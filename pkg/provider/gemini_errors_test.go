package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	gemAQKey           = "AQ.CCCCCCCCCCCCCCCCCCCCCCCC"
	gemAIzaKey         = "AIzaSy_TEST_xxxxxxxxxxxxxxxxxxxxxxxx"
	gemAQAuthKey       = "AQ.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	gemStudioBase      = "https://generativelanguage.googleapis.com/v1beta"
	gemExpressBase     = "https://aiplatform.googleapis.com/v1beta1/publishers/google"
	gemGoogleAuthMsg   = "Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other valid authentication credential. See https://developers.google.com/identity/sign-in/web/devconsole-project."
	gemGuidanceMarker  = "rejected this API key's type"
	gemAPIKeyInvalidMs = "API key not valid. Please pass a valid API key."
)

// Google JSON error body with an optional ErrorInfo reason
func gemGoogleErrorBody(code int, message, status, reason string) string {
	e := map[string]any{"code": code, "message": message, "status": status}
	if reason != "" {
		e["details"] = []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": reason, "domain": "googleapis.com",
			"metadata": map[string]any{"service": "generativelanguage.googleapis.com"}}}
	}
	data, _ := json.Marshal(map[string]any{"error": e})
	return string(data)
}

// Probe against a fake endpoint answering with a status, headers and body
func gemRunProbe(t *testing.T, status int, headers map[string]string, body string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "fake-key" || !strings.HasSuffix(r.URL.Path, "/v1beta/models/gemini-3.7-flash:generateContent") {
			w.WriteHeader(500)
			return
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	return probeGeminiTier(context.Background(), "fake-key", srv.URL, "", 10*time.Second)
}

func TestGeminiProbeTierFreeTierVia429Body(t *testing.T) {
	body := `{"error":{"code":429,"message":"Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 20"}}`
	if got := gemRunProbe(t, 429, nil, body); got != "free" {
		t.Fatalf("tier %s", got)
	}
}

func TestGeminiProbeTierSuccessful200WithoutRPDHeaderIsPaid(t *testing.T) {
	if got := gemRunProbe(t, 200, nil, `{"candidates":[]}`); got != "paid" {
		t.Fatalf("tier %s", got)
	}
}

func TestGeminiProbeTierRPDHeaderAndFailures(t *testing.T) {
	if got := gemRunProbe(t, 200, map[string]string{"X-RateLimit-Limit-Requests-Per-Day": "1000"}, `{}`); got != "free" {
		t.Fatalf("rpd 1000 %s", got)
	}
	if got := gemRunProbe(t, 200, map[string]string{"X-RateLimit-Limit-Requests-Per-Day": "1500"}, `{}`); got != "paid" {
		t.Fatalf("rpd 1500 %s", got)
	}
	if got := gemRunProbe(t, 500, nil, ``); got != "unknown" {
		t.Fatalf("500 %s", got)
	}
	if got := probeGeminiTier(context.Background(), "  ", "", "", 0); got != "unknown" {
		t.Fatalf("blank key %s", got)
	}
}

func TestGeminiIsFreeTierQuotaError(t *testing.T) {
	if !isFreeTierQuotaError("Quota exceeded for metric: generate_content_free_tier_requests") || isFreeTierQuotaError("rate limited") || isFreeTierQuotaError("") {
		t.Fatal("free tier marker")
	}
}

func TestGeminiHTTPErrorFreeTier429AppendsGuidance(t *testing.T) {
	body := `{"error":{"code":429,"message":"Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 20","status":"RESOURCE_EXHAUSTED"}}`
	msg := geminiHTTPError(429, http.Header{}, body, "", "", true).Error()
	if !strings.Contains(strings.ToLower(msg), "free tier") || !strings.Contains(msg, "aistudio.google.com/apikey") {
		t.Fatal(msg)
	}
}

func TestGeminiHTTPErrorPaid429HasNoBillingURL(t *testing.T) {
	msg := geminiHTTPError(429, http.Header{}, `{"error":{"code":429,"message":"Rate limited","status":"RESOURCE_EXHAUSTED"}}`, "", "", true).Error()
	if strings.Contains(msg, "aistudio.google.com/apikey") {
		t.Fatal(msg)
	}
}

func TestGeminiWrongSurfaceGuidance(t *testing.T) {
	if text := wrongGeminiSurfaceGuidance(gemStudioBase, gemAQKey, "PERMISSION_DENIED"); !strings.Contains(text, "aiplatform.googleapis.com/v1beta1") || !strings.Contains(text, "Vertex") {
		t.Fatal(text)
	}
	if text := wrongGeminiSurfaceGuidance(gemExpressBase, gemAQKey, "PERMISSION_DENIED"); !strings.Contains(text, "generativelanguage.googleapis.com") || !strings.Contains(text, "aistudio.google.com") {
		t.Fatal(text)
	}
	for _, tc := range [][3]string{{gemStudioBase, gemAQKey, "UNAUTHENTICATED"}, {gemStudioBase, gemAQKey, ""}, {gemStudioBase, gemAIzaKey, "PERMISSION_DENIED"},
		{"https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi", gemAQKey, "PERMISSION_DENIED"}} {
		if text := wrongGeminiSurfaceGuidance(tc[0], tc[1], tc[2]); text != "" {
			t.Fatalf("%v: %q", tc, text)
		}
	}
}

func TestGeminiHTTPErrorSurfaceGuidance(t *testing.T) {
	body := `{"error": {"code": 403, "message": "Requests to this API aiplatform.googleapis.com method google.cloud.aiplatform.v1beta1.PredictionService.StreamGenerateContent are blocked.", "status": "PERMISSION_DENIED"}}`
	err := geminiHTTPError(403, http.Header{}, body, gemAQKey, gemStudioBase, true)
	if !strings.Contains(err.Error(), "aiplatform.googleapis.com/v1beta1") || err.StatusCode != 403 {
		t.Fatal(err)
	}
	raw := geminiHTTPError(403, http.Header{}, `{"error": {"code": 403, "message": "Location is not found", "status": "PERMISSION_DENIED"}}`, gemAIzaKey, gemStudioBase, true)
	if strings.Contains(raw.Error(), "aiplatform.googleapis.com/v1beta1") || strings.Contains(raw.Error(), "aistudio.google.com") {
		t.Fatal(raw)
	}
}

func TestGeminiIsStandardKeyAuthError(t *testing.T) {
	if isStandardKeyAuthError(400, gemGoogleAuthMsg, "", "") || isStandardKeyAuthError(403, gemGoogleAuthMsg, "", "") ||
		isStandardKeyAuthError(400, `Invalid JSON payload received. Unknown name "x".`, "INVALID_ARGUMENT", gemAIzaKey) {
		t.Fatal("oauth message requires 401")
	}
	if isStandardKeyAuthError(401, "", "", "") {
		t.Fatal("empty message")
	}
	if !isStandardKeyAuthError(400, gemAPIKeyInvalidMs, "API_KEY_INVALID", gemAIzaKey) || !isStandardKeyAuthError(400, gemAPIKeyInvalidMs, "", gemAIzaKey) {
		t.Fatal("AIza 400")
	}
	if isStandardKeyAuthError(400, gemAPIKeyInvalidMs, "API_KEY_INVALID", "") || isStandardKeyAuthError(400, gemAPIKeyInvalidMs, "API_KEY_INVALID", gemAQAuthKey) {
		t.Fatal("non AIza 400")
	}
}

func TestGeminiHTTPErrorStandardKeyGuidance(t *testing.T) {
	err := geminiHTTPError(401, http.Header{}, gemGoogleErrorBody(401, gemGoogleAuthMsg, "UNAUTHENTICATED", "ACCESS_TOKEN_TYPE_UNSUPPORTED"), "", "", true)
	if !strings.Contains(err.Error(), gemGuidanceMarker) || !strings.Contains(err.Error(), "aistudio.google.com/api-keys") || err.Code != "gemini_unauthorized" {
		t.Fatal(err)
	}
	if err.Details["reason"] != "ACCESS_TOKEN_TYPE_UNSUPPORTED" || err.Details["status"] != "UNAUTHENTICATED" {
		t.Fatalf("details %v", err.Details)
	}
	invalid := gemGoogleErrorBody(400, gemAPIKeyInvalidMs, "INVALID_ARGUMENT", "API_KEY_INVALID")
	err = geminiHTTPError(400, http.Header{}, invalid, gemAIzaKey, "", true)
	if !strings.Contains(err.Error(), gemGuidanceMarker) || !strings.Contains(err.Error(), "aistudio.google.com/api-keys") || err.Code != "gemini_http_400" {
		t.Fatal(err)
	}
	if strings.Contains(geminiHTTPError(400, http.Header{}, invalid, gemAQAuthKey, "", true).Error(), gemGuidanceMarker) {
		t.Fatal("auth key got guidance")
	}
	if strings.Contains(geminiHTTPError(403, http.Header{}, gemGoogleErrorBody(403, gemGoogleAuthMsg, "PERMISSION_DENIED", ""), "", "", true).Error(), gemGuidanceMarker) {
		t.Fatal("403 got guidance")
	}
}

func TestGeminiClientErrorKeepsGuidanceAndRetryAfter(t *testing.T) {
	f := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Retry-After", "7")
		gemWriteJSON(w, 429, `{"error":{"code":429,"message":"Quota exceeded for metric: generate_content_free_tier_requests, limit: 20","status":"RESOURCE_EXHAUSTED"}}`)
	})
	c := gemClient(t, f, gemStudioBase, gemAIzaKey, "gemini-2.5-flash")
	_, err := c.Complete(context.Background(), gemUserRequest("gemini-2.5-flash", "hi"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "gemini_rate_limited" || apiErr.StatusCode != 429 || apiErr.RetryAfter != 7*time.Second ||
		!strings.Contains(apiErr.Message, "aistudio.google.com/apikey") {
		t.Fatalf("err %#v", apiErr)
	}
	sf := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		gemWriteJSON(w, 401, gemGoogleErrorBody(401, gemGoogleAuthMsg, "UNAUTHENTICATED", "ACCESS_TOKEN_TYPE_UNSUPPORTED"))
	})
	sc := gemClient(t, sf, gemStudioBase, gemAIzaKey, "gemini-2.5-flash")
	_, err = sc.Stream(context.Background(), gemUserRequest("gemini-2.5-flash", "hi"), nil)
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrAuth || !strings.Contains(apiErr.Message, gemGuidanceMarker) {
		t.Fatalf("stream err %#v", err)
	}
}

func TestGeminiVertexBearerUsesPublishersPathWithoutKeyGuidance(t *testing.T) {
	f := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 0 {
			gemWriteJSON(w, 200, `{"candidates": [{"content": {"parts": [{"text": "ok"}]}, "finishReason": "STOP"}]}`)
			return
		}
		gemWriteJSON(w, 403, `{"error": {"code": 403, "message": "denied", "status": "PERMISSION_DENIED"}}`)
	})
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cl, err := newGeminiNativeClient(Route{Provider: "vertex", Model: "gemini-3.7-flash", Mode: ModeGeminiNative, Auth: AuthGCPOAuth, Transport: f,
		BaseURL:     "https://us-central1-aiplatform.googleapis.com/v1beta1/projects/p/locations/us-central1/endpoints/openapi",
		TokenSource: func(context.Context) (string, error) { return "ya29.token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Complete(context.Background(), gemUserRequest("google/gemini-3.7-flash", "hi")); err != nil {
		t.Fatal(err)
	}
	rec := f.all()[0]
	if rec.URL != "https://us-central1-aiplatform.googleapis.com/v1beta1/projects/p/locations/us-central1/publishers/google/models/gemini-3.7-flash:generateContent" ||
		rec.Headers.Get("Authorization") != "Bearer ya29.token" || rec.Headers.Get("x-goog-api-key") != "" {
		t.Fatalf("rec %v %v", rec.URL, rec.Headers)
	}
	_, err = cl.Complete(context.Background(), gemUserRequest("gemini-3.7-flash", "hi"))
	if err == nil || strings.Contains(err.Error(), "aistudio.google.com") {
		t.Fatalf("bearer error %v", err)
	}
}

func TestGeminiAliasesPreserveNativeEndpointSelection(t *testing.T) {
	for _, provider := range []string{"gemini", "google", "google-gemini", "google-ai-studio"} {
		for base, native := range map[string]bool{"https://generativelanguage.googleapis.com/v1beta": true, "https://example.invalid/v1": false} {
			c, err := NewClient(Route{Provider: provider, Model: "gemini-flash-latest", BaseURL: base, APIKey: "test-key", Mode: ModeChatCompletions})
			if err != nil {
				t.Fatal(err)
			}
			if _, isNative := c.(*geminiNativeClient); isNative != native {
				t.Fatalf("%s %s: %T", provider, base, c)
			}
		}
	}
	if c, err := NewClient(Route{Provider: "custom", Model: "m", BaseURL: "http://localhost:4000/gemini", APIKey: "k", Mode: ModeGeminiNative}); err != nil {
		t.Fatal(err)
	} else if _, ok := c.(*geminiNativeClient); !ok {
		t.Fatalf("gemini_native mode %T", c)
	}
}

// Pool probe double reporting availability and entry count
type gemPoolProbe struct {
	available bool
	entries   int
}

func (p gemPoolProbe) Provider() string                         { return "gemini" }
func (p gemPoolProbe) HasCredentials() bool                     { return p.entries > 0 }
func (p gemPoolProbe) HasAvailable(string) bool                 { return p.available }
func (p gemPoolProbe) NextAvailableAt(string) (time.Time, bool) { return time.Time{}, false }
func (p gemPoolProbe) EntryCount() int                          { return p.entries }
func (p gemPoolProbe) LiftReopenedCooldowns(string) bool        { return false }

func TestGeminiFastFallbackPoolRecovery(t *testing.T) {
	if !PoolMayRecoverFromRateLimit(gemPoolProbe{available: true, entries: 3}) {
		t.Fatal("multi entry pool should recover")
	}
	if PoolMayRecoverFromRateLimit(gemPoolProbe{available: false, entries: 3}) {
		t.Fatal("exhausted pool should skip rotation")
	}
	if PoolMayRecoverFromRateLimit(gemPoolProbe{available: true, entries: 1}) || PoolMayRecoverFromRateLimit(nil) {
		t.Fatal("single entry or missing pool should fall back")
	}
}

func TestGeminiDisabledReasoningBoundsThinkingWhereSupported(t *testing.T) {
	off := false
	for model, field := range map[string][2]any{
		"gemini-2.5-flash": {"thinkingBudget", 0}, "gemini-3.6-flash": {"thinkingLevel", "low"}, "gemini-3.1-pro": {"thinkingLevel", "low"},
		"gemini-4-argon": {"thinkingLevel", "low"}, "google/gemini-4-argon": {"thinkingLevel", "low"}, "gemini-flash-lite-latest": {"thinkingLevel", "low"},
		"gemini-flash-latest": {"thinkingLevel", "low"}, "gemini-1.5-flash": {nil, nil},
	} {
		for _, r := range []Reasoning{{Enabled: &off}, {Effort: "none"}} {
			cfg := buildGeminiThinkingConfig(model, reasoningFromRequest(r))
			if cfg == nil || cfg["includeThoughts"] != false {
				t.Fatalf("%s %+v: %v", model, r, cfg)
			}
			if field[0] == nil {
				if _, ok := cfg["thinkingBudget"]; ok {
					t.Fatalf("%s budget %v", model, cfg)
				}
				if _, ok := cfg["thinkingLevel"]; ok {
					t.Fatalf("%s level %v", model, cfg)
				}
				continue
			}
			other := "thinkingBudget"
			if field[0] == "thinkingBudget" {
				other = "thinkingLevel"
			}
			if cfg[field[0].(string)] != field[1] {
				t.Fatalf("%s %v", model, cfg)
			}
			if _, ok := cfg[other]; ok {
				t.Fatalf("%s %s present %v", model, other, cfg)
			}
		}
	}
}

func TestGeminiEnabledReasoningNeverBoundsThinkingAndNonGeminiGetsNothing(t *testing.T) {
	on, off := true, false
	for _, r := range []Reasoning{{Enabled: &on}, {Effort: "medium"}} {
		gemEq(t, buildGeminiThinkingConfig("gemini-2.5-flash", reasoningFromRequest(r)), `{"includeThoughts": true}`)
	}
	if buildGeminiThinkingConfig("gpt-4o", reasoningFromRequest(Reasoning{Enabled: &off})) != nil || buildGeminiThinkingConfig("gemma-2b", reasoningFromRequest(Reasoning{Enabled: &off})) != nil {
		t.Fatal("non gemini got a config")
	}
}

func TestGeminiSnakeCaseTranslationCarriesThinkingControls(t *testing.T) {
	gemEq(t, snakeCaseGeminiThinkingConfig(map[string]any{"includeThoughts": false, "thinkingBudget": 0}), `{"include_thoughts": false, "thinking_budget": 0}`)
	gemEq(t, snakeCaseGeminiThinkingConfig(map[string]any{"includeThoughts": false, "thinkingLevel": "low"}), `{"include_thoughts": false, "thinking_level": "low"}`)
	gemEq(t, snakeCaseGeminiThinkingConfig(map[string]any{"includeThoughts": false}), `{"include_thoughts": false}`)
}

func TestGemini4MapsEffortToThinkingLevelAndLongDecodeBudget(t *testing.T) {
	for _, tc := range []struct {
		effort, level string
		shown         bool
		budget        int
	}{{"none", "low", false, 0}, {"minimal", "low", true, 0}, {"medium", "medium", true, 0}, {"high", "high", true, 0}, {"xhigh", "high", true, 262_144}, {"max", "high", true, 1_048_576}} {
		r := Reasoning{Effort: tc.effort}
		gemEq(t, buildGeminiThinkingConfig("gemini-4-argon", reasoningFromRequest(r)), gemCanon(t, map[string]any{"includeThoughts": tc.shown, "thinkingLevel": tc.level}))
		for base, native := range map[string]bool{gemStudioBase: true, gemStudioBase + "/openai": false} {
			route := Route{Provider: "gemini", Model: "gemini-4-argon", BaseURL: base, APIKey: "k", Mode: ModeChatCompletions}
			t.Setenv("ICEBATH_HOME", t.TempDir())
			req := gemUserRequest("gemini-4-argon", "hi")
			req.Reasoning = r
			var extra map[string]any
			if native {
				c := gemClient(t, newGemFake(t, func(http.ResponseWriter, *http.Request, int) {}), base, "k", "gemini-4-argon")
				p, err := c.prepare(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				extra = p.extra
			} else {
				cc := newCCRoute(route)
				extra = geminiPlain(cc.build(context.Background(), req, loadWireSettings(context.Background())).extraBody)
			}
			got, has := extra["max_output_tokens"]
			if native && tc.budget > 0 && got != tc.budget {
				t.Fatalf("%s native budget %v", tc.effort, got)
			}
			if (!native || tc.budget == 0) && has {
				t.Fatalf("%s %s budget %v", tc.effort, base, got)
			}
		}
	}
}

func TestGeminiEnabledReasoningMapsEffortToThinkingLevelOnGemini3AndLater(t *testing.T) {
	on := true
	for _, tc := range [][3]string{{"gemini-4-argon", "minimal", "low"}, {"gemini-4-argon", "low", "low"}, {"gemini-4-argon", "medium", "medium"},
		{"gemini-4-argon", "xhigh", "high"}, {"google/gemini-4-argon", "high", "high"}, {"gemini-3.8-flash", "medium", "medium"},
		{"gemini-3.1-pro", "medium", "low"}, {"gemini-3.1-pro", "high", "high"}, {"gemini-pro-latest", "high", "high"}} {
		cfg := buildGeminiThinkingConfig(tc[0], reasoningFromRequest(Reasoning{Enabled: &on, Effort: tc[1]}))
		gemEq(t, cfg, gemCanon(t, map[string]any{"includeThoughts": true, "thinkingLevel": tc[2]}))
	}
}

func TestGeminiUnversionedNonAliasIDsGetNoThinkingLevel(t *testing.T) {
	gemEq(t, buildGeminiThinkingConfig("gemini-exp-1206", reasoningFromRequest(Reasoning{Effort: "high"})), `{"includeThoughts": true}`)
	gemEq(t, buildGeminiThinkingConfig("gemini-1.5-pro", reasoningFromRequest(Reasoning{Effort: "high"})), `{"includeThoughts": true}`)
}
