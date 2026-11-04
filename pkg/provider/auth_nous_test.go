package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- helpers ----

// Restores every nous seam and process level toggle after the test
func resetNousSeams(t *testing.T) {
	t.Helper()
	refresh, client, fromState, login := nousRefreshAccessTokenFn, nousHTTPClientFn, refreshNousOAuthFromStateFn, nousDeviceCodeLoginFn
	curated, pricing, free, policy := nousLoginCuratedIDsFn, nousLoginPricingFn, nousLoginFreeTierFn, nousLoginPolicyAllowedFn
	loadPool, resolve := nousKeepaliveLoadPoolFn, nousKeepaliveResolveFn
	recommended := fetchNousRecommendedModelsFn
	prompt := testPromptModelSelection
	guest := testGuestWithoutFreeTier
	entitlement := testNousEntitlementMessage
	t.Setenv("ICEBATH_SHARED_AUTH_DIR", filepath.Join(t.TempDir(), "shared"))
	t.Setenv("NOUS_INFERENCE_BASE_URL", "")
	t.Setenv("ICEBATH_PORTAL_BASE_URL", "")
	t.Setenv("NOUS_PORTAL_BASE_URL", "")
	os.Unsetenv("NOUS_INFERENCE_BASE_URL")
	os.Unsetenv("ICEBATH_PORTAL_BASE_URL")
	os.Unsetenv("NOUS_PORTAL_BASE_URL")
	InvalidateNousAuthStatusCache()
	resolveTokenCacheMu.Lock()
	resolveTokenCache = map[string]resolvedNousToken{}
	resolveTokenCacheMu.Unlock()
	testBillingInvalidateCalls, testSettleAfterUpgradeCalls, testSaveModelChoiceCalls, testGuestHandoffDrainResult = 0, 0, nil, nil
	t.Cleanup(func() {
		nousRefreshAccessTokenFn, nousHTTPClientFn, refreshNousOAuthFromStateFn, nousDeviceCodeLoginFn = refresh, client, fromState, login
		nousLoginCuratedIDsFn, nousLoginPricingFn, nousLoginFreeTierFn, nousLoginPolicyAllowedFn = curated, pricing, free, policy
		nousKeepaliveLoadPoolFn, nousKeepaliveResolveFn = loadPool, resolve
		fetchNousRecommendedModelsFn = recommended
		testPromptModelSelection = prompt
		testGuestWithoutFreeTier = guest
		testNousEntitlementMessage = entitlement
		SetMultiplexActive(false)
		InvalidateNousAuthStatusCache()
	})
}

func nousJWT(seconds float64, scope any, sub string, extra map[string]any) string {
	claims := map[string]any{"sub": sub, "scope": scope, "exp": int64(time.Now().Unix()) + int64(seconds)}
	for k, v := range extra {
		claims[k] = v
	}
	return jwtWithClaims(claims)
}

func invokeJWT(seconds float64) string { return nousJWT(seconds, DefaultNousScope, "test-user", nil) }

func futureISO(seconds float64) string { return nousISO(time.Now().Add(time.Duration(seconds * float64(time.Second)))) }

type nousAuthSetup struct {
	accessToken, refreshToken, scope, expiresAt string
	expiresIn                                   int
	agentKey, agentKeyExpiresAt                 any
}

// Writes the auth.json the hermes fixture _setup_nous_auth writes
func setupNousAuth(t *testing.T, h string, s nousAuthSetup) {
	t.Helper()
	if s.scope == "" {
		s.scope = "inference:invoke"
	}
	if s.accessToken == "" {
		s.accessToken = nousJWT(3600, s.scope, "test-user", nil)
	}
	if s.expiresAt == "" {
		s.expiresAt = "2026-02-01T00:00:00+00:00"
	}
	writeAuthStore(t, h, map[string]any{
		"version": 1, "active_provider": "nous",
		"providers": map[string]any{"nous": map[string]any{
			"portal_base_url": "https://portal.example.com", "inference_base_url": "https://inference.example.com/v1",
			"client_id": "icebath-cli", "token_type": "Bearer", "scope": s.scope, "access_token": s.accessToken,
			"refresh_token": s.refreshToken, "obtained_at": "2026-02-01T00:00:00+00:00", "expires_in": s.expiresIn,
			"expires_at": s.expiresAt, "agent_key": s.agentKey, "agent_key_id": nil, "agent_key_expires_at": s.agentKeyExpiresAt,
			"agent_key_expires_in": nil, "agent_key_reused": nil, "agent_key_obtained_at": nil,
		}},
	})
}

func nousProviderState(t *testing.T, h string) map[string]any {
	t.Helper()
	providers, _ := readAuthStore(t, h)["providers"].(map[string]any)
	state, _ := providers["nous"].(map[string]any)
	return state
}

// One scripted HTTP answer
type nousStep struct {
	status  int
	payload any
	raw     []byte
	headers map[string]string
	err     error
}

type nousScriptedTransport struct {
	mu    sync.Mutex
	steps []nousStep
	seen  []*http.Request
	forms []string
}

func (s *nousScriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, req)
	form := ""
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		form = string(raw)
	}
	s.forms = append(s.forms, form)
	if len(s.steps) == 0 {
		return nil, errors.New("nous scripted transport exhausted")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	if step.err != nil {
		return nil, step.err
	}
	body := step.raw
	if body == nil {
		body, _ = json.Marshal(step.payload)
	}
	resp := &http.Response{StatusCode: step.status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}
	for k, v := range step.headers {
		resp.Header.Set(k, v)
	}
	return resp, nil
}

func useNousTransport(t *testing.T, steps ...nousStep) *nousScriptedTransport {
	t.Helper()
	rt := &nousScriptedTransport{steps: steps}
	nousHTTPClientFn = func(env PoolEnv, timeout time.Duration, verify oauthVerify) *http.Client {
		return &http.Client{Transport: rt, Timeout: timeout}
	}
	return rt
}

// Fake Portal refresh recording the refresh tokens it was handed
func fakeNousRefresh(posted *[]string, payload func(n int) map[string]any) {
	nousRefreshAccessTokenFn = func(ctx context.Context, client *http.Client, portalBaseURL, clientID, refreshToken string) (map[string]any, error) {
		*posted = append(*posted, refreshToken)
		return payload(len(*posted)), nil
	}
}

