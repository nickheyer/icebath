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
	prevSleep, prevClient, prevRefresh, prevImport, prevProbe := oauthSleep, codexHTTPClientFn, codexRefreshPureFn, importCodexCLITokensFn, probeCodexQuotaRestoredFn
	oauthSleep = func(time.Duration) {}
	codexQuotaProbeMu.Lock()
	codexQuotaProbeCache = map[string]codexProbeResult{}
	codexQuotaProbeMu.Unlock()
	t.Cleanup(func() {
		oauthSleep, codexHTTPClientFn, codexRefreshPureFn, importCodexCLITokensFn, probeCodexQuotaRestoredFn = prevSleep, prevClient, prevRefresh, prevImport, prevProbe
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

func codexAuthStore(tokens map[string]any) map[string]any {
	return map[string]any{
		"version": 1, "active_provider": "openai-codex",
		"providers": map[string]any{"openai-codex": map[string]any{"tokens": tokens, "last_refresh": "2026-02-26T00:00:00Z", "auth_mode": "chatgpt"}},
	}
}

func authErrorOf(t *testing.T, err error) *AuthError {
	t.Helper()
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected AuthError, got %v", err)
	}
	return authErr
}

func singletonTokens(t *testing.T, h string) map[string]any {
	t.Helper()
	providers, _ := readAuthStore(t, h)["providers"].(map[string]any)
	codex, _ := providers["openai-codex"].(map[string]any)
	tokens, _ := codex["tokens"].(map[string]any)
	return tokens
}

func TestResolveCodexRuntimeCredentialsMissingAccessToken(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, codexAuthStore(map[string]any{"access_token": "", "refresh_token": "refresh"}))
	_, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	authErr := authErrorOf(t, err)
	if authErr.Code != "codex_auth_missing_access_token" || !authErr.ReloginRequired {
		t.Fatalf("unexpected error %+v", authErr)
	}
}

func TestResolveCodexFallsBackToPoolWhenSingletonEmpty(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{
		"version": 1, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"source": "device_code", "access_token": "pool-fallback-token", "refresh_token": "pool-refresh", "last_status": "ok", "auth_type": "oauth"}}},
	})
	resolved, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved["api_key"] != "pool-fallback-token" || resolved["source"] != "credential_pool" || anyString(resolved["base_url"]) == "" {
		t.Fatalf("unexpected result %v", resolved)
	}
}

func TestSaveCodexTokensSyncsCredentialPool(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{
		"version": 1,
		"providers": map[string]any{"openai-codex": map[string]any{
			"tokens": map[string]any{"access_token": "old-at", "refresh_token": "old-rt"}, "last_refresh": "2026-01-01T00:00:00Z", "auth_mode": "chatgpt"}},
		"credential_pool": map[string]any{"openai-codex": []any{
			map[string]any{"id": "abc123", "source": "device_code", "auth_type": "oauth", "access_token": "old-at", "refresh_token": "old-rt",
				"last_status": "exhausted", "last_error_code": 401, "last_error_reason": "token_invalidated", "last_error_reset_at": 9999999999},
			map[string]any{"id": "manual1", "source": "manual:codex", "auth_type": "oauth", "access_token": "manual-at", "refresh_token": "manual-rt"},
		}},
	})
	if err := saveCodexTokens(env, map[string]any{"access_token": "new-at", "refresh_token": "new-rt"}, "2026-05-27T00:00:00Z", "", true, false); err != nil {
		t.Fatal(err)
	}
	rows := poolRows(t, h, "openai-codex")
	seeded := rowByID(rows, "abc123")
	if seeded["access_token"] != "new-at" || seeded["refresh_token"] != "new-rt" || seeded["last_refresh"] != "2026-05-27T00:00:00Z" {
		t.Fatalf("seeded row not refreshed: %v", seeded)
	}
	for _, f := range []string{"last_status", "last_error_code", "last_error_reason", "last_error_reset_at"} {
		if seeded[f] != nil {
			t.Fatalf("%s not cleared: %v", f, seeded[f])
		}
	}
	manual := rowByID(rows, "manual1")
	if manual["access_token"] != "manual-at" || manual["refresh_token"] != "manual-rt" {
		t.Fatalf("manual row overwritten: %v", manual)
	}
	if singletonTokens(t, h)["access_token"] != "new-at" {
		t.Fatal("singleton not updated")
	}
}

