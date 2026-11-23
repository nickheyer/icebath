package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// usageCall is one request a fake usage transport saw
type usageCall struct {
	Method, URL string
	Headers     http.Header
	Body        string
}

// usageRT answers requests from a queue of status and body pairs and records each call
type usageRT struct {
	mu        sync.Mutex
	calls     []usageCall
	responses []usageResp
}

type usageResp struct {
	status int
	body   string
}

func (rt *usageRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	body := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	rt.calls = append(rt.calls, usageCall{req.Method, req.URL.String(), req.Header.Clone(), body})
	r := rt.responses[0]
	if len(rt.responses) > 1 {
		rt.responses = rt.responses[1:]
	}
	return &http.Response{StatusCode: r.status, Body: io.NopCloser(strings.NewReader(r.body)), Header: http.Header{}, Request: req}, nil
}

func usageJSONText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Routes every usage HTTP client through a fake transport answering the given responses
func useUsageTransport(t *testing.T, responses ...usageResp) *usageRT {
	t.Helper()
	rt := &usageRT{responses: responses}
	prev := accountUsageClientFn
	accountUsageClientFn = func(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout, Transport: rt} }
	t.Cleanup(func() { accountUsageClientFn = prev })
	return rt
}

func isolateAccountUsage(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	accountUsageCache.Lock()
	accountUsageCache.snapshots = map[accountUsageKey]*AccountUsageSnapshot{}
	accountUsageCache.started = map[accountUsageKey]time.Time{}
	accountUsageCache.lastTry = map[accountUsageKey]time.Time{}
	accountUsageCache.inflight = map[accountUsageKey]bool{}
	accountUsageCache.Unlock()
	prevResolve, prevPool := codexUsageResolveFn, codexUsageLoadPoolFn
	codexUsageResolveFn = func(PoolEnv, bool) (map[string]any, error) {
		t.Fatal("legacy codex resolver must not be used")
		return nil, nil
	}
	codexUsageLoadPoolFn = func(PoolEnv) codexUsagePool {
		t.Fatal("pool must not be consulted")
		return nil
	}
	t.Cleanup(func() {
		codexUsageResolveFn, codexUsageLoadPoolFn = prevResolve, prevPool
	})
}

// fakeCodexPool is a codex pool answering select and refresh with fixed entries
type fakeCodexPool struct {
	selectEntry, refreshEntry *PooledCredential
	refreshHints              []string
}

func (p *fakeCodexPool) Select(string) (PooledCredential, bool) {
	if p.selectEntry == nil {
		return PooledCredential{}, false
	}
	return *p.selectEntry, true
}

func (p *fakeCodexPool) TryRefreshMatching(hint, _ string) (PooledCredential, bool) {
	p.refreshHints = append(p.refreshHints, hint)
	if p.refreshEntry == nil {
		return PooledCredential{}, false
	}
	return *p.refreshEntry, true
}

func codexUsagePayload() map[string]any {
	return map[string]any{
		"plan_type": "plus",
		"rate_limit": map[string]any{
			"primary_window":   map[string]any{"used_percent": 21, "reset_at": 1779846359},
			"secondary_window": map[string]any{"used_percent": 4, "reset_at": 1780230796},
		},
		"credits": map[string]any{"has_credits": false},
	}
}

func explicitCodexSnapshot(t *testing.T, payload any) (*AccountUsageSnapshot, *usageRT) {
	rt := useUsageTransport(t, usageResp{200, usageJSONText(payload)})
	snap := FetchAccountUsage(context.Background(), "openai-codex",
		AccountUsageFetchOptions{BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "live-agent-token"})
	return snap, rt
}

func windowLabels(s *AccountUsageSnapshot) []string {
	var out []string
	for _, w := range s.Windows {
		out = append(out, w.Label)
	}
	return out
}

func TestAccountUsageCodexPrefersExplicitLiveAgentCredentials(t *testing.T) {
	isolateAccountUsage(t)
	snap, rt := explicitCodexSnapshot(t, codexUsagePayload())
	if snap == nil || snap.Provider != "openai-codex" || snap.Plan != "Plus" {
		t.Fatalf("snapshot %+v", snap)
	}
	if got := windowLabels(snap); !reflect.DeepEqual(got, []string{"Session", "Weekly"}) || *snap.Windows[0].UsedPercent != 21 {
		t.Fatalf("windows %v", got)
	}
	if rt.calls[0].URL != "https://chatgpt.com/backend-api/wham/usage" || rt.calls[0].Headers.Get("Authorization") != "Bearer live-agent-token" {
		t.Fatalf("call %+v", rt.calls[0])
	}
}