func recordingEnv(t *testing.T, h string) (PoolEnv, *recordingHandler) {
	t.Helper()
	rec := &recordingHandler{}
	env := testEnv(t, h)
	env.Log = newRecordingLogger(rec)
	return env, rec
}

func useDefaultLogger(t *testing.T, rec *recordingHandler) {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(newRecordingLogger(rec))
	t.Cleanup(func() { slog.SetDefault(previous) })
}

// ---- test_auth_nous_provider.py ----

func TestResolveNousRuntimeCredentialsPrefersInvokeJWTAndMirrors(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	token := invokeJWT(3600)
	setupNousAuth(t, h, nousAuthSetup{accessToken: token, scope: DefaultNousScope, expiresAt: futureISO(3600), expiresIn: 3600, refreshToken: "refresh-old"})
	env := testEnv(t, h)
	creds, err := resolveNousRuntimeCredentials(env, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if creds["api_key"] != token || creds["source"] != NousAuthPathInvokeJWT || creds["auth_path"] != NousAuthPathInvokeJWT {
		t.Fatalf("creds = %v", creds)
	}
	singleton := nousProviderState(t, h)
	if singleton["agent_key"] != token {
		t.Fatalf("agent_key = %v", singleton["agent_key"])
	}
	exp, ok := ParseISOTimestamp(singleton["agent_key_expires_at"])
	if !ok || exp <= nowEpoch()+300 {
		t.Fatalf("agent_key_expires_at = %v", singleton["agent_key_expires_at"])
	}
	rows := poolRows(t, h, "nous")
	if len(rows) != 1 || rows[0]["agent_key"] != token || rows[0]["source"] != NousDeviceCodeSource {
		t.Fatalf("pool rows = %v", rows)
	}
}

func TestResolveNousRuntimeCredentialsInvokeJWTIsIdempotent(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	exp := time.Now().Unix() + 3600
	expiresAt := nousEpochISO(float64(exp))
	token := jwtWithClaims(map[string]any{"sub": "test-user", "scope": DefaultNousScope, "exp": exp})
	originalObtainedAt := "2026-04-17T22:00:10+00:00"
	writeAuthStore(t, h, map[string]any{
		"version": 1, "active_provider": "nous",
		"providers": map[string]any{"nous": map[string]any{
			"portal_base_url": "https://portal.nousresearch.com", "inference_base_url": "https://inference-api.nousresearch.com/v1",
			"client_id": "icebath-cli", "token_type": "Bearer", "scope": DefaultNousScope, "access_token": token,
			"refresh_token": "refresh-token", "obtained_at": "2026-02-01T00:00:00+00:00", "expires_in": 123,
			"expires_at": expiresAt, "agent_key": token, "agent_key_id": nil, "agent_key_expires_at": expiresAt,
			"agent_key_expires_in": 123, "agent_key_reused": false, "agent_key_obtained_at": originalObtainedAt,
			"tls": map[string]any{"insecure": false, "ca_bundle": nil},
		}},
	})
	authPath := filepath.Join(h, "auth.json")
	before, _ := os.ReadFile(authPath)
	beforeStat, _ := os.Stat(authPath)
	env := testEnv(t, h)
	creds, err := resolveNousRuntimeCredentials(env, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if creds["api_key"] != token || creds["source"] != NousAuthPathInvokeJWT {
		t.Fatalf("creds = %v", creds)
	}
	after, _ := os.ReadFile(authPath)
	afterStat, _ := os.Stat(authPath)
	if string(after) != string(before) || !afterStat.ModTime().Equal(beforeStat.ModTime()) {
		t.Fatalf("auth.json rewritten for an unchanged invoke JWT resolution")
	}
	if _, err := os.Stat(nousSharedStorePath(env)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared store written for an unchanged resolution")
	}
	if nousProviderState(t, h)["agent_key_obtained_at"] != originalObtainedAt {
		t.Fatalf("agent_key_obtained_at changed")
	}
}

func TestResolveNousRuntimeCredentialsReauthsWhenInvokeScopeMissing(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	token := jwtWithClaims(map[string]any{"sub": "test-user", "scope": "inference:mint_agent_key", "exp": time.Now().Unix() + 3600})
	setupNousAuth(t, h, nousAuthSetup{accessToken: token, refreshToken: "", scope: "inference:mint_agent_key", expiresAt: futureISO(3600), expiresIn: 3600})
	_, err := resolveNousRuntimeCredentials(testEnv(t, h), false, "")
	authErr := authErrorOf(t, err)
	if authErr.Code != "nous_auth_missing_refresh_token" || !strings.Contains(authErr.Error(), "missing_inference_invoke_scope") || !authErr.ReloginRequired {
		t.Fatalf("err = %+v", authErr)
	}
	if nousProviderState(t, h)["agent_key"] != nil {
		t.Fatalf("agent_key persisted")
	}
	if rows := poolRows(t, h, "nous"); len(rows) != 0 {
		t.Fatalf("pool rows = %v", rows)
	}
}

func TestNousInferenceAuthLogsDoNotIncludeSecretValues(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	token := invokeJWT(3600)
	refreshedToken := invokeJWT(7200)
	refreshToken := "refresh-secret-token"
	setupNousAuth(t, h, nousAuthSetup{accessToken: token, refreshToken: refreshToken, scope: DefaultNousScope, expiresAt: futureISO(3600), expiresIn: 3600})
	nousRefreshAccessTokenFn = func(ctx context.Context, client *http.Client, portalBaseURL, clientID, refreshToken string) (map[string]any, error) {
		return map[string]any{"access_token": refreshedToken, "refresh_token": "refresh-new", "expires_in": 7200, "token_type": "Bearer", "scope": DefaultNousScope}, nil
	}
	env, rec := recordingEnv(t, h)
	levels := &levelRecorder{}
	env.Log = slog.New(levels)
	if _, err := resolveNousRuntimeCredentials(env, true, ""); err != nil {
		t.Fatal(err)
	}
	_ = rec
	logged := strings.Join(levels.messages, "\n")
	if !strings.Contains(logged, "using NAS invoke JWT") {
		t.Fatalf("invoke JWT selection not logged")
	}
	for _, m := range levels.infoOrAbove {
		if strings.Contains(m, "using NAS invoke JWT") {
			t.Fatalf("invoke JWT selection logged above debug")
		}
	}
	for _, secret := range []string{token, refreshedToken, refreshToken} {
		if strings.Contains(logged, secret) {
			t.Fatalf("secret leaked into logs")
		}
	}
}

// Records messages with their level
type levelRecorder struct {
	mu          sync.Mutex
	messages    []string
	infoOrAbove []string
}

func (l *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *levelRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.messages = append(l.messages, r.Message)
	if r.Level >= slog.LevelInfo {
		l.infoOrAbove = append(l.infoOrAbove, r.Message)
	}
	return nil
}
func (l *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *levelRecorder) WithGroup(string) slog.Handler      { return l }

func TestGetNousAuthStatusChecksCredentialPool(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	env := testEnv(t, h)
	pool := LoadPool(env, "nous")
	token := invokeJWT(3600)
	entry := PooledCredentialFromMap("nous", map[string]any{
		"access_token": token, "refresh_token": "test-refresh-token", "portal_base_url": "https://portal.example.com",
		"inference_base_url": "https://inference.example.com/v1", "agent_key": token, "agent_key_expires_at": futureISO(3600),
		"scope": "inference:invoke", "label": "dashboard device_code", "auth_type": "oauth",
		"source": "manual:dashboard_device_code", "base_url": "https://inference.example.com/v1",
	})
	if _, err := pool.AddEntry(entry); err != nil {
		t.Fatal(err)
	}
	status := GetNousAuthStatus(env)
	if status["logged_in"] != true || !strings.Contains(anyString(status["portal_base_url"]), "example.com") {
		t.Fatalf("status = %v", status)
	}
}

func TestGetNousAuthStatusEmptyReturnsNotLoggedIn(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	if status := GetNousAuthStatus(testEnv(t, h)); status["logged_in"] != false {
		t.Fatalf("status = %v", status)
	}
}

func fullNousStateFixture() map[string]any {
	token := invokeJWT(3600)
	expiresAt := futureISO(3600)
	return map[string]any{
		"portal_base_url": "https://portal.example.com", "inference_base_url": "https://inference.example.com/v1",
		"client_id": "icebath-cli", "scope": "inference:invoke", "token_type": "Bearer", "access_token": token,
		"refresh_token": "refresh-tok", "obtained_at": "2026-04-17T22:00:00+00:00", "expires_at": expiresAt,
		"expires_in": 3600, "agent_key": token, "agent_key_id": nil, "agent_key_expires_at": expiresAt,
		"agent_key_expires_in": 3600, "agent_key_reused": false, "agent_key_obtained_at": "2026-04-17T22:00:10+00:00",
		"tls": map[string]any{"insecure": false, "ca_bundle": nil},
	}
}

func TestPersistNousCredentialsIdempotentNoDuplicatePoolEntries(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	env := testEnv(t, h)
	if _, _, err := persistNousCredentials(env, fullNousStateFixture(), ""); err != nil {
		t.Fatal(err)
	}
	second := fullNousStateFixture()
	secondToken := invokeJWT(7200)
	second["access_token"], second["agent_key"], second["agent_key_expires_at"] = secondToken, secondToken, futureISO(7200)
	if _, _, err := persistNousCredentials(env, second, ""); err != nil {
		t.Fatal(err)
	}
	state := nousProviderState(t, h)
	if state["access_token"] != secondToken || state["agent_key"] != secondToken {
		t.Fatalf("singleton = %v", state)
	}
	rows := poolRows(t, h, "nous")
	if len(rows) != 1 || rows[0]["source"] != NousDeviceCodeSource || rows[0]["agent_key"] != secondToken {
		t.Fatalf("pool rows = %v", rows)
	}
}

func refreshClient(t *testing.T, steps ...nousStep) (*http.Client, *nousScriptedTransport) {
	t.Helper()
	rt := &nousScriptedTransport{steps: steps}
	return &http.Client{Transport: rt}, rt
}

func TestRefreshTokenReuseDetectionSurfacesActionableMessage(t *testing.T) {
	client, _ := refreshClient(t, nousStep{status: 400, payload: map[string]any{"error": "invalid_grant", "error_description": "Refresh token reuse detected; please re-authenticate"}})
	_, err := nousRefreshAccessToken(context.Background(), client, "https://portal.nousresearch.com", "icebath-cli", "rt_consumed_elsewhere")
	authErr := authErrorOf(t, err)
	if authErr.Code != "invalid_grant" || !authErr.ReloginRequired {
		t.Fatalf("err = %+v", authErr)
	}
	if !strings.Contains(authErr.Error(), "icebath auth add nous") || !strings.Contains(authErr.Error(), "single-use") {
		t.Fatalf("message = %q", authErr.Error())
	}
}

func TestRefreshTokenExchangeErrorClassification(t *testing.T) {
	notJSON := []byte("not json")
	cases := []struct {
		status   int
		payload  any
		raw      []byte
		headers  map[string]string
		code     string
		terminal bool
	}{
		{500, nil, []byte(""), nil, "temporarily_unavailable", false},
		{503, nil, []byte(""), nil, "temporarily_unavailable", false},
		{599, nil, []byte(""), nil, "temporarily_unavailable", false},
		{429, map[string]any{"code": "429", "message": "rate limited"}, nil, nil, "", false},
		{404, map[string]any{"message": "not found"}, nil, nil, "", false},
		{400, nil, notJSON, nil, "", false},
		{401, map[string]any{"message": "unauthorized"}, nil, nil, "invalid_grant", true},
		{403, nil, notJSON, nil, "invalid_grant", true},
		{400, []any{"not", "a", "dict"}, nil, nil, "", false},
		{401, "unauthorized", nil, nil, "invalid_grant", true},
		{403, nil, notJSON, map[string]string{"x-vercel-mitigated": "deny"}, "upstream_blocked", false},
		{429, nil, notJSON, map[string]string{"x-vercel-mitigated": "challenge", "Retry-After": "30"}, "upstream_blocked", false},
		{401, nil, notJSON, map[string]string{"x-vercel-mitigated": "deny"}, "invalid_grant", true},
	}
	for _, tc := range cases {
		client, _ := refreshClient(t, nousStep{status: tc.status, payload: tc.payload, raw: tc.raw, headers: tc.headers})
		_, err := nousRefreshAccessToken(context.Background(), client, "https://portal.nousresearch.com", "icebath-cli", "refresh-still-valid")
		authErr := authErrorOf(t, err)
		if authErr.Code != tc.code || authErr.ReloginRequired != tc.terminal || isTerminalNousRefreshError(err) != tc.terminal {
			t.Fatalf("status %d: err = %+v", tc.status, authErr)
		}
		if tc.code == "temporarily_unavailable" || tc.code == "upstream_blocked" {
			if authErr.Retryable == nil || !*authErr.Retryable {
				t.Fatalf("status %d: not retryable", tc.status)
			}
		}
		if _, has := tc.headers["Retry-After"]; has && (!authErr.HasRetryAfter || authErr.RetryAfter != 30*time.Second) {
			t.Fatalf("retry after = %v", authErr.RetryAfter)
		}
	}
}

func TestRuntimeRefresh503PreservesNousOAuthCredentials(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		payload any
		raw     []byte
		code    string
	}{
		{"portal-503", 503, nil, map[string]any{}, nil, "temporarily_unavailable"},
		{"edge-deny-403", 403, map[string]string{"x-vercel-mitigated": "deny"}, nil, []byte("edge block page"), "upstream_blocked"},
		{"edge-challenge-429", 429, map[string]string{"x-vercel-mitigated": "challenge"}, nil, []byte("edge block page"), "upstream_blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetNousSeams(t)
			h := newTestHome(t)
			accessToken := invokeJWT(3600)
			setupNousAuth(t, h, nousAuthSetup{accessToken: accessToken, refreshToken: "refresh-still-valid", expiresAt: futureISO(3600), expiresIn: 3600})
			useNousTransport(t, nousStep{status: tc.status, headers: tc.headers, payload: tc.payload, raw: tc.raw})
			_, err := resolveNousRuntimeCredentials(testEnv(t, h), true, "")
			authErr := authErrorOf(t, err)
			state := nousProviderState(t, h)
			if state["access_token"] != accessToken || state["refresh_token"] != "refresh-still-valid" {
				t.Fatalf("credentials changed: %v", state)
			}
			if _, has := state["last_auth_error"]; has {
				t.Fatalf("quarantined on a transient failure")
			}
			if authErr.Code != tc.code || authErr.ReloginRequired || authErr.Retryable == nil || !*authErr.Retryable {
				t.Fatalf("err = %+v", authErr)
			}
		})
	}
}

