package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Isolated icebath home with the provider env vars of the Python autouse fixture cleared
func authTestHome(t *testing.T) (string, PoolEnv) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	for _, key := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "GROQ_API_KEY"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return h, testEnv(t, h)
}

func authTestGroqConfig(t *testing.T, h, providerKey, name, baseURL string) {
	t.Helper()
	if baseURL == "" {
		baseURL = "https://api.groq.com/openai/v1"
	}
	writeConfigYAML(t, h, fmt.Sprintf("providers:\n  %s:\n    name: %s\n    base_url: %s\n    key_env: GROQ_API_KEY\n    discover_models: true\n", providerKey, name, baseURL))
}

func authTestJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func authJWTWithEmail(email string) string { return authTestJWT(map[string]any{"email": email}) }

func authCodexJWT(email, accountID, subject string) string {
	return authTestJWT(map[string]any{"email": email, "sub": subject, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID}})
}

func authCodexPoolOnlyStore(exhausted bool) map[string]any {
	entry := map[string]any{
		"id": "codex-1", "label": "codex@example.com", "auth_type": "oauth", "priority": 0,
		"source": "manual:device_code", "access_token": authJWTWithEmail("codex@example.com"),
		"refresh_token": "refresh-token", "base_url": "https://chatgpt.com/backend-api/codex",
		"last_refresh": "2026-06-15T10:00:00Z"}
	if exhausted {
		now := float64(time.Now().UnixNano()) / 1e9
		entry["last_status"] = "exhausted"
		entry["last_status_at"] = now
		entry["last_error_code"] = 429
		entry["last_error_reason"] = "usage_limit_reached"
		entry["last_error_message"] = "The usage limit has been reached"
		entry["last_error_reset_at"] = now + 3600
	}
	return map[string]any{"version": 1, "active_provider": "openai-codex", "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{entry}}}
}

func authRun(env PoolEnv, ns map[string]any) (string, string, error) {
	var out, errw bytes.Buffer
	cli := &authCLI{ctx: context.Background(), env: env, ns: ns, out: &out, errw: &errw}
	err := cli.run()
	return out.String(), errw.String(), err
}

func authMust(t *testing.T, env PoolEnv, ns map[string]any) (string, string) {
	t.Helper()
	out, errw, err := authRun(env, ns)
	if err != nil {
		t.Fatalf("auth %v failed: %v\nstdout %q\nstderr %q", ns["auth_action"], err, out, errw)
	}
	return out, errw
}

func authPoolEntries(t *testing.T, h, provider string) []map[string]any {
	t.Helper()
	pool, _ := readAuthStore(t, h)["credential_pool"].(map[string]any)
	raw, _ := pool[provider].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(map[string]any))
	}
	return out
}

func authPoolHas(t *testing.T, h, provider string) bool {
	t.Helper()
	pool, _ := readAuthStore(t, h)["credential_pool"].(map[string]any)
	_, ok := pool[provider]
	return ok
}

func authTokenSet(entries []map[string]any) map[string]bool {
	set := map[string]bool{}
	for _, e := range entries {
		set[e["access_token"].(string)] = true
	}
	return set
}

func TestAuthAddApiKeyPersistsManualEntry(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	authMust(t, env, map[string]any{"auth_action": "add", "provider": "openrouter", "auth_type": "api-key", "api_key": "sk-or-manual", "label": "personal"})
	var entry map[string]any
	for _, item := range authPoolEntries(t, h, "openrouter") {
		if item["source"] == "manual" {
			entry = item
		}
	}
	if entry == nil || entry["label"] != "personal" || entry["auth_type"] != "api_key" || entry["source"] != "manual" || entry["access_token"] != "sk-or-manual" {
		t.Fatalf("manual entry %v", entry)
	}
}

func TestAuthAddMigratesLegacyPrefixedKeyForConfiguredProvider(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}, "credential_pool": map[string]any{
		"custom:groq": []any{map[string]any{"id": "legacy-key", "label": "legacy", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "gsk-legacy"}}}})
	authTestGroqConfig(t, h, "groq", "Groq", "")
	authMust(t, env, map[string]any{"auth_action": "add", "provider": "groq", "auth_type": "api-key", "api_key": "gsk-new", "label": "new"})
	if authPoolHas(t, h, "custom:groq") {
		t.Fatal("legacy custom:groq pool survived the migration")
	}
	if got := authTokenSet(authPoolEntries(t, h, "groq")); !reflect.DeepEqual(got, map[string]bool{"gsk-legacy": true, "gsk-new": true}) {
		t.Fatalf("groq tokens %v", got)
	}
}

