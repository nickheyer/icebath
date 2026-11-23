package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Isolated home whose Codex CLI file is absent and whose sleeps and probe cache are reset
func codexTestEnv(t *testing.T) (string, PoolEnv) {
	t.Helper()
	h := newTestHome(t)
	t.Setenv("CODEX_HOME", filepath.Join(filepath.Dir(h), "missing-codex"))
	resetCodexTestSeams(t)
	return h, testEnv(t, h)
}

func resetCodexTestSeams(t *testing.T) {
	t.Helper()
	prevSleep, prevClient, prevRefresh, prevProbe := oauthSleep, codexHTTPClientFn, codexRefreshPureFn, probeCodexQuotaRestoredFn
	oauthSleep = func(time.Duration) {}
	codexQuotaProbeMu.Lock()
	codexQuotaProbeCache = map[string]codexProbeResult{}
	codexQuotaProbeMu.Unlock()
	t.Cleanup(func() {
		oauthSleep, codexHTTPClientFn, codexRefreshPureFn, probeCodexQuotaRestoredFn = prevSleep, prevClient, prevRefresh, prevProbe
		codexQuotaProbeMu.Lock()
		codexQuotaProbeCache = map[string]codexProbeResult{}
		codexQuotaProbeMu.Unlock()
	})
}

// scriptedStep is one scripted transport answer, an error or a JSON response
type scriptedStep struct {
	err     error
	status  int
	payload any
	headers map[string]string
}

// scriptedTransport pops one step per request
type scriptedTransport struct {
	mu    sync.Mutex
	steps []scriptedStep
	calls int
	seen  []*http.Request
}

func (s *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.seen = append(s.seen, req)
	if len(s.steps) == 0 {
		return nil, errors.New("scripted transport exhausted")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	if step.err != nil {
		return nil, step.err
	}
	return jsonResponse(req, step.status, step.payload, step.headers), nil
}

func useCodexTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	codexHTTPClientFn = func(timeout time.Duration) *http.Client { return &http.Client{Transport: rt, Timeout: timeout} }
}

// Transport error shaped like a dropped TLS handshake
type sslEOFError struct{ msg string }

func (e *sslEOFError) Error() string   { return e.msg }
func (e *sslEOFError) Timeout() bool   { return false }
func (e *sslEOFError) Temporary() bool { return true }

const sslEOFMessage = "[SSL: UNEXPECTED_EOF_WHILE_READING] EOF occurred in violation of protocol (_ssl.c:1016)"

type timeoutError struct{}

func (timeoutError) Error() string   { return "timed out" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// auth.json holding one ChatGPT login of openai-codex with the given tokens, the row a device code sign in stores
func codexLoginStore(tokens map[string]any) map[string]any {
	row := map[string]any{"id": "codex-login", "label": "chatgpt", "auth_type": "oauth", "priority": 0, "source": "manual:device_code", "last_refresh": "2026-02-26T00:00:00Z"}
	for k, v := range tokens {
		row[k] = v
	}
	return map[string]any{"version": 2, "providers": map[string]any{}, "credential_pool": map[string]any{"openai-codex": []any{row}}}
}

func authErrorOf(t *testing.T, err error) *AuthError {
	t.Helper()
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected AuthError, got %v", err)
	}
	return authErr
}

func TestResolveCodexLoginWithoutAnAccessTokenAsksForASignIn(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, codexLoginStore(map[string]any{"access_token": "", "refresh_token": "refresh"}))
	_, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	authErr := authErrorOf(t, err)
	if authErr.Code != "codex_auth_missing" || !authErr.ReloginRequired {
		t.Fatalf("unexpected error %+v", authErr)
	}
}

func TestResolveCodexUsesTheStoredLogin(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{
		"version": 2, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"source": "manual:device_code", "access_token": "pool-fallback-token", "refresh_token": "pool-refresh", "last_status": "ok", "auth_type": "oauth"}}},
	})
	resolved, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved["api_key"] != "pool-fallback-token" || resolved["source"] != "credential_pool" || anyString(resolved["base_url"]) == "" {
		t.Fatalf("unexpected result %v", resolved)
	}
}