func TestRefreshTokenExchangeSendsRefreshTokenHeader(t *testing.T) {
	client, rt := refreshClient(t, nousStep{status: 200, payload: map[string]any{"access_token": "access-2", "refresh_token": "refresh-2"}})
	payload, err := nousRefreshAccessToken(context.Background(), client, "https://portal.nousresearch.com", "icebath-cli", "refresh-1")
	if err != nil {
		t.Fatal(err)
	}
	if payload["access_token"] != "access-2" || payload["refresh_token"] != "refresh-2" {
		t.Fatalf("payload = %v", payload)
	}
	req := rt.seen[0]
	if req.Header.Get("x-nous-refresh-token") != "refresh-1" || req.URL.String() != "https://portal.nousresearch.com/api/oauth/token" {
		t.Fatalf("request = %v %v", req.URL, req.Header)
	}
	if rt.forms[0] != "client_id=icebath-cli&grant_type=refresh_token" {
		t.Fatalf("form = %q", rt.forms[0])
	}
}

func TestSharedStoreWriteAndReadRoundtrip(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env := testEnv(t, h)
	state := fullNousStateFixture()
	writeSharedNousState(env, state, false)
	path := nousSharedStorePath(env)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o", mode)
	}
	loaded, ok := readSharedNousState(env)
	if !ok || loaded["refresh_token"] != "refresh-tok" || loaded["access_token"] != state["access_token"] ||
		loaded["portal_base_url"] != "https://portal.example.com" || loaded["inference_base_url"] != "https://inference.example.com/v1" {
		t.Fatalf("loaded = %v", loaded)
	}
	if _, has := loaded["agent_key"]; has {
		t.Fatalf("agent_key persisted to the shared store")
	}
}