func TestSaveCodexTokensSyncsManualDeviceCodeAliases(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{
		"version": 1,
		"providers": map[string]any{"openai-codex": map[string]any{
			"tokens": map[string]any{"access_token": "old-at", "refresh_token": "old-rt"}, "last_refresh": "2026-01-01T00:00:00Z", "auth_mode": "chatgpt"}},
		"credential_pool": map[string]any{"openai-codex": []any{
			map[string]any{"id": "seeded", "source": "device_code", "auth_type": "oauth", "access_token": "old-at", "refresh_token": "old-rt"},
			map[string]any{"id": "legacy-alias", "source": "manual:device_code", "auth_type": "oauth", "access_token": "old-at", "refresh_token": "old-rt",
				"last_status": "exhausted", "last_error_code": 401, "last_error_reason": "token_invalidated"},
			map[string]any{"id": "independent", "source": "manual:device_code", "auth_type": "oauth", "access_token": "independent-at", "refresh_token": "independent-rt"},
			map[string]any{"id": "api-key", "source": "manual:api_key", "auth_type": "api_key", "access_token": "user-api-key"},
		}},
	})
	if err := saveCodexTokens(env, map[string]any{"access_token": "fresh-at", "refresh_token": "fresh-rt"}, "2026-05-28T00:00:00Z", "", true, false); err != nil {
		t.Fatal(err)
	}
	rows := poolRows(t, h, "openai-codex")
	seeded := rowByID(rows, "seeded")
	if seeded["access_token"] != "fresh-at" || seeded["refresh_token"] != "fresh-rt" {
		t.Fatalf("seeded not refreshed: %v", seeded)
	}
	legacy := rowByID(rows, "legacy-alias")
	if legacy["access_token"] != "fresh-at" || legacy["refresh_token"] != "fresh-rt" || legacy["last_refresh"] != "2026-05-28T00:00:00Z" {
		t.Fatalf("legacy alias not refreshed: %v", legacy)
	}
	if legacy["last_status"] != nil || legacy["last_error_code"] != nil || legacy["last_error_reason"] != nil {
		t.Fatalf("legacy alias markers not cleared: %v", legacy)
	}
	independent := rowByID(rows, "independent")
	if independent["access_token"] != "independent-at" || independent["refresh_token"] != "independent-rt" {
		t.Fatalf("independent account overwritten: %v", independent)
	}
	apiKey := rowByID(rows, "api-key")
	if apiKey["access_token"] != "user-api-key" || apiKey["refresh_token"] != nil {
		t.Fatalf("api key row touched: %v", apiKey)
	}
}

func TestSaveCodexTokensClearsErrorMarkersOnlyOnRefreshedEntries(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{
		"version": 1,
		"providers": map[string]any{"openai-codex": map[string]any{
			"tokens": map[string]any{"access_token": "acctA-at", "refresh_token": "acctA-rt"}, "auth_mode": "chatgpt"}},
		"credential_pool": map[string]any{"openai-codex": []any{
			map[string]any{"id": "seeded", "source": "device_code", "auth_type": "oauth", "access_token": "acctA-at", "refresh_token": "acctA-rt",
				"last_status": "exhausted", "last_error_code": 401},
			map[string]any{"id": "acctB", "source": "manual:device_code", "auth_type": "oauth", "access_token": "acctB-at", "refresh_token": "acctB-rt",
				"last_status": "exhausted", "last_error_code": 429, "last_error_reason": "quota_exhausted"},
		}},
	})
	if err := saveCodexTokens(env, map[string]any{"access_token": "fresh-at", "refresh_token": "fresh-rt"}, "2026-06-05T00:00:00Z", "", true, false); err != nil {
		t.Fatal(err)
	}
	rows := poolRows(t, h, "openai-codex")
	seeded := rowByID(rows, "seeded")
	if seeded["access_token"] != "fresh-at" || seeded["last_status"] != nil || seeded["last_error_code"] != nil {
		t.Fatalf("seeded row wrong: %v", seeded)
	}
	acctB := rowByID(rows, "acctB")
	if acctB["access_token"] != "acctB-at" || acctB["last_status"] != "exhausted" || acctB["last_error_code"] != float64(429) || acctB["last_error_reason"] != "quota_exhausted" {
		t.Fatalf("independent row touched: %v", acctB)
	}
}