func TestAccountUsageCodexFallsBackToNativeCredentialPool(t *testing.T) {
	isolateAccountUsage(t)
	rt := useUsageTransport(t, usageResp{200, usageJSONText(codexUsagePayload())})
	codexUsageResolveFn = func(PoolEnv, bool) (map[string]any, error) {
		return nil, &AuthError{Message: "no singleton auth", Provider: "openai-codex", Code: "codex_auth_missing"}
	}
	pool := &fakeCodexPool{selectEntry: &PooledCredential{Provider: "openai-codex", AccessToken: "pooled-token",
		BaseURL: "https://chatgpt.com/backend-api/codex"}}
	codexUsageLoadPoolFn = func(PoolEnv) codexUsagePool { return pool }
	snap := FetchAccountUsage(context.Background(), "openai-codex", AccountUsageFetchOptions{})
	if snap == nil || !reflect.DeepEqual(windowLabels(snap), []string{"Session", "Weekly"}) {
		t.Fatalf("snapshot %+v", snap)
	}
	c := rt.calls[0]
	if c.URL != "https://chatgpt.com/backend-api/wham/usage" || c.Headers.Get("Authorization") != "Bearer pooled-token" {
		t.Fatalf("call %+v", c)
	}
	if _, ok := c.Headers["Chatgpt-Account-Id"]; ok {
		t.Fatal("pool creds must omit the account id header")
	}
}

func TestAccountUsageCodexTransientResolverErrorNeverFallsToPool(t *testing.T) {
	isolateAccountUsage(t)
	useUsageTransport(t, usageResp{200, usageJSONText(codexUsagePayload())})
	codexUsageResolveFn = func(PoolEnv, bool) (map[string]any, error) { return nil, errors.New("network down") }
	if snap := FetchAccountUsage(context.Background(), "openai-codex", AccountUsageFetchOptions{}); snap != nil {
		t.Fatalf("a transient failure must fail open, got %+v", snap)
	}
}

func TestAccountUsageCodexWeeklyOnlyPrimaryWindowIsLabeledWeekly(t *testing.T) {
	isolateAccountUsage(t)
	snap, _ := explicitCodexSnapshot(t, map[string]any{"plan_type": "pro", "rate_limit": map[string]any{
		"primary_window": map[string]any{"used_percent": 1, "limit_window_seconds": 604800}, "secondary_window": nil}})
	if len(snap.Windows) != 1 || snap.Windows[0].Label != "Weekly" || *snap.Windows[0].UsedPercent != 1.0 {
		t.Fatalf("windows %+v", snap.Windows)
	}
}

func TestAccountUsageCodexWindowLabelsFollowDurationWithPositionalFallback(t *testing.T) {
	isolateAccountUsage(t)
	snap, _ := explicitCodexSnapshot(t, map[string]any{"rate_limit": map[string]any{
		"primary_window":   map[string]any{"used_percent": 4, "limit_window_seconds": 604800},
		"secondary_window": map[string]any{"used_percent": 21, "limit_window_seconds": 18000}}})
	if got := windowLabels(snap); !reflect.DeepEqual(got, []string{"Weekly", "Session"}) {
		t.Fatalf("labels %v", got)
	}
	snap, _ = explicitCodexSnapshot(t, map[string]any{"rate_limit": map[string]any{
		"primary_window":   map[string]any{"used_percent": 4},
		"secondary_window": map[string]any{"used_percent": 21, "limit_window_seconds": 12345}}})
	if got := windowLabels(snap); !reflect.DeepEqual(got, []string{"Session", "Weekly"}) {
		t.Fatalf("labels %v", got)
	}
}

func TestAccountUsageCodexSnapshotExposesExactRawPayloadWithOneGet(t *testing.T) {
	isolateAccountUsage(t)
	payload := codexUsagePayload()
	payload["future_field"] = map[string]any{"nested": []any{1, 2}}
	snap, rt := explicitCodexSnapshot(t, payload)
	if usageJSONText(snap.Raw) != usageJSONText(payload) || len(rt.calls) != 1 {
		t.Fatalf("raw %v calls %d", snap.Raw, len(rt.calls))
	}
	if !reflect.DeepEqual(snap.Raw["future_field"], map[string]any{"nested": []any{json.Number("1"), json.Number("2")}}) {
		t.Fatalf("future field %#v", snap.Raw["future_field"])
	}
	if NewAccountUsageSnapshot("anthropic", "x", nil, nil).Raw != nil {
		t.Fatal("raw defaults to none")
	}
}