func TestAuthAddMigratesDisplayNameDerivedLegacyPoolKey(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}, "credential_pool": map[string]any{
		"custom:groq": []any{map[string]any{"id": "legacy-key", "label": "legacy", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "gsk-legacy"}}}})
	authTestGroqConfig(t, h, "groq-cloud", "Groq", "")
	authMust(t, env, map[string]any{"auth_action": "add", "provider": "groq-cloud", "auth_type": "api-key", "api_key": "gsk-new", "label": "new"})
	if authPoolHas(t, h, "custom:groq") {
		t.Fatal("legacy custom:groq pool survived the migration")
	}
	if got := authTokenSet(authPoolEntries(t, h, "groq-cloud")); !reflect.DeepEqual(got, map[string]bool{"gsk-legacy": true, "gsk-new": true}) {
		t.Fatalf("groq-cloud tokens %v", got)
	}
}

func TestAuthAddNonRegistryConfiguredProviderPreservesEndpoint(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	authTestGroqConfig(t, h, "private-groq", "Groq", "https://private.example/v1")
	authMust(t, env, map[string]any{"auth_action": "add", "provider": "private-groq", "auth_type": "api-key", "api_key": "private-key", "label": "private"})
	entries := authPoolEntries(t, h, "private-groq")
	if len(entries) == 0 || entries[0]["base_url"] != "https://private.example/v1" {
		t.Fatalf("private-groq entries %v", entries)
	}
}

func TestAuthListIncludesNonRegistryConfiguredProvider(t *testing.T) {
	h, env := authTestHome(t)
	authTestGroqConfig(t, h, "private-groq", "Groq", "")
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}, "credential_pool": map[string]any{
		"private-groq": []any{map[string]any{"id": "private-key", "label": "private", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "secret"}}}})
	out, _ := authMust(t, env, map[string]any{"auth_action": "list", "provider": nil})
	if !strings.Contains(out, "private-groq") {
		t.Fatalf("list output %q", out)
	}
}

func TestInteractiveAuthAddNormalizesDisplayNameToProviderKey(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	authTestGroqConfig(t, h, "groq-cloud", "Groq Enterprise", "")
	answers := []string{"Groq Enterprise", "primary"}
	prevLine, prevSecret := authCmdLineInputFn, authCmdMaskedSecretPromptFn
	authCmdLineInputFn = func(string) (string, error) {
		next := answers[0]
		answers = answers[1:]
		return next, nil
	}
	authCmdMaskedSecretPromptFn = func(string) (string, error) { return "gsk-test", nil }
	defer func() { authCmdLineInputFn, authCmdMaskedSecretPromptFn = prevLine, prevSecret }()
	var out, errw bytes.Buffer
	cli := &authCLI{ctx: context.Background(), env: env, ns: map[string]any{}, out: &out, errw: &errw}
	if err := cli.interactiveAdd(); err != nil {
		t.Fatalf("interactive add: %v\n%s", err, out.String())
	}
	if authPoolHas(t, h, "custom:groq-enterprise") {
		t.Fatal("display name leaked into a custom: pool key")
	}
	entries := authPoolEntries(t, h, "groq-cloud")
	if len(entries) == 0 || entries[0]["access_token"] != "gsk-test" {
		t.Fatalf("groq-cloud entries %v", entries)
	}
}

func TestAuthAddExplicitCustomProviderKeepsPrefixedPoolKey(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	authTestGroqConfig(t, h, "groq", "Groq Proxy", "https://proxy.example/v1")
	authMust(t, env, map[string]any{"auth_action": "add", "provider": "custom:groq", "auth_type": "api-key", "api_key": "proxy-key", "label": "proxy"})
	if !authPoolHas(t, h, "custom:groq") || authPoolHas(t, h, "groq") {
		t.Fatalf("pool keys %v", readAuthStore(t, h)["credential_pool"])
	}
}

func authNousLoginCreds(token string) map[string]any {
	return map[string]any{
		"portal_base_url": "https://portal.example.com", "inference_base_url": "https://inference.example.com/v1",
		"client_id": "hermes-cli", "scope": "inference:invoke", "token_type": "Bearer",
		"access_token": token, "refresh_token": "refresh-token",
		"obtained_at": "2026-03-23T10:00:00+00:00", "expires_at": "2026-03-23T11:00:00+00:00", "expires_in": 3600,
		"agent_key": token, "agent_key_id": nil, "agent_key_expires_at": "2026-03-23T10:30:00+00:00",
		"agent_key_expires_in": 1800, "agent_key_reused": false, "agent_key_obtained_at": "2026-03-23T10:00:10+00:00",
		"tls": map[string]any{"insecure": false, "ca_bundle": nil}}
}