func TestCodexTokensNotWrittenToCodexCLIFile(t *testing.T) {
	h, env := codexTestEnv(t)
	codexHome := filepath.Join(filepath.Dir(h), "codex-cli")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	if err := saveCodexTokens(env, map[string]any{"access_token": "icebath-at", "refresh_token": "icebath-rt"}, "", "", true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the Codex CLI file was written")
	}
	data, err := readCodexTokens(env, true)
	if err != nil {
		t.Fatal(err)
	}
	if data.tokens["access_token"] != "icebath-at" {
		t.Fatalf("store tokens %v", data.tokens)
	}
}

func TestResolveReturnsAuthStoreSource(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, codexAuthStore(map[string]any{"access_token": "access", "refresh_token": "refresh"}))
	creds, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if creds["source"] != "icebath-auth-store" || creds["provider"] != "openai-codex" || creds["base_url"] != DefaultCodexBaseURL {
		t.Fatalf("unexpected creds %v", creds)
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
		"version": 1, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"source": "device_code", "access_token": "pool-revoked", "refresh_token": "pool-refresh", "last_status": "ok", "auth_type": "oauth"}}},
	})
	var hints []string
	prev := tokensSingletonProviders["openai-codex"]
	spec := prev
	spec.refresh = func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		hints = append(hints, accessToken)
		return map[string]any{"access_token": "pool-fresh", "refresh_token": "pool-refresh-2", "last_refresh": utcNowZ()}, nil
	}
	tokensSingletonProviders["openai-codex"] = spec
	t.Cleanup(func() { tokensSingletonProviders["openai-codex"] = prev })
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

// ---- self heal ----

var staleCodexPair = map[string]any{"access_token": "stale-access", "refresh_token": "stale-refresh"}

func rejectedRefresh(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
	return nil, codexErr("refresh token rejected", "invalid_grant", true)
}

func TestSelfHealsOnStaleRefreshToken(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, codexAuthStore(staleCodexPair))
	fresh := map[string]any{"access_token": "fresh-access", "refresh_token": "fresh-refresh", "last_refresh": "2026-06-12T00:00:00Z"}
	codexRefreshPureFn = rejectedRefresh
	importCodexCLITokensFn = func(PoolEnv) map[string]any { return mergeTokens(fresh, nil) }
	out, err := refreshCodexAuthTokens(env, mergeTokens(staleCodexPair, nil), 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out["access_token"] != "fresh-access" || out["refresh_token"] != "fresh-refresh" {
		t.Fatalf("unexpected tokens %v", out)
	}
	if singletonTokens(t, h)["access_token"] != "fresh-access" {
		t.Fatal("recovered token not persisted")
	}
}