func TestPersistNousCredentialsMirrorsToSharedStore(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	env := testEnv(t, h)
	if _, _, err := persistNousCredentials(env, fullNousStateFixture(), ""); err != nil {
		t.Fatal(err)
	}
	if nousProviderState(t, h) == nil {
		t.Fatalf("providers.nous missing")
	}
	shared, ok := readSharedNousState(env)
	if !ok || shared["refresh_token"] != "refresh-tok" {
		t.Fatalf("shared = %v", shared)
	}
	if !strings.HasPrefix(nousSharedStorePath(env), os.Getenv("ICEBATH_SHARED_AUTH_DIR")) {
		t.Fatalf("shared path = %s", nousSharedStorePath(env))
	}
}

func TestTryImportSharedRehydratesOnSuccess(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env := testEnv(t, h)
	writeSharedNousState(env, fullNousStateFixture(), false)
	freshJWT := invokeJWT(7200)
	refreshNousOAuthFromStateFn = func(env PoolEnv, src map[string]any, timeout time.Duration, forceRefresh bool, onStateUpdate func(map[string]any, string)) (map[string]any, error) {
		if !forceRefresh {
			t.Fatalf("shared import must force the refresh")
		}
		out := cloneMap(src)
		out["access_token"], out["refresh_token"], out["agent_key"], out["agent_key_expires_at"] = freshJWT, "fresh-refresh-tok", freshJWT, futureISO(7200)
		return out, nil
	}
	result, ok := tryImportSharedNousState(env, 15*time.Second)
	if !ok || result["access_token"] != freshJWT || result["refresh_token"] != "fresh-refresh-tok" || result["agent_key"] != freshJWT ||
		result["portal_base_url"] != "https://portal.example.com" || result["client_id"] != "icebath-cli" {
		t.Fatalf("result = %v", result)
	}
}

func TestRuntimeCredentialsRejectsHTTPForProductionPortal(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	setupNousAuth(t, h, nousAuthSetup{accessToken: invokeJWT(-60), refreshToken: "valid-refresh", expiresAt: futureISO(-60), expiresIn: 0})
	store := readAuthStore(t, h)
	store["providers"].(map[string]any)["nous"].(map[string]any)["portal_base_url"] = "http://portal.nousresearch.com"
	writeAuthStore(t, h, store)
	var posted []string
	nousRefreshAccessTokenFn = func(ctx context.Context, client *http.Client, portalBaseURL, clientID, refreshToken string) (map[string]any, error) {
		posted = append(posted, portalBaseURL)
		return map[string]any{"access_token": invokeJWT(3600), "refresh_token": "new-refresh", "expires_in": 3600, "scope": "inference:invoke"}, nil
	}
	if _, err := resolveNousRuntimeCredentials(testEnv(t, h), false, ""); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 || posted[0] != DefaultNousPortalURL {
		t.Fatalf("posted = %v", posted)
	}
}