func TestRefresh429ClassifiedAsQuotaNotAuthFailure(t *testing.T) {
	_, _ = codexTestEnv(t)
	useCodexTransport(t, &scriptedTransport{steps: []scriptedStep{{status: 429,
		payload: map[string]any{"error": map[string]any{"message": "You hit your usage limit.", "code": "usage_limit_reached"}},
		headers: map[string]string{"retry-after": "120"}}}})
	_, err := refreshCodexOAuthPure(context.Background(), "a-tok", "r-tok", 20*time.Second)
	authErr := authErrorOf(t, err)
	if authErr.Code != CodexRateLimitedCode || authErr.ReloginRequired || !isRateLimitedAuthError(authErr) {
		t.Fatalf("unexpected error %+v", authErr)
	}
	if !strings.Contains(authErr.Message, "retry after 120s") {
		t.Fatalf("retry after missing: %s", authErr.Message)
	}
	rendered := formatAuthError(authErr)
	if strings.Contains(rendered, "re-authenticate") || strings.Contains(rendered, "icebath auth") {
		t.Fatalf("rate limit copy tells the user to re-authenticate: %s", rendered)
	}
}

func TestRefresh429WithoutRetryAfterHeader(t *testing.T) {
	_, _ = codexTestEnv(t)
	useCodexTransport(t, &scriptedTransport{steps: []scriptedStep{{status: 429, payload: map[string]any{"error": "rate_limited"}}}})
	_, err := refreshCodexOAuthPure(context.Background(), "a-tok", "r-tok", 20*time.Second)
	authErr := authErrorOf(t, err)
	if authErr.Code != CodexRateLimitedCode || authErr.ReloginRequired || authErr.HasRetryAfter {
		t.Fatalf("unexpected error %+v", authErr)
	}
}

func TestPoolOnlyForceRefreshRotatesThePoolEntry(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{
		"version": 2, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"source": "manual:device_code", "access_token": "pool-revoked", "refresh_token": "pool-refresh", "last_status": "ok", "auth_type": "oauth"}}},
	})
	var hints []string
	prev := oauthRefreshProviders["openai-codex"]
	spec := prev
	spec.refresh = pairRefresh(func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		hints = append(hints, accessToken)
		return map[string]any{"access_token": "pool-fresh", "refresh_token": "pool-refresh-2", "last_refresh": utcNowZ()}, nil
	})
	oauthRefreshProviders["openai-codex"] = spec
	t.Cleanup(func() { oauthRefreshProviders["openai-codex"] = prev })
	resolved, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{ForceRefresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if resolved["api_key"] != "pool-fresh" || resolved["source"] != "credential_pool" {
		t.Fatalf("unexpected result %v", resolved)
	}
	if len(hints) != 1 || hints[0] != "pool-revoked" {
		t.Fatalf("refresh hints %v", hints)
	}
}

func codexAccountJWT(accountID, sub string) string {
	return jwtWithClaims(map[string]any{"sub": sub, "exp": time.Now().Unix() + 3600, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID}})
}

// ---- bounded body ----

func TestOversized200AuthBodyIsRejectedBeforeBeingParsed(t *testing.T) {
	_, _ = codexTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := json.Marshal(map[string]any{"access_token": "at", "refresh_token": "rt", "pad": strings.Repeat("a", 3*1024*1024)})
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	prevURL := codexOAuthTokenURL
	codexOAuthTokenURL = server.URL + "/oauth/token"
	t.Cleanup(func() { codexOAuthTokenURL = prevURL })
	_, err := refreshCodexOAuthPure(context.Background(), "old-at", "old-rt", 20*time.Second)
	authErr := authErrorOf(t, err)
	if authErr.Code != "codex_auth_response_too_large" || !strings.Contains(authErr.Message, "exceeded 1024 KiB") {
		t.Fatalf("unexpected error %+v", authErr)
	}
}

func TestNormalAuthBodyStillParses(t *testing.T) {
	_, _ = codexTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token": "at-new", "refresh_token": "rt-new"}`))
	}))
	defer server.Close()
	prevURL := codexOAuthTokenURL
	codexOAuthTokenURL = server.URL + "/oauth/token"
	t.Cleanup(func() { codexOAuthTokenURL = prevURL })
	refreshed, err := refreshCodexOAuthPure(context.Background(), "old-at", "old-rt", 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed["access_token"] != "at-new" || refreshed["refresh_token"] != "rt-new" {
		t.Fatalf("unexpected tokens %v", refreshed)
	}
}

// ---- ssl hint and transient retry ----

