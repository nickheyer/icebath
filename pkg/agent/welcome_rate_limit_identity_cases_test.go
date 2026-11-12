package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	welcomeRouteURL = "https://welcome-api.nousresearch.com/v1"
	paidRouteURL    = "https://inference-api.nousresearch.com/v1"
)

// Unsigned portal token with the anonymous defaults the hermes fixture mints, overridden by claims
func welcomeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload := map[string]any{"sub": "nas_user:1", "client_id": "nas-anonymous", "account_tier": "anonymous",
		"scope": "inference:invoke", "exp": time.Now().Unix() + 900}
	for k, v := range claims {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	seg := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return seg([]byte(`{"alg":"RS256"}`)) + "." + seg(raw) + ".sig"
}

// Gateway 429 or 403 error carrying the JSON body the way the SDK error exposes it
func welcomeGatewayError(t *testing.T, status int, body map[string]any) *provider.APIError {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &provider.APIError{Provider: "nous", StatusCode: status, Body: string(raw), Message: fmt.Sprintf("Error code: %d - %s", status, raw)}
}

func welcomeRefusal(t *testing.T, reason string, retryAfter int) *provider.APIError {
	return welcomeGatewayError(t, 429, map[string]any{"status": 429, "message": "refused", "reason": reason, "retry_after": retryAfter,
		"alternates": []any{}, "upgrade_url": "https://portal.example/signup"})
}

func welcomeClassify(err *provider.APIError, model, baseURL, apiKey string) *provider.Classified {
	return provider.ClassifyError(provider.ClassifyInput{Err: err, Status: err.StatusCode, Body: err.Body, Provider: "nous", Model: model,
		BaseURL: baseURL, APIKey: apiKey})
}

func welcomeAgent(apiKey, baseURL, model string) *AIAgent {
	a := bareSteerAgent()
	a.provider, a.apiKey, a.model = "nous", apiKey, model
	a.setBaseURL(baseURL)
	return a
}

// Reset epoch of a recorded cooldown file, false when none was written
func recordedNousReset(t *testing.T, h string, anonymous bool) (float64, bool) {
	t.Helper()
	name := "nous.json"
	if anonymous {
		name = "nous-anonymous.json"
	}
	raw, err := os.ReadFile(filepath.Join(h, "rate_limits", name))
	if err != nil {
		return 0, false
	}
	var st struct {
		ResetAt float64 `json:"reset_at"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st.ResetAt, true
}

func TestLongWaitRuleTheTurnRecordsALongRefusalFromTheClassifiersContext(t *testing.T) {
	h := caseHome(t)
	ctx := home.WithOverride(context.Background(), h)
	anonKey := welcomeJWT(t, nil)
	err := welcomeRefusal(t, "rate_limited", 600)
	turnCtx := provider.ExtractAPIErrorContext(err)
	if _, has := turnCtx["welcome_refusal"]; has {
		t.Fatalf("turn context carries the refusal: %v", turnCtx)
	}
	classified := welcomeClassify(err, "nous/welcome", welcomeRouteURL, anonKey)
	if !isGenuineNousRateLimit(ctx, welcomeAgent(anonKey, welcomeRouteURL, "gpt-5"), err, turnCtx, classified) {
		t.Fatal("a long anonymous refusal is an exhausted allowance")
	}
	resetAt, recorded := recordedNousReset(t, h, true)
	want, ok := classified.ResetAt()
	if !recorded || !ok || math.Abs(resetAt-float64(want.UnixNano())/1e9) > 1e-3 {
		t.Fatalf("recorded %v at %v, classifier reset %v", recorded, resetAt, want)
	}
	if err := (&provider.NousRateGuard{Home: h, Anonymous: true}).Clear(); err != nil {
		t.Fatal(err)
	}
	paid := welcomeAgent(welcomeJWT(t, map[string]any{"account_tier": "paid"}), paidRouteURL, "gpt-5")
	if isGenuineNousRateLimit(ctx, paid, err, turnCtx, classified) {
		t.Fatal("a named account's identical body is not an anonymous allowance verdict")
	}
	if _, anon := recordedNousReset(t, h, true); anon {
		t.Fatal("named refusal recorded an anonymous cooldown")
	}
	if _, named := recordedNousReset(t, h, false); named {
		t.Fatal("named refusal recorded a cooldown")
	}
	short := welcomeRefusal(t, "rate_limited", 5)
	if isGenuineNousRateLimit(ctx, welcomeAgent(anonKey, welcomeRouteURL, "gpt-5"), short, provider.ExtractAPIErrorContext(short),
		welcomeClassify(short, "nous/welcome", welcomeRouteURL, anonKey)) {
		t.Fatal("a short refusal is waited out")
	}
	if _, anon := recordedNousReset(t, h, true); anon {
		t.Fatal("short refusal recorded a cooldown")
	}
}

func TestSigningInDoesNotInheritAnonymousCooldown(t *testing.T) {
	h := caseHome(t)
	ctx := home.WithOverride(context.Background(), h)
	guest := welcomeAgent(welcomeJWT(t, nil), welcomeRouteURL, "stepfun/step-3.7-flash:free")
	err := welcomeGatewayError(t, 429, map[string]any{"status": 429, "message": "refused", "reason": "rate_limited", "retry_after": 600})
	classified := welcomeClassify(err, "", welcomeRouteURL, guest.apiKey)
	if !isGenuineNousRateLimit(ctx, guest, err, provider.ExtractAPIErrorContext(err), classified) {
		t.Fatal("the anonymous refusal must trip the breaker")
	}
	guard := func(a *AIAgent) PhaseVerdict {
		return nousRateLimitGuard(ctx, a, &LoopState{ActiveSystemPrompt: "system", Retry: &TurnRetryState{}})
	}
	blocked := guard(guest)
	if blocked.Action != "return" || BuildErrorSurfaceFromResult(blocked.Result, "", "")["code"] != "free_tier_rate_limited" {
		t.Fatalf("guest guard %q result %v", blocked.Action, blocked.Result)
	}
	guest.apiKey = welcomeJWT(t, map[string]any{"account_tier": "free", "client_id": "hermes-cli"})
	if v := guard(guest); v.Action != "fallthrough" {
		t.Fatalf("signed in credential inherited the anonymous cooldown: %q", v.Action)
	}
	named := &provider.NousRateGuard{Home: h}
	if err := named.Record(http.Header{"Retry-After": []string{"300"}}, time.Time{}, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	namedBlocked := guard(guest)
	if _, hasFree := namedBlocked.Result["free_tier"]; namedBlocked.Action != "return" || hasFree {
		t.Fatalf("named guard %q result %v", namedBlocked.Action, namedBlocked.Result)
	}
	if err := named.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, ok := named.Remaining(); ok {
		t.Fatal("cleared cooldown still remaining")
	}
}