func authNousOAuthNS(label any) map[string]any {
	return map[string]any{"auth_action": "add", "provider": "nous", "auth_type": "oauth", "api_key": nil, "label": label,
		"portal_url": nil, "inference_url": nil, "client_id": nil, "scope": nil, "no_browser": false, "timeout": nil,
		"insecure": false, "ca_bundle": nil}
}

func TestAuthAddNousOauthPersistsPoolEntry(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	token := authJWTWithEmail("nous@example.com")
	prev := nousDeviceCodeLoginFn
	nousDeviceCodeLoginFn = func(PoolEnv, io.Writer, NousDeviceLoginArgs) (map[string]any, error) {
		return authNousLoginCreds(token), nil
	}
	defer func() { nousDeviceCodeLoginFn = prev }()
	authMust(t, env, authNousOAuthNS(nil))
	entries := authPoolEntries(t, h, "nous")
	var deviceCode []map[string]any
	for _, item := range entries {
		if item["source"] == "manual:device_code" {
			t.Fatalf("duplicate manual:device_code row in %v", entries)
		}
		if item["source"] == "device_code" {
			deviceCode = append(deviceCode, item)
		}
	}
	if len(deviceCode) != 1 {
		t.Fatalf("device_code entries %v", entries)
	}
	entry := deviceCode[0]
	if entry["agent_key"] != token || entry["portal_base_url"] != "https://portal.example.com" {
		t.Fatalf("entry %v", entry)
	}
	singleton, _ := readAuthStore(t, h)["providers"].(map[string]any)["nous"].(map[string]any)
	if singleton["access_token"] != token || singleton["refresh_token"] != "refresh-token" || singleton["agent_key"] != token ||
		singleton["portal_base_url"] != "https://portal.example.com" || singleton["inference_base_url"] != "https://inference.example.com/v1" {
		t.Fatalf("providers.nous %v", singleton)
	}
}

func TestAuthAddNousOauthHonorsCustomLabel(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	token := authJWTWithEmail("nous@example.com")
	prev := nousDeviceCodeLoginFn
	nousDeviceCodeLoginFn = func(PoolEnv, io.Writer, NousDeviceLoginArgs) (map[string]any, error) {
		return authNousLoginCreds(token), nil
	}
	defer func() { nousDeviceCodeLoginFn = prev }()
	authMust(t, env, authNousOAuthNS("my-nous"))
	entries := authPoolEntries(t, h, "nous")
	if len(entries) == 0 || entries[0]["source"] != "device_code" || entries[0]["label"] != "my-nous" {
		t.Fatalf("pool entries %v", entries)
	}
	singleton, _ := readAuthStore(t, h)["providers"].(map[string]any)["nous"].(map[string]any)
	if singleton["label"] != "my-nous" {
		t.Fatalf("providers.nous %v", singleton)
	}
}

// Scripted device code logins handed out in order
func authScriptCodexLogins(t *testing.T, logins []map[string]any) {
	t.Helper()
	prev := codexDeviceCodeLoginFn
	codexDeviceCodeLoginFn = func(PoolEnv, io.Writer) (map[string]any, error) {
		if len(logins) == 0 {
			t.Fatal("codex login called more often than scripted")
		}
		next := logins[0]
		logins = logins[1:]
		return next, nil
	}
	t.Cleanup(func() { codexDeviceCodeLoginFn = prev })
}

func authCodexAddNS() map[string]any {
	return map[string]any{"auth_action": "add", "provider": "openai-codex", "auth_type": "oauth", "api_key": nil, "label": nil}
}

func authEntryField(entries []PooledCredential, pick func(PooledCredential) string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, pick(e))
	}
	return out
}