// ---- test_nous_shared_store_freshness.py ----

func freshnessJWT(tag string, seconds float64, issued *float64, sub string) string {
	extra := map[string]any{"tag": tag}
	if issued != nil {
		extra["iat"] = time.Now().Unix() + int64(*issued)
	}
	return nousJWT(seconds, DefaultNousScope, sub, extra)
}

func freshnessState(tag, refreshToken string, issued, ttl float64, sub, token string) map[string]any {
	if token == "" {
		token = freshnessJWT(tag, issued+ttl, &issued, sub)
	}
	return map[string]any{
		"portal_base_url": DefaultNousPortalURL, "client_id": "icebath-cli", "token_type": "Bearer", "scope": DefaultNousScope,
		"access_token": token, "refresh_token": refreshToken, "obtained_at": futureISO(issued), "expires_in": ttl,
		"expires_at": futureISO(issued + ttl), "agent_key": token, "agent_key_expires_at": futureISO(issued + ttl),
	}
}

func seedFreshness(t *testing.T, env PoolEnv, h string, profile, shared map[string]any) {
	t.Helper()
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "nous", "providers": map[string]any{"nous": profile}})
	writeSharedNousState(env, shared, false)
}

func profileRefreshToken(t *testing.T, h string) string {
	t.Helper()
	return anyString(nousProviderState(t, h)["refresh_token"])
}

func sharedRefreshToken(t *testing.T, env PoolEnv) string {
	t.Helper()
	shared, _ := readSharedNousState(env)
	return anyString(shared["refresh_token"])
}

func TestOlderSharedStateNeverReplacesProfileAndIsHealedFromIt(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env := testEnv(t, h)
	seedFreshness(t, env, h, freshnessState("K2", "RT2", 0, 3000, "user-1", ""), freshnessState("K1", "RT1", -1000, 3000, "user-1", ""))
	var posted []string
	fakeNousRefresh(&posted, func(int) map[string]any {
		return map[string]any{"access_token": freshnessJWT("K3", 3600, nil, "user-1"), "refresh_token": "RT3", "expires_in": 3600, "token_type": "Bearer", "scope": DefaultNousScope}
	})
	if _, err := ResolveNousAccessToken(env, NousAccessTokenOptions{}); err != nil {
		t.Fatal(err)
	}
	if profileRefreshToken(t, h) != "RT2" || sharedRefreshToken(t, env) != "RT2" {
		t.Fatalf("profile %s shared %s", profileRefreshToken(t, h), sharedRefreshToken(t, env))
	}
	if _, err := resolveNousRuntimeCredentials(env, true, ""); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 || posted[0] != "RT2" || profileRefreshToken(t, h) != "RT3" {
		t.Fatalf("posted %v profile %s", posted, profileRefreshToken(t, h))
	}
}

func TestNewerSharedStateIsAdoptedAndAStaleRenewalCannotOverwriteIt(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env := testEnv(t, h)
	newer, older := freshnessState("K2", "RT2", 0, 3000, "user-1", ""), freshnessState("K1", "RT1", -1000, 3000, "user-1", "")
	seedFreshness(t, env, h, older, newer)
	profile := cloneMap(older)
	unlock, err := nousSharedStoreLock(env, AuthLockTimeout)
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeSharedNousOAuthState(env, profile)
	unlock()
	if !merged || profile["refresh_token"] != "RT2" {
		t.Fatalf("merged %v profile %v", merged, profile["refresh_token"])
	}
	writeSharedNousState(env, older, true)
	if sharedRefreshToken(t, env) != "RT2" {
		t.Fatalf("stale renewal overwrote the newer shared copy")
	}
}

// Fake single use Portal issuing ttl second keys and refusing a replayed refresh token
func singleUsePortal(t *testing.T, ttl float64) *[]string {
	t.Helper()
	posted := &[]string{}
	nousRefreshAccessTokenFn = func(ctx context.Context, client *http.Client, portalBaseURL, clientID, refreshToken string) (map[string]any, error) {
		for _, seen := range *posted {
			if seen == refreshToken {
				return nil, nousErr("Refresh token reuse detected", "invalid_grant", true)
			}
		}
		*posted = append(*posted, refreshToken)
		n := len(*posted)
		zero := 0.0
		return map[string]any{"access_token": freshnessJWT("K-new"+itoa(n), ttl, &zero, "user-1"), "refresh_token": "RT-new" + itoa(n),
			"expires_in": ttl, "token_type": "Bearer", "scope": DefaultNousScope}, nil
	}
	return posted
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+n))) }

func TestALaterRotationWithAShorterTTLIsStillTheNewerPair(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env := testEnv(t, h)
	inSync := freshnessState("K1", "RT1", -60, 3600, "user-1", "")
	seedFreshness(t, env, h, inSync, inSync)
	posted := singleUsePortal(t, 900)
	mustResolve := func(force bool) {
		t.Helper()
		if _, err := resolveNousRuntimeCredentials(env, force, ""); err != nil {
			t.Fatal(err)
		}
	}
	mustResolve(true)
	if sharedRefreshToken(t, env) != "RT-new1" {
		t.Fatalf("shared = %s", sharedRefreshToken(t, env))
	}
	mustResolve(false)
	mustResolve(true)
	if strings.Join(*posted, ",") != "RT1,RT-new1" || profileRefreshToken(t, h) != "RT-new2" {
		t.Fatalf("posted %v profile %s", *posted, profileRefreshToken(t, h))
	}
	seedFreshness(t, env, h, freshnessState("K2", "RT2", -600, 3600, "user-1", ""), freshnessState("K3", "RT3", -5, 900, "user-1", ""))
	mustResolve(false)
	if sharedRefreshToken(t, env) != "RT3" {
		t.Fatalf("shared rolled back to %s", sharedRefreshToken(t, env))
	}
	mustResolve(true)
	if (*posted)[len(*posted)-1] != "RT3" {
		t.Fatalf("posted = %v", *posted)
	}
}