func TestAccountUsageCodexInvalidPayloadFailsClosed(t *testing.T) {
	isolateAccountUsage(t)
	if snap, _ := explicitCodexSnapshot(t, []any{"not", "a", "dict"}); snap != nil {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestAccountUsageCodexRetries401WithForcedRefresh(t *testing.T) {
	isolateAccountUsage(t)
	rt := useUsageTransport(t, usageResp{401, "{}"}, usageResp{200, usageJSONText(codexUsagePayload())})
	var forced []bool
	codexUsageResolveFn = func(_ PoolEnv, force bool) (map[string]any, error) {
		forced = append(forced, force)
		token := "revoked-token"
		if force {
			token = "fresh-token"
		}
		return map[string]any{"api_key": token, "base_url": "https://chatgpt.com/backend-api/codex"}, nil
	}
	snap := FetchAccountUsage(context.Background(), "openai-codex", AccountUsageFetchOptions{})
	if snap == nil || snap.Windows[0].Label != "Session" || !reflect.DeepEqual(forced, []bool{false, true}) {
		t.Fatalf("snapshot %+v forced %v", snap, forced)
	}
	if rt.calls[0].Headers.Get("Authorization") != "Bearer revoked-token" || rt.calls[1].Headers.Get("Authorization") != "Bearer fresh-token" {
		t.Fatal("retry must use the refreshed token")
	}
}

func TestAccountUsageCodex401RetryRefreshesExplicitCredentialNotAnotherAccount(t *testing.T) {
	isolateAccountUsage(t)
	rt := useUsageTransport(t, usageResp{401, "{}"}, usageResp{200, usageJSONText(codexUsagePayload())})
	pool := &fakeCodexPool{refreshEntry: &PooledCredential{Provider: "openai-codex", AccessToken: "refreshed-own-token",
		BaseURL: "https://chatgpt.com/backend-api/codex"}}
	codexUsageLoadPoolFn = func(PoolEnv) codexUsagePool { return pool }
	snap := FetchAccountUsage(context.Background(), "openai-codex",
		AccountUsageFetchOptions{BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "own-expired-token"})
	if snap == nil || !reflect.DeepEqual(pool.refreshHints, []string{"own-expired-token"}) {
		t.Fatalf("snapshot %+v hints %v", snap, pool.refreshHints)
	}
	if rt.calls[1].Headers.Get("Authorization") != "Bearer refreshed-own-token" {
		t.Fatalf("retry headers %v", rt.calls[1].Headers)
	}
	pool.refreshEntry = nil
	useUsageTransport(t, usageResp{401, "{}"})
	if snap := FetchAccountUsage(context.Background(), "openai-codex",
		AccountUsageFetchOptions{BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "own-expired-token"}); snap != nil {
		t.Fatal("an unrefreshable session credential must fail open, never read another account")
	}
}

func exhaustedResetPayload(available int) map[string]any {
	return map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 100}},
		"rate_limit_reset_credits": map[string]any{"available_count": available}}
}

func TestAccountUsageRedeemRetries401WithForcedRefresh(t *testing.T) {
	isolateAccountUsage(t)
	rt := useUsageTransport(t, usageResp{401, "{}"}, usageResp{200, usageJSONText(exhaustedResetPayload(2))},
		usageResp{200, `{"code": "reset", "windows_reset": 2}`})
	codexUsageResolveFn = func(_ PoolEnv, force bool) (map[string]any, error) {
		token := "revoked-token"
		if force {
			token = "fresh-token"
		}
		return map[string]any{"api_key": token, "base_url": "https://chatgpt.com/backend-api/codex"}, nil
	}
	cleared := 0
	prev := codexUsageClearCooldownsFn
	codexUsageClearCooldownsFn = func(PoolEnv) { cleared++ }
	t.Cleanup(func() { codexUsageClearCooldownsFn = prev })
	res := RedeemCodexResetCredit(context.Background(), "", "", false)
	if res.Status != "reset" || res.AvailableCount != 1 || res.WindowsReset != 2 || cleared != 1 {
		t.Fatalf("result %+v cleared %d", res, cleared)
	}
	if res.Message != "✅ Reset redeemed — your usage limits have been reset. 1 banked reset remaining." {
		t.Fatalf("message %q", res.Message)
	}
	if len(rt.calls) != 3 || rt.calls[2].Method != http.MethodPost ||
		rt.calls[2].URL != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume" ||
		rt.calls[2].Headers.Get("Authorization") != "Bearer fresh-token" || !strings.Contains(rt.calls[2].Body, `"redeem_request_id"`) {
		t.Fatalf("calls %+v", rt.calls)
	}
}

func TestAccountUsageRedeemMissingCredentialsReportsUnavailable(t *testing.T) {
	isolateAccountUsage(t)
	codexUsageResolveFn = func(PoolEnv, bool) (map[string]any, error) { return nil, &AuthError{Message: "none"} }
	codexUsageLoadPoolFn = func(PoolEnv) codexUsagePool { return &fakeCodexPool{} }
	res := RedeemCodexResetCredit(context.Background(), "", "", false)
	if res.Status != "unavailable" || !strings.Contains(res.Message, "No Codex credentials available") || !strings.Contains(res.Message, "icebath auth") {
		t.Fatalf("result %+v", res)
	}
}