func seedCodexHomes(t *testing.T, h string, icebathTokens, cliTokens map[string]any) string {
	t.Helper()
	codexHome := filepath.Join(filepath.Dir(h), "codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"openai-codex": map[string]any{"tokens": icebathTokens, "auth_mode": "chatgpt"}}})
	raw, _ := json.Marshal(map[string]any{"tokens": cliTokens})
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(h, "auth.json")
}

func TestSelfHealsMissingSingletonAccessTokenFromCodexCLI(t *testing.T) {
	h, env := codexTestEnv(t)
	seedCodexHomes(t, h, map[string]any{"refresh_token": "stale-refresh"}, map[string]any{"access_token": "fresh-access", "refresh_token": "fresh-refresh"})
	resolved, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved["api_key"] != "fresh-access" || resolved["source"] != "icebath-auth-store" {
		t.Fatalf("unexpected result %v", resolved)
	}
	tokens := singletonTokens(t, h)
	if tokens["access_token"] != "fresh-access" || tokens["refresh_token"] != "fresh-refresh" {
		t.Fatalf("store tokens %v", tokens)
	}
}

func TestOptOutNeverAdoptsCodexCLILogin(t *testing.T) {
	h, env := codexTestEnv(t)
	writeConfigYAML(t, h, "auth:\n  adopt_external_logins: false\n")
	authFile := seedCodexHomes(t, h, map[string]any{"refresh_token": "stale-refresh"}, map[string]any{"access_token": "fresh-access", "refresh_token": "fresh-refresh"})
	before := readAuthStore(t, h)
	_, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	if authErrorOf(t, err).Code != "codex_auth_missing_access_token" {
		t.Fatalf("unexpected error %v", err)
	}
	codexRefreshPureFn = rejectedRefresh
	_, err = refreshCodexAuthTokens(env, mergeTokens(staleCodexPair, nil), 5*time.Second)
	if !authErrorOf(t, err).ReloginRequired {
		t.Fatalf("relogin error papered over: %v", err)
	}
	_ = authFile
	if !config.Equal(readAuthStore(t, h), before) {
		t.Fatal("auth.json changed")
	}
}

func codexAccountJWT(accountID, sub string) string {
	return jwtWithClaims(map[string]any{"sub": sub, "exp": time.Now().Unix() + 3600, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID}})
}

func TestRecoveryRefusesCodexCLILoginFromAnotherWorkspace(t *testing.T) {
	h, env := codexTestEnv(t)
	personal, team := codexAccountJWT("acct-personal", "user-1"), codexAccountJWT("acct-team", "user-1")
	authFile := seedCodexHomes(t, h, map[string]any{"access_token": personal}, map[string]any{"access_token": team, "refresh_token": "rt-team"})
	before, _ := os.ReadFile(authFile)
	logs := &recordingHandler{}
	env.Log = newRecordingLogger(logs)
	_, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{SkipExpiringRefresh: true})
	if authErrorOf(t, err).Code != "codex_auth_missing_refresh_token" {
		t.Fatalf("unexpected error %v", err)
	}
	after, _ := os.ReadFile(authFile)
	if !bytes.Equal(before, after) {
		t.Fatal("auth.json was rewritten")
	}
	warnings := logs.containing("different ChatGPT workspace")
	if len(warnings) == 0 {
		t.Fatal("refusal not logged")
	}
	for _, m := range logs.messages {
		if strings.Contains(m, team) {
			t.Fatal("token leaked into the log")
		}
	}
}

func TestRecoveryDoesNotOverwriteConcurrentReauth(t *testing.T) {
	h, env := codexTestEnv(t)
	personal, reauthed := codexAccountJWT("acct-personal", "user-1"), codexAccountJWT("acct-personal", "user-1-fresh")
	seedCodexHomes(t, h, map[string]any{"access_token": personal}, map[string]any{"access_token": codexAccountJWT("acct-personal", "user-1"), "refresh_token": "rt-cli"})
	importCodexCLITokensFn = func(env PoolEnv) map[string]any {
		if err := saveCodexTokens(env, map[string]any{"access_token": reauthed, "refresh_token": "rt-reauthed"}, "", "", true, false); err != nil {
			t.Fatal(err)
		}
		return importCodexCLITokens(env)
	}
	if _, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{SkipExpiringRefresh: true}); err == nil {
		t.Fatal("expected an error")
	}
	tokens := singletonTokens(t, h)
	if tokens["access_token"] != reauthed || tokens["refresh_token"] != "rt-reauthed" || len(tokens) != 2 {
		t.Fatalf("re-auth overwritten: %v", tokens)
	}
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
		"version": 1, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"id": "cred-quota", "label": "quota-frozen", "auth_type": "oauth", "priority": 0, "source": "device_code",
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