func TestTwoRotationsInOneIatSecondOrderByObtainedAtNotExpiry(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env := testEnv(t, h)
	iat := int64(2_000_000_000)
	rotation := func(tag, refreshToken string, offset float64, ttl float64) map[string]any {
		token := jwtWithClaims(map[string]any{"sub": "user-1", "scope": DefaultNousScope, "iat": iat, "exp": iat + int64(ttl), "tag": tag})
		state := freshnessState(tag, refreshToken, 0, ttl, "user-1", token)
		state["obtained_at"] = nousEpochISO(float64(iat) + offset)
		return state
	}
	first := rotation("K1", "RT1", 0.2, 3600)
	later := rotation("K2", "RT2", 0.7, 900)
	seedFreshness(t, env, h, first, later)
	profile := cloneMap(first)
	unlock, err := nousSharedStoreLock(env, AuthLockTimeout)
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeSharedNousOAuthState(env, profile)
	unlock()
	if !merged || profile["refresh_token"] != "RT2" {
		t.Fatalf("merged %v profile %v", merged, profile["refresh_token"])
	}
	writeSharedNousState(env, first, true)
	if sharedRefreshToken(t, env) != "RT2" {
		t.Fatalf("spent pair restored")
	}
}

func TestUnknownIdentitiesNeverPushOverTheSharedStore(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env := testEnv(t, h)
	newer := freshnessState("A", "RTA", -10, 3000, "user-1", "opaqueA")
	older := freshnessState("B", "RTB", -600, 3000, "user-1", "opaqueB")
	seedFreshness(t, env, h, newer, older)
	unlock, err := nousSharedStoreLock(env, AuthLockTimeout)
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeSharedNousOAuthState(env, cloneMap(newer))
	unlock()
	if merged || sharedRefreshToken(t, env) != "RTB" {
		t.Fatalf("merged %v shared %s", merged, sharedRefreshToken(t, env))
	}
}

func TestAProfileNeverAdoptsAnotherAccountsNewerSharedLogin(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env := testEnv(t, h)
	seedFreshness(t, env, h, freshnessState("KA", "RTA", -600, 3000, "uA", ""), freshnessState("KB", "RTB", -10, 3000, "uB", ""))
	if _, err := ResolveNousAccessToken(env, NousAccessTokenOptions{}); err != nil {
		t.Fatal(err)
	}
	if profileRefreshToken(t, h) != "RTA" || sharedRefreshToken(t, env) != "RTB" {
		t.Fatalf("profile %s shared %s", profileRefreshToken(t, h), sharedRefreshToken(t, env))
	}
}

func TestAQuarantinedProfileRecoversFromASiblingsRelogin(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env := testEnv(t, h)
	quarantined := freshnessState("K-old", "RT-old", -7200, 3000, "user-1", "")
	for _, k := range []string{"access_token", "refresh_token", "expires_at", "expires_in", "obtained_at", "agent_key", "agent_key_expires_at"} {
		delete(quarantined, k)
	}
	quarantined["last_auth_error"] = map[string]any{"provider": "nous", "code": "invalid_grant", "reason": "test"}
	seedFreshness(t, env, h, quarantined, freshnessState("K-login", "RT-login", -5, 3000, "user-1", ""))
	if _, err := ResolveNousAccessToken(env, NousAccessTokenOptions{}); err != nil {
		t.Fatal(err)
	}
	if profileRefreshToken(t, h) != "RT-login" {
		t.Fatalf("profile = %s", profileRefreshToken(t, h))
	}
}

// ---- test_nous_inference_url_validation.py ----

func TestValidatorRejectsAttackerHostAndKeepsTheDefault(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	env, rec := recordingEnv(t, h)
	if got := validateNousInferenceURLFromNetworkIn(env, "https://attacker.com/v1"); got != "" {
		t.Fatalf("accepted %q", got)
	}
	if len(rec.containing("attacker.com")) == 0 {
		t.Fatalf("no warning named the host")
	}
	if got := validateNousInferenceURLFromNetwork(DefaultNousInferenceURL); got != strings.TrimRight(DefaultNousInferenceURL, "/") {
		t.Fatalf("default refused: %q", got)
	}
}

func TestRefreshResetsRejectedURLToDefault(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	poisoned := "https://stg-inference-api.nousresearch.com/v1"
	state := map[string]any{"access_token": invokeJWT(-60), "refresh_token": "rtok", "client_id": "icebath-cli",
		"portal_base_url": DefaultNousPortalURL, "inference_base_url": poisoned}
	nousRefreshAccessTokenFn = func(ctx context.Context, client *http.Client, portalBaseURL, clientID, refreshToken string) (map[string]any, error) {
		return map[string]any{"access_token": invokeJWT(3600), "refresh_token": "newrtok", "expires_in": 3600, "inference_base_url": poisoned}, nil
	}
	result, err := refreshNousOAuthFromState(testEnv(t, h), state, 15*time.Second, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result["inference_base_url"] != DefaultNousInferenceURL {
		t.Fatalf("inference_base_url = %v", result["inference_base_url"])
	}
}

const stagingInference = "https://stg-inference-api.nousresearch.com/v1"

func TestNoRefreshEnvOverrideNotPersisted(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	setupNousAuth(t, h, nousAuthSetup{accessToken: invokeJWT(3600), refreshToken: "rtok", expiresAt: futureISO(3600), expiresIn: 3600})
	store := readAuthStore(t, h)
	store["providers"].(map[string]any)["nous"].(map[string]any)["inference_base_url"] = DefaultNousInferenceURL
	writeAuthStore(t, h, store)
	t.Setenv("NOUS_INFERENCE_BASE_URL", stagingInference)
	creds, err := resolveNousRuntimeCredentials(testEnv(t, h), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if nousProviderState(t, h)["inference_base_url"] != DefaultNousInferenceURL {
		t.Fatalf("env override leaked into persisted state")
	}
	if creds["base_url"] != stagingInference {
		t.Fatalf("base_url = %v", creds["base_url"])
	}
}

func TestNoRefreshHealsPoisonedStoredWithoutEnv(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	setupNousAuth(t, h, nousAuthSetup{accessToken: invokeJWT(3600), refreshToken: "rtok", expiresAt: futureISO(3600), expiresIn: 3600})
	store := readAuthStore(t, h)
	store["providers"].(map[string]any)["nous"].(map[string]any)["inference_base_url"] = stagingInference
	writeAuthStore(t, h, store)
	creds, err := resolveNousRuntimeCredentials(testEnv(t, h), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if creds["base_url"] != DefaultNousInferenceURL {
		t.Fatalf("base_url = %v", creds["base_url"])
	}
}

// ---- test_nous_portal_staging_allowlist.py ----

func stagingAuthFile(t *testing.T, h, storedPortal string) {
	t.Helper()
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "nous", "providers": map[string]any{"nous": map[string]any{
		"portal_base_url": storedPortal, "access_token": "expired-access", "refresh_token": "staging-refresh",
		"client_id": "icebath-cli-vps", "expires_at": "2000-01-01T00:00:00+00:00"}}})
}

func runAccessTokenAndCapture(t *testing.T, h string) ([]string, *recordingHandler) {
	t.Helper()
	var posted []string
	nousRefreshAccessTokenFn = func(ctx context.Context, client *http.Client, portalBaseURL, clientID, refreshToken string) (map[string]any, error) {
		posted = append(posted, portalBaseURL)
		return map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600}, nil
	}
	env, rec := recordingEnv(t, h)
	useDefaultLogger(t, rec)
	if _, err := ResolveNousAccessToken(env, NousAccessTokenOptions{}); err != nil {
		t.Fatal(err)
	}
	return posted, rec
}