func loginPostForTest() (*oauthHTTPResponse, error) {
	return codexLoginPost(context.Background(), "https://auth.openai.com/api/accounts/deviceauth/usercode", []byte(`{}`),
		map[string]string{"Content-Type": "application/json"}, [2]string{"Failed to request device code", "device_code_request_failed"})
}

func pollForTest() (map[string]any, error) {
	return codexPollAuthorizationCode(context.Background(), io.Discard, "https://auth.openai.com", "da", "uc", 0)
}

func TestSSLTransportErrorKeepsDetailAndAddsHint(t *testing.T) {
	_, _ = codexTestEnv(t)
	exc := &sslEOFError{msg: sslEOFMessage}
	cases := []struct {
		name string
		call func() error
		code string
	}{
		{"login_post", func() error { _, err := loginPostForTest(); return err }, "device_code_request_failed"},
		{"poll", func() error { _, err := pollForTest(); return err }, "device_code_poll_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := make([]scriptedStep, 0, 8)
			for i := 0; i < 8; i++ {
				steps = append(steps, scriptedStep{err: exc})
			}
			useCodexTransport(t, &scriptedTransport{steps: steps})
			err := tc.call()
			authErr := authErrorOf(t, err)
			if authErr.Code != tc.code {
				t.Fatalf("code %s", authErr.Code)
			}
			if !strings.Contains(authErr.Message, "UNEXPECTED_EOF_WHILE_READING") || !strings.Contains(authErr.Message, "GODEBUG=tlsmlkem=0") {
				t.Fatalf("message lost detail or hint: %s", authErr.Message)
			}
			if !errors.Is(err, exc) {
				t.Fatal("cause not chained")
			}
		})
	}
}

func TestPlainTimeoutHasNoSSLHint(t *testing.T) {
	_, _ = codexTestEnv(t)
	useCodexTransport(t, &scriptedTransport{steps: []scriptedStep{{err: timeoutError{}}, {err: timeoutError{}}, {err: timeoutError{}}}})
	_, err := loginPostForTest()
	authErr := authErrorOf(t, err)
	if !strings.Contains(authErr.Message, "timed out") || strings.Contains(authErr.Message, "GODEBUG") {
		t.Fatalf("unexpected message %s", authErr.Message)
	}
}

func TestPollSurvivesTransportBlipsButNotOtherErrors(t *testing.T) {
	_, _ = codexTestEnv(t)
	approval := scriptedStep{status: 200, payload: map[string]any{"authorization_code": "ac", "code_verifier": "cv"}}
	transport := &scriptedTransport{steps: []scriptedStep{{err: &sslEOFError{msg: sslEOFMessage}}, {err: &net.OpError{Op: "read", Err: errors.New("connection reset")}}, approval}}
	useCodexTransport(t, transport)
	got, err := pollForTest()
	if err != nil {
		t.Fatal(err)
	}
	if got["authorization_code"] != "ac" || got["code_verifier"] != "cv" || transport.calls != 3 {
		t.Fatalf("unexpected result %v after %d calls", got, transport.calls)
	}
	bug := errors.New("decode bug")
	transport = &scriptedTransport{steps: []scriptedStep{{err: bug}, approval}}
	useCodexTransport(t, transport)
	_, err = pollForTest()
	if !errors.Is(err, bug) {
		t.Fatalf("cause not chained: %v", err)
	}
	if transport.calls != 1 {
		t.Fatalf("a non transport error was retried %d times", transport.calls)
	}
}

func TestLoginPostRetriesBoundedAndKeepsTypedFailure(t *testing.T) {
	_, _ = codexTestEnv(t)
	transport := &scriptedTransport{steps: []scriptedStep{{err: &sslEOFError{msg: sslEOFMessage}}, {status: 200, payload: map[string]any{"user_code": "uc"}}}}
	useCodexTransport(t, transport)
	resp, err := loginPostForTest()
	if err != nil || resp.Status != 200 || transport.calls != 2 {
		t.Fatalf("resp %v err %v calls %d", resp, err, transport.calls)
	}
	exc := &sslEOFError{msg: sslEOFMessage}
	transport = &scriptedTransport{steps: []scriptedStep{{err: exc}, {err: exc}, {err: exc}, {err: exc}}}
	useCodexTransport(t, transport)
	_, err = loginPostForTest()
	authErr := authErrorOf(t, err)
	if authErr.Code != "device_code_request_failed" || !strings.Contains(authErr.Message, "UNEXPECTED_EOF_WHILE_READING") || !strings.Contains(authErr.Message, "GODEBUG=tlsmlkem=0") {
		t.Fatalf("unexpected error %+v", authErr)
	}
	if !errors.Is(err, exc) || transport.calls != 3 {
		t.Fatalf("cause %v calls %d", errors.Is(err, exc), transport.calls)
	}
}