func TestAuthAddCodexOauthKeepsDistinctPoolAccounts(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	firstToken, secondToken := authJWTWithEmail("first-codex@example.com"), authJWTWithEmail("second-codex@example.com")
	authScriptCodexLogins(t, []map[string]any{
		{"tokens": map[string]any{"access_token": firstToken, "refresh_token": "first-refresh-token"}, "base_url": "https://chatgpt.com/backend-api/codex", "last_refresh": "2026-03-23T10:00:00Z"},
		{"tokens": map[string]any{"access_token": secondToken, "refresh_token": "second-refresh-token"}, "base_url": "https://chatgpt.com/backend-api/codex", "last_refresh": "2026-03-23T10:05:00Z"},
	})
	authMust(t, env, authCodexAddNS())
	authMust(t, env, authCodexAddNS())
	entries := LoadPool(env, "openai-codex").Entries()
	if got := authEntryField(entries, func(e PooledCredential) string { return e.Source }); !reflect.DeepEqual(got, []string{"manual:device_code", "manual:device_code"}) {
		t.Fatalf("sources %v", got)
	}
	if got := authEntryField(entries, func(e PooledCredential) string { return e.Label }); !reflect.DeepEqual(got, []string{"first-codex@example.com", "second-codex@example.com"}) {
		t.Fatalf("labels %v", got)
	}
	if got := authEntryField(entries, func(e PooledCredential) string { return e.AccessToken }); !reflect.DeepEqual(got, []string{firstToken, secondToken}) {
		t.Fatalf("tokens %v", got)
	}
	if got := authEntryField(entries, func(e PooledCredential) string { return e.RefreshToken }); !reflect.DeepEqual(got, []string{"first-refresh-token", "second-refresh-token"}) {
		t.Fatalf("refresh tokens %v", got)
	}
	payload := readAuthStore(t, h)
	if providers, _ := payload["providers"].(map[string]any); providers["openai-codex"] != nil {
		t.Fatalf("singleton block written: %v", providers["openai-codex"])
	}
	if payload["active_provider"] != "openai-codex" {
		t.Fatalf("active_provider %v", payload["active_provider"])
	}
}

func authAddCodexTwice(t *testing.T, env PoolEnv, h, secondToken string) string {
	t.Helper()
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	authScriptCodexLogins(t, []map[string]any{
		{"tokens": map[string]any{"access_token": authCodexJWT("me@example.com", "acct-A", "user-1"), "refresh_token": "rt-1"}, "base_url": "https://chatgpt.com/backend-api/codex", "last_refresh": "2026-09-01T00:00:00Z"},
		{"tokens": map[string]any{"access_token": secondToken, "refresh_token": "rt-2"}, "base_url": "https://chatgpt.com/backend-api/codex", "last_refresh": "2026-09-01T00:00:00Z"},
	})
	authMust(t, env, authCodexAddNS())
	_, errw := authMust(t, env, authCodexAddNS())
	return errw
}

func TestAuthAddCodexWarnsWhenLoginIsSameAccountAsPooledEntry(t *testing.T) {
	h, env := authTestHome(t)
	errw := authAddCodexTwice(t, env, h, authCodexJWT("me@example.com", "acct-A", "user-1"))
	if !strings.Contains(errw, "me@example.com") {
		t.Fatalf("stderr %q", errw)
	}
	if got := len(LoadPool(env, "openai-codex").Entries()); got != 2 {
		t.Fatalf("the warning must not block the add, entries %d", got)
	}
}

func TestCodexAuthStatusReportsPoolOnlyCredential(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, authCodexPoolOnlyStore(false))
	status := GetCodexAuthStatus(env)
	if status["logged_in"] != true || status["source"] != "pool:codex@example.com" {
		t.Fatalf("status %v", status)
	}
}

func TestCodexRuntimePoolOnlyRateLimitIsNotMissingAuth(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, authCodexPoolOnlyStore(true))
	_, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected AuthError, got %v", err)
	}
	if authErr.Code != CodexRateLimitedCode || authErr.ReloginRequired {
		t.Fatalf("code %q relogin %v", authErr.Code, authErr.ReloginRequired)
	}
}