func TestEnvOverrideWinsEvenWithStagingStateStored(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	staging := "https://portal.staging-nousresearch.com"
	t.Setenv("ICEBATH_PORTAL_BASE_URL", staging)
	stagingAuthFile(t, h, staging)
	posted, rec := runAccessTokenAndCapture(t, h)
	if len(posted) != 1 || posted[0] != staging {
		t.Fatalf("posted = %v", posted)
	}
	if len(rec.containing("ignoring invalid portal_base_url")) > 0 {
		t.Fatalf("allowlist gate fired for the operator override")
	}
}

func TestNoEnvNoStagingStateProdURLUsedUnmodified(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	stagingAuthFile(t, h, DefaultNousPortalURL)
	posted, rec := runAccessTokenAndCapture(t, h)
	if len(posted) != 1 || posted[0] != DefaultNousPortalURL {
		t.Fatalf("posted = %v", posted)
	}
	if len(rec.containing("ignoring invalid portal_base_url")) > 0 {
		t.Fatalf("allowlist warned on the production host")
	}
}

// ---- test_nous_nonproduction_inference_host.py ----

const (
	prodInference  = "https://inference-api.nousresearch.com/v1"
	envInference   = "https://inference.example-env.nousresearch.com/v1"
	otherNousHost  = "https://portal.nousresearch.com/v1"
	nonprodPortal  = "https://portal.example-env.nousresearch.com"
	launchOverride = "https://launch.example.nousresearch.com"
)

func TestRoutingOverridesFollowTheProfileScopeAndFailClosedWithoutOne(t *testing.T) {
	cases := []struct {
		name   string
		envVar string
		helper func(env PoolEnv) string
	}{
		{"inference", "NOUS_INFERENCE_BASE_URL", nousInferenceEnvOverrideIn},
		{"portal", "ICEBATH_PORTAL_BASE_URL", nousPortalEnvOverride},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetNousSeams(t)
			h := newTestHome(t)
			t.Setenv(tc.envVar, launchOverride)
			env, rec := recordingEnv(t, h)
			env.Scope = nil
			if got := tc.helper(env); got != launchOverride {
				t.Fatalf("single profile read = %q", got)
			}
			SetMultiplexActive(true)
			scoped := env
			scoped.Ctx = WithSecretScope(context.Background(), map[string]string{tc.envVar: "https://profile.example.nousresearch.com"}, h)
			if got := tc.helper(scoped); got != "https://profile.example.nousresearch.com" {
				t.Fatalf("scoped read = %q", got)
			}
			scoped.Ctx = WithSecretScope(context.Background(), map[string]string{}, h)
			if got := tc.helper(scoped); got != "" {
				t.Fatalf("scoped miss read environ: %q", got)
			}
			unscoped := env
			unscoped.Ctx = context.Background()
			if got := tc.helper(unscoped); got != "" {
				t.Fatalf("lost scope read the launch value: %q", got)
			}
			if len(rec.containing("no profile secret scope")) == 0 {
				t.Fatalf("lost scope did not warn")
			}
		})
	}
}

func TestNetworkInferenceHostNeedsTheOperatorNamedRecipient(t *testing.T) {
	cases := []struct{ override, url, expected string }{
		{envInference, envInference, envInference},
		{"", envInference, ""},
		{envInference, otherNousHost, ""},
		{"", prodInference, prodInference},
		{envInference, prodInference, prodInference},
	}
	for _, tc := range cases {
		resetNousSeams(t)
		newTestHome(t)
		if tc.override != "" {
			t.Setenv("NOUS_INFERENCE_BASE_URL", tc.override)
		}
		if got := validateNousInferenceURLFromNetwork(tc.url); got != tc.expected {
			t.Fatalf("override %q url %q: got %q want %q", tc.override, tc.url, got, tc.expected)
		}
	}
}

func TestPortalOverrideAloneGrantsNothing(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	t.Setenv("ICEBATH_PORTAL_BASE_URL", nonprodPortal)
	env := testEnv(t, h)
	state := map[string]any{"portal_base_url": nonprodPortal, "inference_base_url": envInference, "client_id": "cid"}
	portal, stored, _, _ := nousEffectiveRouting(env, state)
	if portal != nonprodPortal || stored != DefaultNousInferenceURL {
		t.Fatalf("portal %q stored %q", portal, stored)
	}
	if got := nousHealedInferenceURL(env, map[string]any{"inference_base_url": envInference}); got != DefaultNousInferenceURL {
		t.Fatalf("healed = %q", got)
	}
	t.Setenv("NOUS_INFERENCE_BASE_URL", envInference)
	if got := nousHealedInferenceURL(env, map[string]any{"inference_base_url": envInference}); got != envInference {
		t.Fatalf("healed with override = %q", got)
	}
	if got := validateNousInferenceURLFromNetwork(strings.Replace(envInference, "https", "http", 1)); got != "" {
		t.Fatalf("plain http accepted: %q", got)
	}
}

func TestRecipientMatchFollowsTheProfileScopeUnderMultiplex(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	t.Setenv("NOUS_INFERENCE_BASE_URL", envInference)
	SetMultiplexActive(true)
	env := testEnv(t, h)
	env.Scope = nil
	env.Ctx = WithSecretScope(context.Background(), map[string]string{}, h)
	if got := validateNousInferenceURLFromNetworkIn(env, envInference); got != "" {
		t.Fatalf("secondary without the override accepted %q", got)
	}
	env.Ctx = WithSecretScope(context.Background(), map[string]string{"NOUS_INFERENCE_BASE_URL": envInference}, h)
	if got := validateNousInferenceURLFromNetworkIn(env, envInference); got != envInference {
		t.Fatalf("scoped override refused: %q", got)
	}
}