// ---- quota probe ----

// usageTransport answers the usage endpoint, an expired bearer getting 401 when expiry aware
type usageTransport struct {
	mu          sync.Mutex
	payload     map[string]any
	status      int
	expiryAware bool
	calls       []*http.Request
}

func (u *usageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, req)
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if u.expiryAware && codexAccessTokenIsExpiring(token, 0) {
		return jsonResponse(req, 401, map[string]any{"error": map[string]any{"code": "token_expired"}}, nil), nil
	}
	status := u.status
	if status == 0 {
		status = 200
	}
	return jsonResponse(req, status, u.payload, nil), nil
}

func (u *usageTransport) authorizations() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := []string{}
	for _, r := range u.calls {
		out = append(out, r.Header.Get("Authorization"))
	}
	return out
}

func usagePayload(primary, secondary float64) map[string]any {
	return map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": primary}, "secondary_window": map[string]any{"used_percent": secondary}}}
}

func TestProbeSendsChatGPTAccountIDFromJWT(t *testing.T) {
	_, env := codexTestEnv(t)
	transport := &usageTransport{payload: usagePayload(0, 0)}
	useCodexTransport(t, transport)
	token := jwtWithClaims(map[string]any{"exp": time.Now().Unix() + 3600, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-123"}})
	restored, known := probeCodexQuotaRestored(env, token, "")
	if !known || !restored {
		t.Fatalf("restored=%v known=%v", restored, known)
	}
	if got := transport.calls[0].Header.Get("ChatGPT-Account-ID"); got != "acct-123" {
		t.Fatalf("account header %q", got)
	}
}

func TestProbeCountsAdditionalRateLimits(t *testing.T) {
	_, env := codexTestEnv(t)
	payload := usagePayload(0, 0)
	payload["additional_rate_limits"] = []any{map[string]any{"limit_name": "codex_model_scoped", "rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 100.0}}}}
	useCodexTransport(t, &usageTransport{payload: payload})
	restored, known := probeCodexQuotaRestored(env, jwtWithClaims(map[string]any{"exp": time.Now().Unix() + 3600}), "")
	if !known || restored {
		t.Fatalf("restored=%v known=%v", restored, known)
	}
}

func poolOnlyRateLimitedStore(now float64) map[string]any {
	return map[string]any{
		"version": 2, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"id": "cred-quota", "label": "quota-frozen", "auth_type": "oauth", "priority": 0, "source": "manual:device_code",
			"access_token": "tok-quota", "last_status": "exhausted", "last_status_at": now, "last_error_code": 429,
			"last_error_reason": "usage_limit_reached", "last_error_message": "The usage limit has been reached",
			"last_error_reset_at": now + 3*24*3600}}},
	}
}

func firstCodexPoolEntry(store map[string]any) map[string]any {
	pool := store["credential_pool"].(map[string]any)
	return pool["openai-codex"].([]any)[0].(map[string]any)
}

func TestResolverRecoversWhenProbeConfirmsReset(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, poolOnlyRateLimitedStore(nowEpoch()))
	probeCodexQuotaRestoredFn = func(PoolEnv, string, string) (bool, bool) { return true, true }
	resolved, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved["api_key"] != "tok-quota" || resolved["source"] != "credential_pool" {
		t.Fatalf("unexpected result %v", resolved)
	}
	entry := poolRows(t, h, "openai-codex")[0]
	if entry["last_status"] != nil || entry["last_error_reset_at"] != nil {
		t.Fatalf("cooldown not cleared: %v", entry)
	}
}

func expiredJWTPoolStore(now float64) map[string]any {
	store := poolOnlyRateLimitedStore(now)
	entry := firstCodexPoolEntry(store)
	entry["access_token"] = jwtWithClaims(map[string]any{"exp": now - 7200})
	entry["refresh_token"] = "rf-old"
	return store
}