func TestAuthAddXaiOauthKeepsDistinctPoolAccounts(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	firstToken, secondToken := "xai-access-token-account-a", "xai-access-token-account-b"
	logins := []map[string]any{
		{"tokens": map[string]any{"access_token": firstToken, "refresh_token": "first-xai-refresh", "id_token": "", "token_type": "Bearer"},
			"discovery": map[string]any{"token_endpoint": "https://auth.x.ai/token"}, "redirect_uri": "", "base_url": "https://api.x.ai/v1",
			"last_refresh": "2026-07-10T10:00:00Z", "source": "oauth-device-code"},
		{"tokens": map[string]any{"access_token": secondToken, "refresh_token": "second-xai-refresh", "id_token": "", "token_type": "Bearer"},
			"discovery": map[string]any{"token_endpoint": "https://auth.x.ai/token"}, "redirect_uri": "", "base_url": "https://api.x.ai/v1",
			"last_refresh": "2026-07-10T10:05:00Z", "source": "oauth-device-code"},
	}
	prev := xaiOAuthDeviceCodeLoginFn
	xaiOAuthDeviceCodeLoginFn = func(PoolEnv, io.Writer, time.Duration, bool) (map[string]any, error) {
		next := logins[0]
		logins = logins[1:]
		return next, nil
	}
	defer func() { xaiOAuthDeviceCodeLoginFn = prev }()
	base := map[string]any{"auth_action": "add", "provider": "xai-oauth", "auth_type": "oauth", "api_key": nil, "timeout": nil, "no_browser": false}
	nsA, nsB := map[string]any{}, map[string]any{}
	for k, v := range base {
		nsA[k], nsB[k] = v, v
	}
	nsA["label"], nsB["label"] = "xai-heavy", "xai-premium"
	authMust(t, env, nsA)
	authMust(t, env, nsB)
	entries := LoadPool(env, "xai-oauth").Entries()
	if got := authEntryField(entries, func(e PooledCredential) string { return e.Source }); !reflect.DeepEqual(got, []string{"manual:device_code", "manual:device_code"}) {
		t.Fatalf("sources %v", got)
	}
	if got := authEntryField(entries, func(e PooledCredential) string { return e.Label }); !reflect.DeepEqual(got, []string{"xai-heavy", "xai-premium"}) {
		t.Fatalf("labels %v", got)
	}
	if got := authEntryField(entries, func(e PooledCredential) string { return e.AccessToken }); !reflect.DeepEqual(got, []string{firstToken, secondToken}) {
		t.Fatalf("tokens %v", got)
	}
	if got := authEntryField(entries, func(e PooledCredential) string { return e.RefreshToken }); !reflect.DeepEqual(got, []string{"first-xai-refresh", "second-xai-refresh"}) {
		t.Fatalf("refresh tokens %v", got)
	}
	payload := readAuthStore(t, h)
	if providers, _ := payload["providers"].(map[string]any); providers["xai-oauth"] != nil {
		t.Fatalf("singleton block written: %v", providers["xai-oauth"])
	}
	if payload["active_provider"] != "xai-oauth" {
		t.Fatalf("active_provider %v", payload["active_provider"])
	}
}

func TestAuthRemoveReindexesPriorities(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"anthropic": []any{
		map[string]any{"id": "cred-1", "label": "primary", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-ant-api-primary"},
		map[string]any{"id": "cred-2", "label": "secondary", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": "sk-ant-api-secondary"},
	}}})
	authMust(t, env, map[string]any{"auth_action": "remove", "provider": "anthropic", "target": "1"})
	entries := authPoolEntries(t, h, "anthropic")
	if len(entries) != 1 || entries[0]["label"] != "secondary" || entries[0]["priority"] != float64(0) {
		t.Fatalf("entries %v", entries)
	}
}

func TestAuthRemoveCodexMigratesLegacyDictSuppression(t *testing.T) {
	h, env := authTestHome(t)
	store := authCodexPoolOnlyStore(false)
	primary := store["credential_pool"].(map[string]any)["openai-codex"].([]any)[0].(map[string]any)
	primary["id"], primary["label"] = "codex-qb", "qb"
	store["suppressed_sources"] = map[string]any{"openai-codex": map[string]any{"legacy": true}}
	writeAuthStore(t, h, store)
	authMust(t, env, map[string]any{"auth_action": "remove", "provider": "openai-codex", "target": "qb"})
	payload := readAuthStore(t, h)
	if entries := authPoolEntries(t, h, "openai-codex"); len(entries) != 0 {
		t.Fatalf("pool entries left %v", entries)
	}
	suppressed, _ := payload["suppressed_sources"].(map[string]any)
	if !reflect.DeepEqual(suppressed["openai-codex"], []any{"legacy", "device_code", "manual:device_code"}) {
		t.Fatalf("suppressed_sources %v", suppressed)
	}
	if _, ok := LoadPool(env, "openai-codex").Peek(); ok {
		t.Fatal("a removed credential came back through load_pool")
	}
}