// ---- test_nous_session_validity.py ----

func blockLiveNousAuth(t *testing.T) {
	t.Helper()
	nousHTTPClientFn = func(env PoolEnv, timeout time.Duration, verify oauthVerify) *http.Client {
		t.Fatalf("session validity must not resolve or refresh credentials")
		return nil
	}
	nousRefreshAccessTokenFn = func(ctx context.Context, client *http.Client, portalBaseURL, clientID, refreshToken string) (map[string]any, error) {
		t.Fatalf("session validity must not resolve or refresh credentials")
		return nil, nil
	}
}

func TestLocalStatusNotLoggedInAfterTerminalQuarantine(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"nous": map[string]any{
		"last_auth_error": map[string]any{"relogin_required": true, "code": "invalid_grant"}}}})
	blockLiveNousAuth(t)
	status := GetNousAuthStatusLocal(testEnv(t, h))
	if status["logged_in"] != false || status["relogin_required"] != true || status["error_code"] != "invalid_grant" {
		t.Fatalf("status = %v", status)
	}
	if got := GetNousSessionValidity(testEnv(t, h)); got != NousSessionTerminal {
		t.Fatalf("validity = %s", got)
	}
}

func TestLocalStatusRepeatedPollingNeverUsesLiveAuth(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"nous": map[string]any{
		"access_token": invokeJWT(3600), "refresh_token": "rt", "scope": DefaultNousScope}}})
	blockLiveNousAuth(t)
	env := testEnv(t, h)
	for i := 0; i < 10; i++ {
		if GetNousAuthStatusLocal(env)["logged_in"] != true {
			t.Fatalf("poll %d not logged in", i)
		}
	}
	if got := GetNousSessionValidity(env); got != NousSessionValid {
		t.Fatalf("validity = %s", got)
	}
}

// ---- test_billing_scope_stepup.py ----

func TestStepUpRequestsBillingScopeAndReusesPriorURLs(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"nous": map[string]any{
		"scope": "inference:invoke", "portal_base_url": "https://preview.example.com", "inference_base_url": "https://inf.example.com", "client_id": "icebath-cli"}}})
	var captured NousDeviceLoginArgs
	nousDeviceCodeLoginFn = func(env PoolEnv, out io.Writer, args NousDeviceLoginArgs) (map[string]any, error) {
		captured = args
		return map[string]any{"scope": "inference:invoke billing:manage", "access_token": "t"}, nil
	}
	granted, err := StepUpNousBillingScope(testEnv(t, h), io.Discard, true, 15*time.Second, nil)
	if err != nil || !granted {
		t.Fatalf("granted %v err %v", granted, err)
	}
	if captured.Scope != "inference:invoke billing:manage" || captured.PortalBaseURL != "https://preview.example.com" || captured.ClientID != "icebath-cli" {
		t.Fatalf("captured = %+v", captured)
	}
}

func TestStepUpDropsTheBillingTokenCache(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"nous": map[string]any{"scope": "inference:invoke"}}})
	nousDeviceCodeLoginFn = func(env PoolEnv, out io.Writer, args NousDeviceLoginArgs) (map[string]any, error) {
		return map[string]any{"scope": "inference:invoke billing:manage", "access_token": "new"}, nil
	}
	granted, err := StepUpNousBillingScope(testEnv(t, h), io.Discard, true, 15*time.Second, nil)
	if err != nil || !granted || testBillingInvalidateCalls != 1 || testSettleAfterUpgradeCalls != 1 {
		t.Fatalf("granted %v err %v invalidations %d settles %d", granted, err, testBillingInvalidateCalls, testSettleAfterUpgradeCalls)
	}
}

func TestDeviceLoginFiresOnVerificationBeforePolling(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	var order []string
	rt := useNousTransport(t,
		nousStep{status: 200, payload: map[string]any{"verification_uri_complete": "https://portal.example/device?code=ABCD", "user_code": "ABCD-1234",
			"device_code": "dev", "verification_uri": "https://portal.example/device", "expires_in": 600, "interval": 5}},
		nousStep{status: 200, payload: map[string]any{"access_token": "t", "scope": "inference:invoke", "expires_in": 3600}})
	seen := map[string]string{}
	_, _ = nousDeviceCodeLogin(testEnv(t, h), io.Discard, NousDeviceLoginArgs{OpenBrowser: false, OnVerification: func(url, code string) {
		order = append(order, "verify")
		seen["url"], seen["code"] = url, code
	}})
	for _, req := range rt.seen {
		if strings.HasSuffix(req.URL.Path, "/api/oauth/token") {
			order = append(order, "poll")
		}
	}
	if len(order) < 2 || order[0] != "verify" || order[1] != "poll" {
		t.Fatalf("order = %v", order)
	}
	if seen["url"] != "https://portal.example/device?code=ABCD" || seen["code"] != "ABCD-1234" {
		t.Fatalf("seen = %v", seen)
	}
}

// ---- test_credential_pool_nous_refresh_stampede.py ----

func TestForcedRefreshAdoptsPeerRotationInsteadOfReposting(t *testing.T) {
	resetNousSeams(t)
	h := newTestHome(t)
	peerToken := invokeJWT(3600)
	failedToken := invokeJWT(3000)
	setupNousAuth(t, h, nousAuthSetup{accessToken: peerToken, refreshToken: "rt-after-peer-rotation", scope: DefaultNousScope, expiresAt: futureISO(3600), expiresIn: 3600})
	var posted []string
	fakeNousRefresh(&posted, func(int) map[string]any {
		return map[string]any{"access_token": invokeJWT(7200), "refresh_token": "rt-should-not-happen", "expires_in": 7200, "token_type": "Bearer", "scope": DefaultNousScope}
	})
	env := testEnv(t, h)
	creds, err := resolveNousRuntimeCredentials(env, true, failedToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(posted) != 0 || creds["api_key"] != peerToken {
		t.Fatalf("posted %v api_key %v", posted, creds["api_key"])
	}
	if _, err := resolveNousRuntimeCredentials(env, true, ""); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 || posted[0] != "rt-after-peer-rotation" {
		t.Fatalf("posted = %v", posted)
	}
}