func fakeCodexRefresh(fresh string, calls *[]string) {
	codexRefreshPureFn = func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		*calls = append(*calls, refreshToken)
		return map[string]any{"access_token": fresh, "refresh_token": "rf-new", "last_refresh": "now"}, nil
	}
}

func TestResolverRefreshesExpiredTokenBeforeProbe(t *testing.T) {
	h, env := codexTestEnv(t)
	now := nowEpoch()
	writeAuthStore(t, h, expiredJWTPoolStore(now))
	fresh := jwtWithClaims(map[string]any{"exp": now + 3600})
	var refreshCalls []string
	fakeCodexRefresh(fresh, &refreshCalls)
	transport := &usageTransport{payload: usagePayload(0, 0), expiryAware: true}
	useCodexTransport(t, transport)
	resolved, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshCalls) != 1 || refreshCalls[0] != "rf-old" {
		t.Fatalf("refresh calls %v", refreshCalls)
	}
	if auths := transport.authorizations(); len(auths) == 0 || auths[0] != "Bearer "+fresh {
		t.Fatalf("probe authorizations %v", auths)
	}
	if resolved["api_key"] != fresh {
		t.Fatalf("api_key %v", resolved["api_key"])
	}
	entry := poolRows(t, h, "openai-codex")[0]
	if entry["refresh_token"] != "rf-new" || entry["last_status"] != nil {
		t.Fatalf("entry %v", entry)
	}
}

func TestPoolSelectionRefreshesExpiredTokenBeforeProbe(t *testing.T) {
	h, env := codexTestEnv(t)
	now := nowEpoch()
	store := expiredJWTPoolStore(now)
	writeAuthStore(t, h, store)
	fresh := jwtWithClaims(map[string]any{"exp": now + 3600})
	var refreshCalls []string
	fakeCodexRefresh(fresh, &refreshCalls)
	transport := &usageTransport{payload: usagePayload(0, 100), expiryAware: true}
	useCodexTransport(t, transport)
	pool := LoadPool(env, "openai-codex")
	if _, ok := pool.Select(""); ok {
		t.Fatal("exhausted entry selected")
	}
	if _, ok := pool.Select(""); ok {
		t.Fatal("exhausted entry selected on the second pass")
	}
	if len(refreshCalls) != 1 || refreshCalls[0] != "rf-old" {
		t.Fatalf("refresh calls %v", refreshCalls)
	}
	if auths := transport.authorizations(); len(auths) != 1 || auths[0] != "Bearer "+fresh {
		t.Fatalf("probe authorizations %v", auths)
	}
	entry := pool.Entries()[0]
	if entry.AccessToken != fresh || entry.RefreshToken != "rf-new" || entry.LastStatus != StatusExhausted {
		t.Fatalf("entry %+v", entry)
	}
	if poolRows(t, h, "openai-codex")[0]["refresh_token"] != "rf-new" {
		t.Fatalf("disk pool row %v", poolRows(t, h, "openai-codex")[0])
	}
}

func TestPoolSelectionKeepsProbeRotatedTokensWhenQuotaRestored(t *testing.T) {
	h, env := codexTestEnv(t)
	now := nowEpoch()
	store := expiredJWTPoolStore(now)
	firstCodexPoolEntry(store)["source"] = "manual:device_code"
	writeAuthStore(t, h, store)
	fresh := jwtWithClaims(map[string]any{"exp": now + 3600})
	var refreshCalls []string
	fakeCodexRefresh(fresh, &refreshCalls)
	transport := &usageTransport{payload: usagePayload(0, 0), expiryAware: true}
	useCodexTransport(t, transport)
	pool := LoadPool(env, "openai-codex")
	selected, ok := pool.Select("")
	if !ok {
		t.Fatal("restored entry not selected")
	}
	if len(refreshCalls) != 1 || refreshCalls[0] != "rf-old" {
		t.Fatalf("refresh calls %v", refreshCalls)
	}
	if auths := transport.authorizations(); len(auths) != 1 || auths[0] != "Bearer "+fresh {
		t.Fatalf("probe authorizations %v", auths)
	}
	if selected.AccessToken != fresh || selected.RefreshToken != "rf-new" || selected.LastStatus != StatusOK {
		t.Fatalf("selected %+v", selected)
	}
	if pool.Entries()[0].RefreshToken != "rf-new" {
		t.Fatal("the pool's own row kept the consumed refresh token")
	}
	if poolRows(t, h, "openai-codex")[0]["refresh_token"] != "rf-new" {
		t.Fatal("disk row kept the consumed refresh token")
	}
}