func TestAccountUsageRedeemGuardsAndOutcomes(t *testing.T) {
	isolateAccountUsage(t)
	useUsageTransport(t, usageResp{200, usageJSONText(map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 40}},
		"rate_limit_reset_credits": map[string]any{"available_count": 3}})})
	res := RedeemCodexResetCredit(context.Background(), "https://chatgpt.com/backend-api/codex", "tok", false)
	if res.Status != "not_exhausted" || !strings.Contains(res.Message, "only 40% used") || !strings.Contains(res.Message, "You have 3 resets banked") {
		t.Fatalf("result %+v", res)
	}
	useUsageTransport(t, usageResp{200, usageJSONText(exhaustedResetPayload(0))})
	if res := RedeemCodexResetCredit(context.Background(), "", "tok", false); res.Status != "no_credits_banked" {
		t.Fatalf("result %+v", res)
	}
	useUsageTransport(t, usageResp{200, usageJSONText(exhaustedResetPayload(1))}, usageResp{200, `{"code": "weird", "n": [1, 2.5, null, true]}`})
	res = RedeemCodexResetCredit(context.Background(), "", "tok", false)
	if res.Status != "unavailable" || res.Message != "Unexpected response from the Codex backend: {'code': 'weird', 'n': [1, 2.5, None, True]}" {
		t.Fatalf("result %+v", res)
	}
	useUsageTransport(t, usageResp{200, usageJSONText(exhaustedResetPayload(1))}, usageResp{200, `{"code": " Nothing_To_Reset "}`})
	if res := RedeemCodexResetCredit(context.Background(), "", "tok", false); res.Status != "nothing_to_reset" || res.AvailableCount != 1 {
		t.Fatalf("result %+v", res)
	}
	useUsageTransport(t, usageResp{403, "{}"})
	if res := RedeemCodexResetCredit(context.Background(), "", "tok", true); !strings.Contains(res.Message, "HTTP 403") || !strings.Contains(res.Message, "run `icebath auth`") {
		t.Fatalf("result %+v", res)
	}
	useUsageTransport(t, usageResp{502, "{}"})
	if res := RedeemCodexResetCredit(context.Background(), "", "tok", true); res.Message != "Codex backend error (HTTP 502) — try again shortly." {
		t.Fatalf("result %+v", res)
	}
	useUsageTransport(t, usageResp{200, usageJSONText(map[string]any{"rate_limit_reset_credits": map[string]any{"available_count": 1}})},
		usageResp{200, `{"code": "already_redeemed"}`})
	if res := RedeemCodexResetCredit(context.Background(), "", "tok", true); res.Status != "already_redeemed" || res.AvailableCount != 0 {
		t.Fatalf("forced redeem %+v", res)
	}
}

func TestAccountUsageAnthropicFetchHonorsExplicitAPIKeyAndMarksModelScopedWindows(t *testing.T) {
	isolateAccountUsage(t)
	prev := anthropicUsageTokenFn
	anthropicUsageTokenFn = func(PoolEnv) string { t.Fatal("ambient token must not be consulted"); return "" }
	t.Cleanup(func() { anthropicUsageTokenFn = prev })
	rt := useUsageTransport(t, usageResp{200, usageJSONText(map[string]any{
		"five_hour":        map[string]any{"utilization": 0.3, "resets_at": "2026-10-05T12:00:00Z"},
		"seven_day":        map[string]any{"utilization": 0.2, "resets_at": "2026-10-08T12:00:00Z"},
		"seven_day_opus":   map[string]any{"utilization": 1.0, "resets_at": "2026-10-09T12:00:00Z"},
		"seven_day_sonnet": map[string]any{"utilization": 0.4, "resets_at": "2026-10-09T12:00:00Z"},
		"extra_usage":      map[string]any{"is_enabled": true, "used_credits": 3.5, "monthly_limit": 50}})})
	snap := FetchAccountUsage(context.Background(), "anthropic", AccountUsageFetchOptions{APIKey: "sk-ant-oat-explicit-token"})
	if snap == nil || rt.calls[0].Headers.Get("Authorization") != "Bearer sk-ant-oat-explicit-token" || rt.calls[0].URL != anthropicOAuthUsageURL {
		t.Fatalf("snapshot %+v", snap)
	}
	scopes, percents := map[string]string{}, map[string]float64{}
	for _, w := range snap.Windows {
		scopes[w.Label], percents[w.Label] = w.Scope, *w.UsedPercent
	}
	want := map[string]string{"Current session": "account", "Current week": "account", "Opus week": "model", "Sonnet week": "model"}
	if !reflect.DeepEqual(scopes, want) || percents["Opus week"] != 100 || percents["Current session"] != 30 {
		t.Fatalf("scopes %v percents %v", scopes, percents)
	}
	if !reflect.DeepEqual(snap.Details, []string{"Extra usage: 3.50 / 50.00 USD"}) {
		t.Fatalf("details %v", snap.Details)
	}
	anthropicUsageTokenFn = func(PoolEnv) string { return "sk-ant-api03-plain-key" }
	snap = FetchAccountUsage(context.Background(), "anthropic", AccountUsageFetchOptions{})
	if snap == nil || snap.Available() || snap.UnavailableReason != "Anthropic account limits are only available for OAuth-backed Claude accounts." {
		t.Fatalf("api key snapshot %+v", snap)
	}
}