func TestClearProviderAuthRemovesProviderPoolEntries(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "anthropic",
		"providers": map[string]any{"anthropic": map[string]any{"access_token": "legacy-token"}},
		"credential_pool": map[string]any{
			"anthropic":  []any{map[string]any{"id": "cred-1", "label": "primary", "auth_type": "oauth", "priority": 0, "source": "manual:hermes_pkce", "access_token": "pool-token"}},
			"openrouter": []any{map[string]any{"id": "cred-2", "label": "other-provider", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-or-test"}}}})
	cleared, err := env.Store.ClearProviderAuth("anthropic")
	if err != nil || !cleared {
		t.Fatalf("cleared %v err %v", cleared, err)
	}
	payload := readAuthStore(t, h)
	providers, _ := payload["providers"].(map[string]any)
	if payload["active_provider"] != nil || providers["anthropic"] != nil || authPoolHas(t, h, "anthropic") || !authPoolHas(t, h, "openrouter") {
		t.Fatalf("payload %v", payload)
	}
}

func TestUnsuppressCredentialSourceClearsMarker(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1})
	if err := env.Store.SuppressCredentialSource("openai-codex", "device_code"); err != nil {
		t.Fatal(err)
	}
	if !env.Store.IsSourceSuppressed("openai-codex", "device_code") {
		t.Fatal("marker not set")
	}
	cleared, err := env.Store.UnsuppressCredentialSource("openai-codex", "device_code")
	if err != nil || !cleared || env.Store.IsSourceSuppressed("openai-codex", "device_code") {
		t.Fatalf("cleared %v err %v suppressed %v", cleared, err, env.Store.IsSourceSuppressed("openai-codex", "device_code"))
	}
	if _, ok := readAuthStore(t, h)["suppressed_sources"]; ok {
		t.Fatal("empty suppressed_sources dict should be cleaned up entirely")
	}
}

func TestUnsuppressCredentialSourcePreservesOtherMarkers(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1})
	for _, pair := range [][2]string{{"openai-codex", "device_code"}, {"anthropic", "claude_code"}} {
		if err := env.Store.SuppressCredentialSource(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	if cleared, err := env.Store.UnsuppressCredentialSource("openai-codex", "device_code"); err != nil || !cleared {
		t.Fatalf("cleared %v err %v", cleared, err)
	}
	if !env.Store.IsSourceSuppressed("anthropic", "claude_code") {
		t.Fatal("unrelated marker was dropped")
	}
}

func TestSeedFromSingletonsRespectsHermesPkceSuppression(t *testing.T) {
	h, env := authTestHome(t)
	writeConfigYAML(t, h, "model:\n  provider: anthropic\n  model: claude\n")
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}, "suppressed_sources": map[string]any{"anthropic": []any{"hermes_pkce"}}})
	raw, _ := json.Marshal(map[string]any{"accessToken": "tok", "refreshToken": "r", "expiresAt": 9999999999000})
	if err := os.WriteFile(filepath.Join(h, ".anthropic_oauth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var entries []PooledCredential
	_, active := seedFromSingletons(env, "anthropic", &entries)
	if len(entries) != 0 {
		t.Fatalf("suppressed hermes_pkce seeded %v", entries)
	}
	if active["hermes_pkce"] {
		t.Fatalf("active sources %v", active)
	}
}

func TestAuthRemoveEnvSeededDotenvWithBomNoShellHint(t *testing.T) {
	h, env := authTestHome(t)
	if err := os.WriteFile(filepath.Join(h, ".env"), append([]byte{0xef, 0xbb, 0xbf}, []byte("DEEPSEEK_API_KEY=sk-ds-only\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "sk-ds-only")
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"deepseek": []any{
		map[string]any{"id": "env-1", "label": "DEEPSEEK_API_KEY", "auth_type": "api_key", "priority": 0, "source": "env:DEEPSEEK_API_KEY", "access_token": "sk-ds-only"}}}})
	out, _ := authMust(t, env, map[string]any{"auth_action": "remove", "provider": "deepseek", "target": "1"})
	dotenv, err := os.ReadFile(filepath.Join(h, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dotenv), "DEEPSEEK_API_KEY") {
		t.Fatalf(".env still holds the key: %q", dotenv)
	}
	if strings.Contains(out, "still set in your shell environment") {
		t.Fatalf("phantom shell hint: %q", out)
	}
}

func TestAuthAddOpenrouterOauthPersistsPkceKeyWithoutTouchingApiKeyDefault(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	prev := authCmdOpenrouterPKCELoginFn
	authCmdOpenrouterPKCELoginFn = func(PoolEnv, io.Writer, bool, time.Duration) (map[string]any, error) {
		return map[string]any{"api_key": "sk-or-v1-from-pkce"}, nil
	}
	defer func() { authCmdOpenrouterPKCELoginFn = prev }()
	authMust(t, env, map[string]any{"auth_action": "add", "provider": "openrouter", "auth_type": "oauth", "api_key": nil, "label": "browser-login", "timeout": nil, "no_browser": true})
	if resolved, err := resolveProvider(env, "auto"); err != nil || resolved != "openrouter" {
		t.Fatalf("resolve_provider(auto) %q %v", resolved, err)
	}
	authMust(t, env, map[string]any{"auth_action": "add", "provider": "openrouter", "auth_type": nil, "api_key": "sk-or-v1-pasted", "label": "pasted"})
	bySource := map[string]map[string]any{}
	for _, e := range authPoolEntries(t, h, "openrouter") {
		bySource[e["source"].(string)] = e
	}
	pkce := bySource["manual:openrouter_pkce"]
	if pkce == nil || pkce["auth_type"] != "api_key" || pkce["access_token"] != "sk-or-v1-from-pkce" || pkce["base_url"] != "https://openrouter.ai/api/v1" {
		t.Fatalf("pkce row %v", pkce)
	}
	if bySource["manual"] == nil || bySource["manual"]["access_token"] != "sk-or-v1-pasted" {
		t.Fatalf("manual row %v", bySource["manual"])
	}
}

func TestOpenrouterLoopbackCallbackBindsNoncePathAndRejectsForgedRedirect(t *testing.T) {
	t.Setenv("BROWSER", "firefox")
	t.Setenv("DISPLAY", ":0")
	var mu sync.Mutex
	seen := map[string]any{}
	done := make(chan struct{})
	prev := openBrowserFn
	openBrowserFn = func(rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		callback := parsed.Query().Get("callback_url")
		mu.Lock()
		seen["callback"] = callback
		mu.Unlock()
		forged := callback[:strings.LastIndex(callback, "/")] + "/forged-nonce?code=evil"
		go func() {
			defer close(done)
			client := &http.Client{Timeout: 5 * time.Second}
			if resp, err := client.Get(forged); err == nil {
				resp.Body.Close()
				mu.Lock()
				seen["forged_status"] = resp.StatusCode
				mu.Unlock()
			}
			if resp, err := client.Get(callback + "?code=good-code"); err == nil {
				resp.Body.Close()
				mu.Lock()
				seen["genuine_status"] = resp.StatusCode
				mu.Unlock()
			}
		}()
		return nil
	}
	defer func() { openBrowserFn = prev }()
	code, err := openrouterLoopbackCode(io.Discard, [][2]string{{"code_challenge", "c"}, {"code_challenge_method", "S256"}}, true, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	<-done
	mu.Lock()
	defer mu.Unlock()
	parsed, err := url.Parse(seen["callback"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hostname() != "127.0.0.1" || !strings.HasPrefix(parsed.Path, "/callback/") || len(parsed.Path) <= 20 {
		t.Fatalf("callback %q", seen["callback"])
	}
	if seen["forged_status"] != http.StatusNotFound || seen["genuine_status"] != http.StatusOK || code != "good-code" {
		t.Fatalf("forged %v genuine %v code %q", seen["forged_status"], seen["genuine_status"], code)
	}
}

// Records one JSON line per dispatch like the fixture plugin so the test can prove action and args arrived
func authSeamHandler(logPath string) func(action string, args []string, provider string) (bool, error) {
	return func(action string, args []string, provider string) (bool, error) {
		record := map[string]any{"action": action, "provider": nil, "label": nil, "target": nil, "api_key": nil}
		positionals := 0
		for i := 0; i < len(args); i++ {
			switch {
			case args[i] == "--label" && i+1 < len(args):
				i++
				record["label"] = args[i]
			case args[i] == "--api-key" && i+1 < len(args):
				i++
				record["api_key"] = args[i]
			case strings.HasPrefix(args[i], "--"):
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
					i++
				}
			case positionals == 0:
				record["provider"] = args[i]
				positionals++
			default:
				record["target"] = args[i]
			}
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return false, err
		}
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return false, err
		}
		defer f.Close()
		_, err = f.Write(append(raw, '\n'))
		return true, err
	}
}

func authSeamLog(t *testing.T, logPath string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		out = append(out, record)
	}
	return out
}

// Argparse namespace of icebath auth <argv> as the real subcommand parser would build it
func authParseArgs(t *testing.T, argv []string) map[string]any {
	t.Helper()
	ns := map[string]any{"command": "auth", "auth_action": argv[0], "provider": nil, "auth_type": nil, "label": nil,
		"priority": nil, "api_key": nil, "portal_url": nil, "inference_url": nil, "client_id": nil, "scope": nil,
		"no_browser": false, "browser": false, "timeout": nil, "insecure": false, "ca_bundle": nil, "target": nil}
	positionals := []string{"provider", "target"}
	for i := 1; i < len(argv); i++ {
		if strings.HasPrefix(argv[i], "--") {
			key := strings.ReplaceAll(strings.TrimPrefix(argv[i], "--"), "-", "_")
			if key == "type" {
				key = "auth_type"
			}
			i++
			ns[key] = argv[i]
			continue
		}
		if len(positionals) == 0 {
			t.Fatalf("unexpected positional %q", argv[i])
		}
		ns[positionals[0]] = argv[i]
		positionals = positionals[1:]
	}
	return ns
}

func TestOauthPluginOwnsEveryAuthAction(t *testing.T) {
	cases := []struct {
		argv     []string
		expected map[string]any
	}{
		{[]string{"add", "fake-auth", "--label", "work", "--api-key", "sk-fixture"},
			map[string]any{"action": "add", "provider": "fake-auth", "label": "work", "target": nil, "api_key": "sk-fixture"}},
		{[]string{"status", "fake-auth"},
			map[string]any{"action": "status", "provider": "fake-auth", "label": nil, "target": nil, "api_key": nil}},
		{[]string{"logout", "fake-auth"},
			map[string]any{"action": "logout", "provider": "fake-auth", "label": nil, "target": nil, "api_key": nil}},
		{[]string{"refresh", "fake-auth", "acct-2"},
			map[string]any{"action": "refresh", "provider": "fake-auth", "label": nil, "target": "acct-2", "api_key": nil}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			h, env := authTestHome(t)
			logPath := filepath.Join(t.TempDir(), "auth-log.jsonl")
			registerTestProfile(t, Profile{Name: "fake-auth", AuthType: AuthTypeOAuthExternal, BaseURL: "https://example.invalid/v1", AuthHandler: authSeamHandler(logPath)})
			if resolved, err := resolveProvider(env, "fake-auth"); err != nil || resolved != "fake-auth" {
				t.Fatalf("resolve_provider %q %v", resolved, err)
			}
			if p, ok := registryProfileCtx(context.Background(), "fake-auth"); !ok || p.AuthType != "oauth_external" {
				t.Fatalf("registry row %v %v", p.AuthType, ok)
			}
			var out, errw bytes.Buffer
			code, err := AuthCommand(context.Background(), env, authParseArgs(t, tc.argv), &out, &errw)
			if err != nil || code != 0 {
				t.Fatalf("code %d err %v stderr %q", code, err, errw.String())
			}
			if got := authSeamLog(t, logPath); !reflect.DeepEqual(got, []map[string]any{tc.expected}) {
				t.Fatalf("log %v want %v", got, tc.expected)
			}
			if out.String() != "" {
				t.Fatalf("stdout %q", out.String())
			}
			if _, err := os.Stat(filepath.Join(h, "auth.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("auth.json written: %v", err)
			}
		})
	}
}

func TestOauthPluginWithoutHandlerFailsLoudAndBuiltinsAreUntouched(t *testing.T) {
	h, env := authTestHome(t)
	logPath := filepath.Join(t.TempDir(), "auth-log.jsonl")
	registerTestProfile(t, Profile{Name: "handlerless", AuthType: AuthTypeOAuthExternal, BaseURL: "https://example.invalid/v1"})
	_, _, err := authRun(env, authParseArgs(t, []string{"add", "handlerless"}))
	if err == nil || !strings.Contains(err.Error(), "ships no auth_handler") || !strings.Contains(err.Error(), "oauth_external") {
		t.Fatalf("handlerless add error %v", err)
	}
	if _, _, ok := authSystemExit(err); !ok {
		t.Fatalf("a missing handler must exit like SystemExit, got %T", err)
	}
	authMust(t, env, authParseArgs(t, []string{"add", "openrouter", "--api-key", "sk-or-fixture", "--label", "personal"}))
	if got := authSeamLog(t, logPath); len(got) != 0 {
		t.Fatalf("built in provider consulted the seam: %v", got)
	}
	var found map[string]any
	for _, e := range authPoolEntries(t, h, "openrouter") {
		if e["access_token"] == "sk-or-fixture" {
			found = e
		}
	}
	if found == nil || found["label"] != "personal" {
		t.Fatalf("openrouter pool %v", authPoolEntries(t, h, "openrouter"))
	}
}