func TestPoolSelectionThrottlesFailingPreProbeRefresh(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, expiredJWTPoolStore(nowEpoch()))
	var attempts []string
	codexRefreshPureFn = func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		attempts = append(attempts, refreshToken)
		return nil, errors.New("invalid_grant")
	}
	transport := &usageTransport{payload: usagePayload(0, 0), expiryAware: true}
	useCodexTransport(t, transport)
	pool := LoadPool(env, "openai-codex")
	for i := 0; i < 5; i++ {
		if _, ok := pool.Select(""); ok {
			t.Fatal("frozen entry selected")
		}
	}
	if len(attempts) != 1 || attempts[0] != "rf-old" {
		t.Fatalf("refresh attempts %v", attempts)
	}
	if len(transport.authorizations()) > 1 {
		t.Fatalf("probe fired %d times", len(transport.authorizations()))
	}
}

func TestPoolProbeNotFiredForNonQuotaExhaustion(t *testing.T) {
	h, env := codexTestEnv(t)
	store := poolOnlyRateLimitedStore(nowEpoch())
	entry := firstCodexPoolEntry(store)
	entry["last_error_code"] = 401
	entry["last_error_reason"] = "token_expired"
	entry["last_error_message"] = "expired"
	entry["access_token"] = jwtWithClaims(map[string]any{"exp": nowEpoch() + 3600})
	writeAuthStore(t, h, store)
	transport := &usageTransport{payload: usagePayload(0, 0)}
	useCodexTransport(t, transport)
	pool := LoadPool(env, "openai-codex")
	if _, ok := pool.Select(""); ok {
		t.Fatal("auth frozen entry selected")
	}
	if len(transport.authorizations()) != 0 {
		t.Fatal("probe fired for an auth shaped exhaustion")
	}
}

// ---- write through ----

// Global root at tmp/.icebath with the active profile at tmp/.icebath/profiles/work
func profileEnvForTest(t *testing.T, name string) (string, string, PoolEnv) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	profile := filepath.Join(root, "profiles", name)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv(home.EnvHome, profile)
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "missing-codex"))
	for _, v := range seedableEnvVars {
		t.Setenv(v, "")
	}
	home.ResetKeyCache()
	config.ResetCache()
	resetCodexTestSeams(t)
	env := testEnv(t, profile)
	if env.Store.GlobalPath() != filepath.Join(root, "auth.json") {
		t.Fatalf("root not detected: %q", env.Store.GlobalPath())
	}
	return profile, root, env
}

func tokenPair(prefix string) map[string]any {
	return map[string]any{"access_token": prefix + "-at", "refresh_token": prefix + "-rt"}
}

func providerTokens(store map[string]any) map[string]any {
	providers, _ := store["providers"].(map[string]any)
	codex, _ := providers["openai-codex"].(map[string]any)
	tokens, _ := codex["tokens"].(map[string]any)
	return tokens
}

// Token endpoint that rotates old-rt once and rejects any replay of a consumed token
type rotatingEndpoint struct {
	mu   sync.Mutex
	hold time.Duration
	seen []string
}

func (r *rotatingEndpoint) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(body))
	rt := form.Get("refresh_token")
	r.mu.Lock()
	replay := false
	for _, s := range r.seen {
		if s == rt {
			replay = true
		}
	}
	r.seen = append(r.seen, rt)
	r.mu.Unlock()
	time.Sleep(r.hold)
	if replay {
		return jsonResponse(req, 400, map[string]any{"error": "refresh_token_reused"}, nil), nil
	}
	return jsonResponse(req, 200, map[string]any{"access_token": "new-at", "refresh_token": "new-rt"}, nil), nil
}

// ---- relogin copy names the profile ----

func TestFormatAuthErrorReloginSuffixNamesTheProfile(t *testing.T) {
	profileEnvForTest(t, "codex")
	rendered := formatAuthError(codexErr("Codex token refresh failed: invalid_grant", "invalid_grant", true))
	if !strings.Contains(rendered, "`icebath -p codex model`") || strings.Contains(rendered, "`icebath model`") {
		t.Fatalf("suffix does not name the profile: %s", rendered)
	}
}

// ---- credential host binding ----

const (
	testGW      = "https://codex-gw.example/backend-api/codex"
	testOtherGW = "https://other-gw.example/backend-api/codex"
)