func TestResolverSelectsEntryWithExpiredMillisecondReset(t *testing.T) {
	h, env := codexTestEnv(t)
	now := nowEpoch()
	store := poolOnlyRateLimitedStore(now)
	main := firstCodexPoolEntry(store)
	main["access_token"] = "tok-main"
	main["last_error_reset_at"] = (now - 3600) * 1000
	reserve := mergeTokens(main, map[string]any{"id": "cred-reserve", "access_token": "tok-reserve", "priority": 1, "last_error_reset_at": now + 886})
	store["credential_pool"].(map[string]any)["openai-codex"] = []any{main, reserve}
	writeAuthStore(t, h, store)
	probeCodexQuotaRestoredFn = func(PoolEnv, string, string) (bool, bool) { return false, true }
	resolved, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved["api_key"] != "tok-main" || resolved["source"] != "credential_pool" {
		t.Fatalf("unexpected result %v", resolved)
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
	stale := firstCodexPoolEntry(store)
	store["providers"] = map[string]any{"openai-codex": map[string]any{"tokens": map[string]any{"access_token": stale["access_token"], "refresh_token": "rf-old"}}}
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
	disk := readAuthStore(t, h)
	if poolRows(t, h, "openai-codex")[0]["refresh_token"] != "rf-new" {
		t.Fatalf("disk pool row %v", poolRows(t, h, "openai-codex")[0])
	}
	if singletonTokens(t, h)["refresh_token"] != "rf-new" {
		t.Fatalf("singleton %v", disk["providers"])
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

func TestProfileRefreshOfRootGrantWritesThroughToRoot(t *testing.T) {
	profile, root, env := profileEnvForTest(t, "work")
	writeAuthStore(t, root, map[string]any{
		"version":         1,
		"providers":       map[string]any{"openai-codex": map[string]any{"auth_mode": "chatgpt", "tokens": tokenPair("old")}},
		"credential_pool": map[string]any{"openai-codex": []any{mergeTokens(map[string]any{"provider": "openai-codex", "source": "device_code"}, tokenPair("old"))}},
	})
	writeAuthStore(t, profile, map[string]any{"version": 1, "providers": map[string]any{}})
	rotated := tokenPair("new")
	if err := saveCodexTokens(env, rotated, "2026-08-16T00:00:00Z", "", true, true); err != nil {
		t.Fatal(err)
	}
	rootStore := readAuthStore(t, root)
	if !config.Equal(providerTokens(rootStore), rotated) {
		t.Fatalf("root singleton %v", providerTokens(rootStore))
	}
	rootRow := poolRows(t, root, "openai-codex")[0]
	if rootRow["refresh_token"] != "new-rt" || rootRow["access_token"] != "new-at" {
		t.Fatalf("root pool row %v", rootRow)
	}
	if providerTokens(readAuthStore(t, profile)) != nil {
		t.Fatal("a profile copy shadows root")
	}
}

func TestProfileOwnedGrantStaysLocal(t *testing.T) {
	profile, root, env := profileEnvForTest(t, "work")
	writeAuthStore(t, profile, map[string]any{"version": 1, "providers": map[string]any{"openai-codex": map[string]any{"auth_mode": "chatgpt", "tokens": tokenPair("prof")}}})
	writeAuthStore(t, root, map[string]any{"version": 1, "providers": map[string]any{}})
	rotated := tokenPair("next")
	if err := saveCodexTokens(env, rotated, "2026-08-16T00:00:00Z", "", true, true); err != nil {
		t.Fatal(err)
	}
	if !config.Equal(providerTokens(readAuthStore(t, profile)), rotated) {
		t.Fatal("profile grant not rotated locally")
	}
	if providerTokens(readAuthStore(t, root)) != nil {
		t.Fatal("root gained a profile grant")
	}
	writeAuthStore(t, profile, map[string]any{"version": 1, "providers": map[string]any{}})
	writeAuthStore(t, root, map[string]any{"version": 1, "providers": map[string]any{"openai-codex": map[string]any{"auth_mode": "chatgpt", "tokens": tokenPair("root")}}})
	if err := saveCodexTokens(env, tokenPair("login"), "", "", true, false); err != nil {
		t.Fatal(err)
	}
	if !config.Equal(providerTokens(readAuthStore(t, profile)), tokenPair("login")) {
		t.Fatal("fresh login not stored in the profile")
	}
	if !config.Equal(providerTokens(readAuthStore(t, root)), tokenPair("root")) {
		t.Fatal("fresh login rewrote root")
	}
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

func TestConcurrentRefreshesOfSharedRootGrantSubmitOldTokenOnce(t *testing.T) {
	profile, root, env := profileEnvForTest(t, "work")
	writeAuthStore(t, root, map[string]any{"version": 1, "providers": map[string]any{"openai-codex": map[string]any{"auth_mode": "chatgpt", "tokens": tokenPair("old")}}})
	writeAuthStore(t, profile, map[string]any{"version": 1, "providers": map[string]any{}})
	endpoint := &rotatingEndpoint{hold: 1500 * time.Millisecond}
	useCodexTransport(t, endpoint)
	results := map[string]map[string]any{}
	errs := map[string]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			out, err := refreshCodexAuthTokens(env, tokenPair("old"), 5*time.Second)
			mu.Lock()
			results[name], errs[name] = out, err
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	for name, err := range errs {
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(endpoint.seen) != 1 || endpoint.seen[0] != "old-rt" {
		t.Fatalf("endpoint saw %v", endpoint.seen)
	}
	for name, out := range results {
		if !config.Equal(out, tokenPair("new")) {
			t.Fatalf("%s got %v", name, out)
		}
	}
	if !config.Equal(providerTokens(readAuthStore(t, root)), tokenPair("new")) {
		t.Fatal("root not rotated")
	}
	if providerTokens(readAuthStore(t, profile)) != nil {
		t.Fatal("profile gained a copy")
	}
}

// ---- relogin copy names the profile ----

func TestMissingAndReusedCodexCredentialCopyNamesTheProfile(t *testing.T) {
	_, _, env := profileEnvForTest(t, "codex")
	_, err := readCodexTokens(env, true)
	missing := authErrorOf(t, err)
	reused := codexRefreshFailureError(&oauthHTTPResponse{Status: 400, Body: []byte(`{"error": "refresh_token_reused", "error_description": "already used"}`)})
	for _, e := range []*AuthError{missing, reused} {
		if !strings.Contains(e.Message, "`icebath -p codex auth add openai-codex --type oauth`") || strings.Contains(e.Message, "`icebath auth`") {
			t.Fatalf("copy does not name the profile: %s", e.Message)
		}
	}
}

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
		"version": 1, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"id": "gw", "label": "gateway", "auth_type": "oauth", "priority": 0, "source": "device_code",
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
	entry := map[string]any{"id": "codex-pool-entry-0", "label": "device_code-0", "auth_type": "oauth", "source": "device_code", "priority": 0,
		"request_count": 0, "access_token": jwtWithClaims(map[string]any{"exp": nowEpoch() - 3600}), "refresh_token": "codex-refresh-token-0",
		"base_url": DefaultCodexBaseURL, "last_status": "exhausted", "last_error_code": 429, "last_error_reason": "usage_limit_reached",
		"last_error_reset_at": nowEpoch() + 3600}
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"openai-codex": []any{entry}}})
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
		"version": 1, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"source": "device_code", "access_token": "pool-fallback-token", "refresh_token": "pool-refresh", "last_status": "ok", "auth_type": "oauth"}}},
	})
	resolved, err := ResolveCodexRuntimeCredentials(env, CodexResolveOptions{SkipExpiringRefresh: true})
	if err != nil || resolved["api_key"] != "pool-fallback-token" || resolved["source"] != "credential_pool" {
		t.Fatalf("%v %v", resolved, err)
	}
}