func useOpenRouterRuntime(t *testing.T) {
	prev := openrouterUsageRuntimeFn
	openrouterUsageRuntimeFn = func(context.Context, string, string) (*Runtime, error) {
		return &Runtime{Provider: "openrouter", APIKey: "or-key", BaseURL: "https://openrouter.ai/api/v1/"}, nil
	}
	t.Cleanup(func() { openrouterUsageRuntimeFn = prev })
}

func TestAccountUsageOpenRouterUsesLimitRemainingAndIgnoresDeprecatedRateLimit(t *testing.T) {
	isolateAccountUsage(t)
	useOpenRouterRuntime(t)
	rt := useUsageTransport(t, usageResp{200, `{"data": {"total_credits": 25, "total_usage": 5.5}}`},
		usageResp{200, `{"data": {"limit": 20, "limit_remaining": 15, "limit_reset": "monthly", "usage": 5, "usage_daily": 1.25,
			"usage_weekly": 0, "usage_monthly": 5, "rate_limit": {"requests": 10, "interval": "10s"}}}`})
	snap := FetchAccountUsage(context.Background(), "openrouter", AccountUsageFetchOptions{})
	if snap == nil || rt.calls[0].URL != "https://openrouter.ai/api/v1/credits" || rt.calls[1].URL != "https://openrouter.ai/api/v1/key" {
		t.Fatalf("snapshot %+v calls %+v", snap, rt.calls)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Label != "API key quota" || *snap.Windows[0].UsedPercent != 25 ||
		snap.Windows[0].Detail != "$15.00 of $20.00 remaining • resets monthly" {
		t.Fatalf("windows %+v", snap.Windows)
	}
	want := []string{"Credits balance: $19.50", "API key usage: $5.00 total • $1.25 today • $5.00 this month"}
	if !reflect.DeepEqual(snap.Details, want) {
		t.Fatalf("details %v", snap.Details)
	}
	for _, line := range RenderAccountUsageLines(snap, false) {
		if strings.Contains(line, "requests") || strings.Contains(line, "10s") {
			t.Fatalf("deprecated rate_limit rendered %q", line)
		}
	}
}

func TestAccountUsageOpenRouterOmitsQuotaWindowWhenKeyHasNoLimit(t *testing.T) {
	isolateAccountUsage(t)
	useOpenRouterRuntime(t)
	useUsageTransport(t, usageResp{200, `{"data": {"total_credits": 1, "total_usage": 3}}`},
		usageResp{200, `{"data": {"limit": null, "limit_remaining": null, "usage": 0.5}}`})
	snap := FetchAccountUsage(context.Background(), "openrouter", AccountUsageFetchOptions{})
	if snap == nil || len(snap.Windows) != 0 || !reflect.DeepEqual(snap.Details, []string{"Credits balance: $0.00", "API key usage: $0.50 total"}) {
		t.Fatalf("snapshot %+v", snap)
	}
	useUsageTransport(t, usageResp{200, `{"data": {"total_credits": 2}}`}, usageResp{500, `{}`})
	snap = FetchAccountUsage(context.Background(), "openrouter", AccountUsageFetchOptions{})
	if snap == nil || !reflect.DeepEqual(snap.Details, []string{"Credits balance: $2.00"}) {
		t.Fatalf("a failed key read must keep the balance, got %+v", snap)
	}
}

func useUsageProfiles(t *testing.T, profiles map[string]Profile) {
	prev := accountUsageProfileFn
	accountUsageProfileFn = func(_ context.Context, name string) (Profile, bool) {
		p, ok := profiles[name]
		return p, ok
	}
	t.Cleanup(func() { accountUsageProfileFn = prev })
}

func TestAccountUsageFetchReachesRegisteredPluginProfileAndFailsOpen(t *testing.T) {
	isolateAccountUsage(t)
	snap := NewAccountUsageSnapshot("plugin-usage", "plugin", nil, []string{"Credit: 10/100"})
	calls := 0
	useUsageProfiles(t, map[string]Profile{
		"plugin-usage": {Name: "plugin-usage", FetchAccountUsageHook: func(_ context.Context, baseURL, apiKey string) (*AccountUsageSnapshot, error) {
			calls++
			if baseURL != "https://plugin.test" || apiKey != "key" {
				t.Errorf("hook args %q %q", baseURL, apiKey)
			}
			return snap, nil
		}},
		"plugin-silent": {Name: "plugin-silent"},
		"plugin-broken": {Name: "plugin-broken", FetchAccountUsageHook: func(context.Context, string, string) (*AccountUsageSnapshot, error) {
			return nil, errors.New("nope")
		}},
	})
	if got := FetchAccountUsage(context.Background(), "plugin-usage", AccountUsageFetchOptions{BaseURL: "https://plugin.test", APIKey: "key"}); got != snap || calls != 1 {
		t.Fatalf("got %+v calls %d", got, calls)
	}
	if FetchAccountUsage(context.Background(), "plugin-silent", AccountUsageFetchOptions{}) != nil ||
		FetchAccountUsage(context.Background(), "plugin-broken", AccountUsageFetchOptions{}) != nil {
		t.Fatal("silent and broken profiles must read as no snapshot")
	}
	if !HasAccountUsage(context.Background(), "plugin-usage") || HasAccountUsage(context.Background(), "plugin-silent") ||
		!HasAccountUsage(context.Background(), "OpenRouter") || HasAccountUsage(context.Background(), "") {
		t.Fatal("usage support")
	}
}

func TestAccountUsageFetchPrefersBuiltinFetcherOverProfile(t *testing.T) {
	isolateAccountUsage(t)
	builtin := NewAccountUsageSnapshot("openrouter", "builtin", nil, nil)
	prev := accountUsageFetchers
	accountUsageFetchers = map[string]accountUsageFetcher{"openrouter": func(context.Context, string, string) (*AccountUsageSnapshot, error) {
		return builtin, nil
	}}
	t.Cleanup(func() { accountUsageFetchers = prev })
	calls := 0
	useUsageProfiles(t, map[string]Profile{"openrouter": {Name: "openrouter", FetchAccountUsageHook: func(context.Context, string, string) (*AccountUsageSnapshot, error) {
		calls++
		return nil, nil
	}}})
	if FetchAccountUsage(context.Background(), "openrouter", AccountUsageFetchOptions{}) != builtin || calls != 0 {
		t.Fatal("builtin fetcher must win")
	}
}

func TestAccountUsagePluginHookIsBoundedAndFailsOpen(t *testing.T) {
	isolateAccountUsage(t)
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	useUsageProfiles(t, map[string]Profile{"plugin-hang": {Name: "plugin-hang", FetchAccountUsageHook: func(context.Context, string, string) (*AccountUsageSnapshot, error) {
		close(started)
		<-release
		return NewAccountUsageSnapshot("plugin-hang", "late", nil, nil), nil
	}}})
	prevDeadline := PluginUsageHookDeadline
	PluginUsageHookDeadline = 300 * time.Millisecond
	t.Cleanup(func() { PluginUsageHookDeadline = prevDeadline })
	builtinCalls := 0
	prev := accountUsageFetchers
	accountUsageFetchers = map[string]accountUsageFetcher{"openrouter": func(context.Context, string, string) (*AccountUsageSnapshot, error) {
		builtinCalls++
		return nil, nil
	}}
	t.Cleanup(func() { accountUsageFetchers = prev })
	t0 := time.Now()
	if FetchAccountUsage(context.Background(), "plugin-hang", AccountUsageFetchOptions{}) != nil {
		t.Fatal("an overrun hook must read as no snapshot")
	}
	select {
	case <-started:
	default:
		t.Fatal("hook never started")
	}
	if time.Since(t0) >= 1300*time.Millisecond {
		t.Fatalf("took %v", time.Since(t0))
	}
	FetchAccountUsage(context.Background(), "openrouter", AccountUsageFetchOptions{})
	if builtinCalls != 1 {
		t.Fatal("builtin fetchers are untouched by the hook deadline")
	}
}

func TestAccountUsagePluginHookPanicStaysInTheCaller(t *testing.T) {
	isolateAccountUsage(t)
	prev := accountUsageFetchers
	accountUsageFetchers = map[string]accountUsageFetcher{}
	t.Cleanup(func() { accountUsageFetchers = prev })
	useUsageProfiles(t, map[string]Profile{"plugin-boom": {Name: "plugin-boom", FetchAccountUsageHook: func(context.Context, string, string) (*AccountUsageSnapshot, error) {
		panic("boom from plugin")
	}}})
	if FetchAccountUsage(context.Background(), "plugin-boom", AccountUsageFetchOptions{}) != nil {
		t.Fatal("a panicking hook must fail open")
	}
}

func usageF64(v float64) *float64 { return &v }

func codexUsageServer(t *testing.T, statuses map[string]int, payload map[string]any) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if code := statuses[token]; code != 0 && code != 200 {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error": "unauthorized"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func codexAccessJWT(account, sub string) string {
	seg := func(obj map[string]any) string {
		raw, _ := json.Marshal(obj)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	claims := map[string]any{"sub": sub, "exp": time.Now().Unix() + 900, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}}
	return seg(map[string]any{"alg": "RS256"}) + "." + seg(claims) + ".sig"
}

func sessionPayload(used float64) map[string]any {
	return map[string]any{"rate_limit": map[string]any{
		"primary_window":   map[string]any{"used_percent": used, "limit_window_seconds": 18000},
		"secondary_window": map[string]any{"used_percent": 5.0, "limit_window_seconds": 604800}}}
}

func TestAccountUsageReadOnlyFetchRoutesToEntriesOwnHostAndParses(t *testing.T) {
	isolateAccountUsage(t)
	token := codexAccessJWT("acct-9", "user-9")
	srv, seen := codexUsageServer(t, nil, sessionPayload(44))
	snap := FetchAccountUsage(context.Background(), "openai-codex", AccountUsageFetchOptions{BaseURL: srv.URL + "/backend-api/codex",
		APIKey: token, ReadOnly: true, IdentityID: "codex:acct-9:user-9"})
	if snap == nil || snap.Identity != "codex:acct-9:user-9" {
		t.Fatalf("snapshot %+v", snap)
	}
	got := []string{}
	for _, w := range snap.Windows {
		got = append(got, w.Label+" "+pyFloatRepr(*w.UsedPercent)+" "+w.Scope)
	}
	if !reflect.DeepEqual(got, []string{"Session 44.0 account", "Weekly 5.0 account"}) {
		t.Fatalf("windows %v", got)
	}
	if (*seen)[0] != "/backend-api/wham/usage Bearer "+token {
		t.Fatalf("request %v", *seen)
	}
}

func TestAccountUsageReadOnly401RaisesAndNeverRetriesWithRotation(t *testing.T) {
	isolateAccountUsage(t)
	token := codexAccessJWT("acct-1", "user-1")
	srv, seen := codexUsageServer(t, map[string]int{token: 401}, nil)
	if snap := FetchAccountUsage(context.Background(), "openai-codex", AccountUsageFetchOptions{BaseURL: srv.URL + "/backend-api/codex",
		APIKey: token, ReadOnly: true}); snap != nil {
		t.Fatalf("snapshot %+v", snap)
	}
	if len(*seen) != 1 {
		t.Fatalf("requests %v", *seen)
	}
}

func TestAccountUsageReadOnlyFetchRememberedUnderIdentitySlot(t *testing.T) {
	isolateAccountUsage(t)
	token := codexAccessJWT("acct-7", "user-7")
	srv, _ := codexUsageServer(t, nil, sessionPayload(66))
	FetchAccountUsage(context.Background(), "openai-codex", AccountUsageFetchOptions{BaseURL: srv.URL + "/backend-api/codex",
		APIKey: token, ReadOnly: true, IdentityID: "codex:acct-7:user-7"})
	snap := CachedAccountUsage(context.Background(), "openai-codex", "codex:acct-7:user-7", "")
	if snap == nil || *snap.Windows[0].UsedPercent != 66 {
		t.Fatalf("snapshot %+v", snap)
	}
	if CachedAccountUsage(context.Background(), "openai-codex", "", "") != nil {
		t.Fatal("a per credential snapshot must never pose as the provider wide gauge")
	}
}

func TestAccountUsageCacheSlotsThrottleAndStaleness(t *testing.T) {
	isolateAccountUsage(t)
	ctx := context.Background()
	older := NewAccountUsageSnapshot("openai-codex", "x", []AccountUsageWindow{accountWindow("Session", usageF64(10), nil, "")}, nil)
	newer := NewAccountUsageSnapshot("openai-codex", "x", []AccountUsageWindow{accountWindow("Session", usageF64(20), nil, "")}, nil)
	newer.Identity = "codex:a:b"
	t0 := time.Now()
	RememberAccountUsage(ctx, "openai-codex", newer, "", "https://x/", t0)
	RememberAccountUsage(ctx, "openai-codex", older, "", "https://x", t0.Add(-time.Second))
	if CachedAccountUsage(ctx, "OpenAI-Codex", "", "https://x") != newer || CachedAccountUsage(ctx, "openai-codex", "codex:a:b", "") != newer {
		t.Fatal("a late result of an older fetch must not replace a fresher one")
	}
	RememberAccountUsage(ctx, "openai-codex", NewAccountUsageSnapshot("openai-codex", "x", nil, nil), "", "", time.Time{})
	if CachedAccountUsage(ctx, "openai-codex", "", "") != nil {
		t.Fatal("an empty fetch is never stored")
	}
	other := home.WithOverride(ctx, t.TempDir())
	if CachedAccountUsage(other, "openai-codex", "codex:a:b", "") != nil {
		t.Fatal("slots are per profile home")
	}
	stale := *newer
	stale.FetchedAt = time.Now().Add(-16 * time.Minute)
	future := *newer
	future.FetchedAt = time.Now().Add(time.Minute)
	if AccountUsageSnapshotIsStale(newer) || !AccountUsageSnapshotIsStale(&stale) || !AccountUsageSnapshotIsStale(&future) || !AccountUsageSnapshotIsStale(nil) {
		t.Fatal("staleness")
	}
	fetches := 0
	prev := accountUsageFetchers
	accountUsageFetchers = map[string]accountUsageFetcher{"openrouter": func(context.Context, string, string) (*AccountUsageSnapshot, error) {
		fetches++
		return nil, errors.New("down")
	}}
	t.Cleanup(func() { accountUsageFetchers = prev })
	for _, done := range RefreshAccountUsageAsync(ctx, []string{"openrouter", "openrouter"}) {
		<-done
	}
	if again := RefreshAccountUsageAsync(ctx, []string{"openrouter"}); len(again) != 0 || fetches != 1 {
		t.Fatalf("a failed refresh still counts as a try, fetches %d", fetches)
	}
	reqs := []AccountUsageRefreshRequest{{Provider: "OpenRouter", IdentityID: "fp:1", APIKey: "k"}, {Provider: "openrouter", IdentityID: "fp:2"},
		{Provider: "openrouter", IdentityID: "fp:3", APIKey: "k3"}}
	workers := RefreshAccountUsageEntriesAsync(ctx, reqs)
	if len(workers) != 1 {
		t.Fatalf("one worker for every due entry, got %d", len(workers))
	}
	<-workers[0]
	if fetches != 3 || len(RefreshAccountUsageEntriesAsync(ctx, reqs)) != 0 {
		t.Fatalf("fetches %d", fetches)
	}
}

func TestAccountUsageIdentityIDForPoolEntries(t *testing.T) {
	codex := PooledCredential{Provider: "openai-codex", ID: "e1", AccessToken: codexAccessJWT("acct", "user")}
	if got := AccountUsageIdentityID("openai-codex", codex); got != "codex:acct:user" {
		t.Fatalf("codex %q", got)
	}
	if got := AccountUsageIdentityID("openrouter", PooledCredential{Provider: "openrouter", ID: "e2", AccessToken: "tok"}); got != "fp:1a7674eb4ee78df7" {
		t.Fatalf("fingerprint %q", got)
	}
	if got := AccountUsageIdentityID("openrouter", PooledCredential{Provider: "openrouter", ID: "e3"}); got != "entry:e3" {
		t.Fatalf("metadata row %q", got)
	}
}

func TestAccountUsageRenderLinesAndPythonJSON(t *testing.T) {
	prevNow := accountUsageNow
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	accountUsageNow = func() time.Time { return now }
	t.Cleanup(func() { accountUsageNow = prevNow })
	reset := now.Add(26*time.Hour + 30*time.Minute)
	snap := NewAccountUsageSnapshot("openai-codex", "usage_api", []AccountUsageWindow{
		accountWindow("Session", usageF64(20.5), &reset, ""), accountWindow("Weekly", nil, nil, "x"), accountWindow("Quota", usageF64(-5), nil, "detail")},
		[]string{"extra"})
	snap.Plan, snap.UnavailableReason = "Plus", "why"
	lines := RenderAccountUsageLines(snap, false)
	stamp := reset.Local().Format("2006-01-02 15:04 MST")
	want := []string{"📈 Account limits", "Provider: openai-codex (Plus)", "Session: 80% remaining (20% used) • resets in 1d 2h (" + stamp + ")",
		"Weekly: unavailable • x", "Quota: 105% remaining (0% used) • detail", "extra", "Unavailable: why"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines %q", lines)
	}
	if RenderAccountUsageLines(nil, false) == nil || len(RenderAccountUsageLines(nil, false)) != 0 {
		t.Fatal("nil renders no lines")
	}
	_, repr, err := pyJSONLoads([]byte(`{"a": "it's", "b": {"x": -0, "y": 1e400, "z": NaN}, "a": "\ud800"}`))
	if err != nil || repr != `{'a': '\ud800', 'b': {'x': 0, 'y': inf, 'z': nan}}` {
		t.Fatalf("repr %q %v", repr, err)
	}
	for _, bad := range []string{"\xef\xbb\xbf{}", "{} x", "{'a': 1}", ""} {
		if _, _, err := pyJSONLoads([]byte(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if f, err := auFloat(" 1_5.5 "); err != nil || f != 15.5 {
		t.Fatalf("float %v %v", f, err)
	}
	if _, err := auFloat("0x10"); err == nil {
		t.Fatal("hex is not a Python float")
	}
}