func exhaustedJWTPool(t *testing.T, h string) {
	t.Helper()
	now := nowEpoch()
	writeAuthStore(t, h, map[string]any{
		"version": 2, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"id": "gw", "label": "gateway", "auth_type": "oauth", "priority": 0, "source": "manual:device_code",
			"access_token": codexAccountJWT("acct", "acct"), "base_url": DefaultCodexBaseURL,
			"last_status": "exhausted", "last_status_at": now, "last_error_code": 429,
			"last_error_reason": "usage_limit_reached", "last_error_message": "The usage limit has been reached",
			"last_error_reset_at": now + 3*24*3600}}},
	})
}

func authorizedHosts(transport *usageTransport) map[string]bool {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	out := map[string]bool{}
	for _, r := range transport.calls {
		if r.Header.Get("Authorization") != "" {
			out[r.URL.Hostname()] = true
		}
	}
	return out
}

func TestQuotaRestoredProbeOfAPersistedEntryAsksTheGateway(t *testing.T) {
	h, env := codexTestEnv(t)
	writeConfigYAML(t, h, "model:\n  provider: openai-codex\n  default: gpt-5.5\n  base_url: "+testGW+"\n")
	exhaustedJWTPool(t, h)
	transport := &usageTransport{payload: map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10}}}}
	useCodexTransport(t, transport)
	entry := poolRows(t, h, "openai-codex")[0]
	restored, known := probeCodexPoolEntryQuotaRestored(env, entry)
	if !known || !restored {
		t.Fatalf("restored=%v known=%v", restored, known)
	}
	if hosts := authorizedHosts(transport); len(hosts) != 1 || !hosts["codex-gw.example"] {
		t.Fatalf("authorized hosts %v", hosts)
	}
}

func TestPoolSelectionQuotaProbeAsksTheGateway(t *testing.T) {
	h, env := codexTestEnv(t)
	writeConfigYAML(t, h, "model:\n  provider: openai-codex\n  default: gpt-5.5\n  base_url: "+testGW+"\n")
	exhaustedJWTPool(t, h)
	transport := &usageTransport{payload: map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10}}}}
	useCodexTransport(t, transport)
	pool := LoadPool(env, "openai-codex")
	entries := pool.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries %d", len(entries))
	}
	if !pool.codexQuotaRestoredUpstream(entries[0], false) {
		t.Fatal("quota not restored")
	}
	if hosts := authorizedHosts(transport); len(hosts) != 1 || !hosts["codex-gw.example"] {
		t.Fatalf("authorized hosts %v", hosts)
	}
}

func TestQuotaRestoredProbeDirectChatGPTPositiveControl(t *testing.T) {
	h, env := codexTestEnv(t)
	writeConfigYAML(t, h, "model:\n  provider: openai-codex\n  default: gpt-5.5\n")
	exhaustedJWTPool(t, h)
	transport := &usageTransport{payload: map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10}}}}
	useCodexTransport(t, transport)
	probeCodexPoolEntryQuotaRestored(env, poolRows(t, h, "openai-codex")[0])
	if hosts := authorizedHosts(transport); len(hosts) != 1 || !hosts["chatgpt.com"] {
		t.Fatalf("authorized hosts %v", hosts)
	}
}

func TestRouteFallbackReadsTheProfileScopedOverrideNotASiblingProcessEnv(t *testing.T) {
	h, env := codexTestEnv(t)
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	t.Setenv("ICEBATH_CODEX_BASE_URL", testOtherGW)
	for _, tc := range []struct {
		scope    map[string]string
		expected string
	}{
		{map[string]string{}, testGW},
		{map[string]string{"ICEBATH_CODEX_BASE_URL": testOtherGW + "/"}, testOtherGW},
	} {
		scoped := env
		scoped.Ctx = WithSecretScope(context.Background(), tc.scope, h)
		scoped.Scope = SecretScopeFrom(scoped.Ctx)
		if got := codexPoolRouteBaseURL(scoped, testGW); got != tc.expected {
			t.Fatalf("scope %v: got %s want %s", tc.scope, got, tc.expected)
		}
	}
}

func TestUsageProbeURLPathStyle(t *testing.T) {
	_, env := codexTestEnv(t)
	if got := codexUsageProbeURL(env, "https://chatgpt.com/backend-api/codex"); got != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("chatgpt path %s", got)
	}
	if got := codexUsageProbeURL(env, "https://gw.example/v1/codex/"); got != "https://gw.example/v1/api/codex/usage" {
		t.Fatalf("gateway path %s", got)
	}
}

func TestCodexRefreshFailureErrorShapes(t *testing.T) {
	_, _ = codexTestEnv(t)
	nested := codexRefreshFailureError(&oauthHTTPResponse{Status: 400, Body: []byte(`{"error": {"code": "invalid_grant", "message": "grant dead"}}`)})
	if nested.Code != "invalid_grant" || !nested.ReloginRequired || nested.Message != "Codex token refresh failed: grant dead" {
		t.Fatalf("nested %+v", nested)
	}
	forbidden := codexRefreshFailureError(&oauthHTTPResponse{Status: 403, Body: []byte(`not json`)})
	if forbidden.Code != "codex_refresh_failed" || !forbidden.ReloginRequired || forbidden.Message != "Codex token refresh failed with status 403." {
		t.Fatalf("forbidden %+v", forbidden)
	}
	server := codexRefreshFailureError(&oauthHTTPResponse{Status: 503, Body: []byte(`{}`)})
	if server.ReloginRequired || !isTerminalCodexOAuthRefreshError(codexErr("x", "codex_refresh_failed", true)) || isTerminalCodexOAuthRefreshError(server) {
		t.Fatalf("server %+v", server)
	}
}

func TestReadOnlyResolverNeverProbesOrMutatesAnExhaustedPool(t *testing.T) {
	h, env := codexTestEnv(t)
	entry := map[string]any{"id": "codex-pool-entry-0", "label": "device_code-0", "auth_type": "oauth", "source": "manual:device_code", "priority": 0,
		"request_count": 0, "access_token": jwtWithClaims(map[string]any{"exp": nowEpoch() - 3600}), "refresh_token": "codex-refresh-token-0",
		"base_url": DefaultCodexBaseURL, "last_status": "exhausted", "last_error_code": 429, "last_error_reason": "usage_limit_reached",
		"last_error_reset_at": nowEpoch() + 3600}
	writeAuthStore(t, h, map[string]any{"version": 2, "credential_pool": map[string]any{"openai-codex": []any{entry}}})
	before, _ := os.ReadFile(filepath.Join(h, "auth.json"))
	transport := &usageTransport{payload: usagePayload(0, 0)}
	useCodexTransport(t, transport)
	var refreshCalls []string
	fakeCodexRefresh(jwtWithClaims(map[string]any{"exp": nowEpoch() + 3600}), &refreshCalls)
	if _, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{ReadOnly: true}); err == nil {
		t.Fatal("expected the cooldown error")
	}
	if len(transport.calls) != 0 || len(refreshCalls) != 0 {
		t.Fatal("a read only resolve probed or refreshed")
	}
	after, _ := os.ReadFile(filepath.Join(h, "auth.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("auth.json rewritten")
	}
	if _, err := os.Stat(filepath.Join(h, "auth.lock")); err == nil {
		t.Fatal("a read only resolve materialised auth.lock")
	}
}

func TestSetCodexQuotaProbeSwapsAndRestoresTheProbe(t *testing.T) {
	restore := SetCodexQuotaProbe(func(PoolEnv, string, string) (bool, bool) { return true, true })
	restored, known := probeCodexQuotaRestoredFn(PoolEnv{}, "token", "https://chatgpt.com/backend-api/codex")
	restore()
	if !restored || !known {
		t.Fatalf("probe %v %v", restored, known)
	}
	if reflect.ValueOf(probeCodexQuotaRestoredFn).Pointer() != reflect.ValueOf(probeCodexQuotaRestored).Pointer() {
		t.Fatal("previous probe not restored")
	}
}

func TestResolveCodexRuntimeCredentialsExportedMatchesInternal(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{
		"version": 2, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"source": "manual:device_code", "access_token": "pool-fallback-token", "refresh_token": "pool-refresh", "last_status": "ok", "auth_type": "oauth"}}},
	})
	resolved, err := ResolveCodexRuntimeCredentials(env, CodexResolveOptions{SkipExpiringRefresh: true})
	if err != nil || resolved["api_key"] != "pool-fallback-token" || resolved["source"] != "credential_pool" {
		t.Fatalf("%v %v", resolved, err)
	}
}
