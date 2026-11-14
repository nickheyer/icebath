package provider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

func registerAcmeACP(t *testing.T) {
	t.Helper()
	for _, v := range []string{"ICEBATH_ACME_ACP_COMMAND", "ICEBATH_ACME_ACP_ARGS", "ACME_ACP_BASE_URL"} {
		t.Setenv(v, "")
	}
	registerTestProfile(t, acmeACPProfile())
}

func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGetAuthStatusDispatchesExternalProcessByAuthType(t *testing.T) {
	h, env := codexTestEnv(t)
	registerAcmeACP(t)
	fake := writeExecutable(t, filepath.Join(filepath.Dir(h), "acme"))
	t.Setenv("ICEBATH_ACME_ACP_COMMAND", fake)
	status := GetAuthStatus(env, "acme-acp")
	if status["provider"] != "acme-acp" || status["configured"] != true || status["resolved_command"] != fake {
		t.Fatalf("unexpected status %v", status)
	}
	if _, has := status["auth_verified"]; !has {
		t.Fatal("auth_verified missing")
	}
}

func TestExternalProcessStatusFindsUserLocalCLIOutsideBackendPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix executable bits")
	}
	h, env := codexTestEnv(t)
	registerAcmeACP(t)
	userHome := filepath.Join(filepath.Dir(h), "userhome")
	cli := writeExecutable(t, filepath.Join(userHome, ".local", "bin", "acme"))
	t.Setenv("HOME", userHome)
	t.Setenv("PATH", filepath.Join(filepath.Dir(h), "backend-bin"))
	status := GetExternalProcessProviderStatus(env, "acme-acp")
	if status["configured"] != true || status["resolved_command"] != cli {
		t.Fatalf("unexpected status %v", status)
	}
	rt, err := authResolver(env).resolveExternalProcessRuntime("acme-acp", "acme-acp")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Command != cli {
		t.Fatalf("runtime command %q", rt.Command)
	}
}

func TestExternalProcessStatusResolvesRelativeConfiguredPathFromCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix executable bits")
	}
	h, env := codexTestEnv(t)
	registerAcmeACP(t)
	base := filepath.Dir(h)
	cli := writeExecutable(t, filepath.Join(base, "bin", "acme"))
	t.Chdir(base)
	t.Setenv("PATH", filepath.Join(base, "backend-bin"))
	t.Setenv("ICEBATH_ACME_ACP_COMMAND", filepath.Join(".", "bin", "acme"))
	status := GetExternalProcessProviderStatus(env, "acme-acp")
	if status["configured"] != true || status["resolved_command"] != cli {
		t.Fatalf("unexpected status %v", status)
	}
}

func TestExternalProcessStatusRejectsWrongAuthType(t *testing.T) {
	_, env := codexTestEnv(t)
	for _, name := range []string{"openrouter", "no-such-provider", "anthropic"} {
		status := GetExternalProcessProviderStatus(env, name)
		if len(status) != 1 || status["configured"] != false {
			t.Fatalf("%s: %v", name, status)
		}
	}
}

func TestAuthVerifiedFalseWithoutEvidence(t *testing.T) {
	h, env := codexTestEnv(t)
	registerAcmeACP(t)
	t.Setenv("HOME", filepath.Join(filepath.Dir(h), "userhome"))
	t.Setenv("PATH", filepath.Join(filepath.Dir(h), "backend-bin"))
	status := GetExternalProcessProviderStatus(env, "acme-acp")
	if status["auth_verified"] != false || status["auth_source"] != nil || status["configured"] != false {
		t.Fatalf("unexpected status %v", status)
	}
	evidenced := acmeACPProfile()
	evidenced.ExternalAuthEvidenceHook = func() (bool, string, bool) { return true, "~/.acme/credentials.json", true }
	RegisterProvider(evidenced)
	status = GetExternalProcessProviderStatus(env, "acme-acp")
	if status["auth_verified"] != true || status["auth_source"] != "~/.acme/credentials.json" {
		t.Fatalf("evidence ignored %v", status)
	}
}

func TestGetAuthStatusFallsThroughForUnknownAndEmpty(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	if status := GetAuthStatus(env, ""); status["logged_in"] != false {
		t.Fatalf("empty target %v", status)
	}
	if status := GetAuthStatus(env, "no-such-provider"); status["logged_in"] != false {
		t.Fatalf("unknown target %v", status)
	}
	if status := GetAuthStatus(env, "minimax-oauth"); status["provider"] != "minimax-oauth" || status["logged_in"] != false {
		t.Fatalf("bespoke dispatch %v", status)
	}
}

// ---- codex status carries the pool entry route ----

func writeGatewayPool(t *testing.T, h, key, rowBase string) {
	t.Helper()
	writeAuthStore(t, h, map[string]any{
		"version": 1, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai-codex": []any{map[string]any{
			"id": "gw", "label": "gateway", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": key, "base_url": rowBase}}},
	})
}

func TestSetupFlowStatusCarriesThePoolEntryRoute(t *testing.T) {
	h, env := codexTestEnv(t)
	writeConfigYAML(t, h, "model:\n  provider: openai-codex\n  default: gpt-5.5\n  base_url: "+testGW+"\n")
	writeGatewayPool(t, h, "dummy-gateway-pool-key", DefaultCodexBaseURL)
	status := GetCodexAuthStatus(env)
	if status["api_key"] != "dummy-gateway-pool-key" || status["base_url"] != testGW {
		t.Fatalf("unexpected status %v", status)
	}
}

func TestSetupFlowStatusCarriesAPoolRowOwnGatewayURL(t *testing.T) {
	h, env := codexTestEnv(t)
	writeConfigYAML(t, h, "model:\n  provider: openai-codex\n  default: gpt-5.5\n")
	writeGatewayPool(t, h, "dummy-gateway-pool-key", testOtherGW)
	if status := GetCodexAuthStatus(env); status["base_url"] != testOtherGW {
		t.Fatalf("unexpected status %v", status)
	}
}

// ---- status snapshots observe the pool ----

func poolOnlyCodexHome(t *testing.T, accessTokens []string) (string, PoolEnv, *[]string) {
	t.Helper()
	h, env := codexTestEnv(t)
	entries := make([]any, 0, len(accessTokens))
	for i, token := range accessTokens {
		entries = append(entries, map[string]any{
			"id": "codex-pool-entry-" + string(rune('0'+i)), "label": "device_code-" + string(rune('0'+i)), "auth_type": "oauth", "source": "device_code",
			"priority": i, "request_count": 0, "access_token": token, "refresh_token": "codex-refresh-token-" + string(rune('0'+i)), "base_url": DefaultCodexBaseURL})
	}
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"openai-codex": entries}})
	refreshCalls := &[]string{}
	transient := func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		*refreshCalls = append(*refreshCalls, refreshToken)
		return nil, codexErr("Codex token refresh failed with status 503.", "codex_refresh_failed", false)
	}
	prev := tokensSingletonProviders["openai-codex"]
	spec := prev
	spec.refresh = transient
	tokensSingletonProviders["openai-codex"] = spec
	codexRefreshPureFn = transient
	t.Cleanup(func() { tokensSingletonProviders["openai-codex"] = prev })
	return h, env, refreshCalls
}

func jwtExpiringIn(offset float64) string {
	return jwtWithClaims(map[string]any{"exp": nowEpoch() + offset})
}

func TestStatusSnapshotDoesNotRefreshOrBenchAnExpiringPoolEntry(t *testing.T) {
	h, env, refreshCalls := poolOnlyCodexHome(t, []string{jwtExpiringIn(-3600)})
	status := GetCodexAuthStatus(env)
	if len(*refreshCalls) != 0 {
		t.Fatal("a status read spent the single use pool refresh token")
	}
	if status["logged_in"] != true {
		t.Fatalf("status %v", status)
	}
	for _, row := range poolRows(t, h, "openai-codex") {
		if row["last_status"] != nil {
			t.Fatalf("status read benched the row: %v", row)
		}
	}
	if !LoadPool(env, "openai-codex").HasAvailable("") {
		t.Fatal("pool no longer available after a status read")
	}
	LoadPool(env, "openai-codex").Select("")
	if len(*refreshCalls) != 1 || (*refreshCalls)[0] != "codex-refresh-token-0" {
		t.Fatalf("runtime lease did not refresh: %v", *refreshCalls)
	}
}

func TestStatusSnapshotLeavesRoundRobinOrderAndCountsUntouched(t *testing.T) {
	h, env, _ := poolOnlyCodexHome(t, []string{jwtExpiringIn(3600), jwtExpiringIn(3600)})
	writeConfigYAML(t, h, "credential_pool_strategies:\n  openai-codex: round_robin\n")
	before, _ := os.ReadFile(filepath.Join(h, "auth.json"))
	if GetCodexAuthStatus(env)["logged_in"] != true {
		t.Fatal("not logged in")
	}
	after, _ := os.ReadFile(filepath.Join(h, "auth.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("a status read rotated or re-counted the persisted pool")
	}
	if _, ok := LoadPool(env, "openai-codex").Select(""); !ok {
		t.Fatal("runtime selection failed")
	}
	rotated, _ := os.ReadFile(filepath.Join(h, "auth.json"))
	if bytes.Equal(before, rotated) {
		t.Fatal("a runtime selection did not persist the new order")
	}
}

func singletonOnlyCodexHome(t *testing.T, tokens, cliTokens map[string]any) (string, PoolEnv) {
	t.Helper()
	h, env := codexTestEnv(t)
	seedCodexHomes(t, h, tokens, cliTokens)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "openai-codex", "providers": map[string]any{"openai-codex": map[string]any{"tokens": tokens, "auth_mode": "chatgpt"}}})
	return h, env
}

func TestStatusSnapshotNeverAdoptsCodexCLITokens(t *testing.T) {
	stale := map[string]any{"access_token": jwtExpiringIn(-60)}
	h, env := singletonOnlyCodexHome(t, stale, map[string]any{"access_token": jwtExpiringIn(86400), "refresh_token": "cli-refresh"})
	GetCodexAuthStatus(env)
	if !config.Equal(singletonTokens(t, h), stale) {
		t.Fatal("a status read persisted the Codex CLI login into auth.json")
	}
	resolved, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved["source"] != "icebath-auth-store" || singletonTokens(t, h)["refresh_token"] != "cli-refresh" {
		t.Fatalf("runtime resolver did not self heal: %v %v", resolved, singletonTokens(t, h))
	}
}

func TestStatusSnapshotNeverRefreshesAnExpiredSingleton(t *testing.T) {
	expired := map[string]any{"access_token": jwtExpiringIn(-60), "refresh_token": "singleton-refresh"}
	h, env := singletonOnlyCodexHome(t, expired, map[string]any{})
	var refreshCalls []string
	rotate := func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		refreshCalls = append(refreshCalls, refreshToken)
		return map[string]any{"access_token": jwtExpiringIn(86400), "refresh_token": "rotated-refresh"}, nil
	}
	codexRefreshPureFn = rotate
	prev := tokensSingletonProviders["openai-codex"]
	spec := prev
	spec.refresh = rotate
	tokensSingletonProviders["openai-codex"] = spec
	t.Cleanup(func() { tokensSingletonProviders["openai-codex"] = prev })
	status := GetCodexAuthStatus(env)
	if len(refreshCalls) != 0 {
		t.Fatal("a status read spent the single use singleton refresh token")
	}
	if status["logged_in"] != true || status["api_key"] != expired["access_token"] || status["source"] != "icebath-auth-store" {
		t.Fatalf("status %v", status)
	}
	if !config.Equal(singletonTokens(t, h), expired) {
		t.Fatal("status read rewrote the singleton")
	}
	if _, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{ForceRefresh: true, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if len(refreshCalls) != 0 || !config.Equal(singletonTokens(t, h), expired) {
		t.Fatal("read only did not win over force refresh")
	}
	if _, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(refreshCalls) != 1 || refreshCalls[0] != "singleton-refresh" || singletonTokens(t, h)["refresh_token"] != "rotated-refresh" {
		t.Fatalf("runtime path did not rotate: %v %v", refreshCalls, singletonTokens(t, h))
	}
}

func TestStatusSnapshotLeavesTheAuthStoreManifestByteIdentical(t *testing.T) {
	expired := map[string]any{"access_token": jwtExpiringIn(-60), "refresh_token": "singleton-refresh"}
	h, env := singletonOnlyCodexHome(t, expired, map[string]any{})
	GetCodexAuthStatus(env)
	_ = os.Remove(filepath.Join(h, "auth.lock"))
	manifest := map[string][]byte{}
	entries, _ := os.ReadDir(h)
	for _, e := range entries {
		if !e.IsDir() {
			raw, _ := os.ReadFile(filepath.Join(h, e.Name()))
			manifest[e.Name()] = raw
		}
	}
	status := GetCodexAuthStatus(env)
	if status["source"] != "icebath-auth-store" {
		t.Fatalf("status %v", status)
	}
	entries, _ = os.ReadDir(h)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(h, e.Name()))
		if prev, ok := manifest[e.Name()]; !ok || !bytes.Equal(prev, raw) {
			t.Fatalf("%s changed on a status read", e.Name())
		}
	}
}

// ---- nous status cache ----

func TestGetNousAuthStatusCachesConsecutiveCalls(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{"providers": map[string]any{}})
	InvalidateNousAuthStatusCache()
	t.Cleanup(InvalidateNousAuthStatusCache)
	calls := 0
	prev := computeNousAuthStatusFn
	computeNousAuthStatusFn = func(PoolEnv) map[string]any {
		calls++
		return map[string]any{"logged_in": false, "source": "auth_store", "call": calls}
	}
	t.Cleanup(func() { computeNousAuthStatusFn = prev })
	first := GetNousAuthStatus(env)
	second := GetNousAuthStatus(env)
	third := GetNousAuthStatus(env)
	if calls != 1 {
		t.Fatalf("compute called %d times", calls)
	}
	if !config.Equal(first, second) || !config.Equal(second, third) {
		t.Fatal("cached snapshots differ")
	}
	first["mutated"] = true
	if _, has := GetNousAuthStatus(env)["mutated"]; has {
		t.Fatal("caller mutation leaked into the cache")
	}
	writeAuthStore(t, h, map[string]any{"providers": map[string]any{"nous": map[string]any{}}})
	touched := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(filepath.Join(h, "auth.json"), touched, touched)
	GetNousAuthStatus(env)
	if calls != 2 {
		t.Fatalf("a store rewrite did not invalidate the memo, calls %d", calls)
	}
}

// ---- oauth flow table ----

func TestOAuthProviderFlowTerminalRefreshCodes(t *testing.T) {
	dead := nousErr("dead", "invalid_grant", true)
	if !isTerminalNousRefreshError(dead) || !isTerminalRefreshError(dead, "nous") {
		t.Fatal("nous invalid_grant must be terminal")
	}
	if !isTerminalNousRefreshError(nousErr("no login", "nous_auth_missing", true)) {
		t.Fatal("nous missing state must be terminal")
	}
	if isTerminalNousRefreshError(nousErr("dead", "invalid_grant", false)) {
		t.Fatal("relogin not required is never terminal")
	}
	if isTerminalCodexOAuthRefreshError(dead) {
		t.Fatal("a nous error is not a codex terminal error")
	}
	if !isTerminalCodexOAuthRefreshError(codexErr("x", "codex_auth_missing_refresh_token", true)) {
		t.Fatal("codex missing refresh token must be terminal")
	}
	if !isTerminalXAIOAuthRefreshError(xaiErr("x", "xai_refresh_failed", true)) || isTerminalXAIOAuthRefreshError(xaiErr("x", "invalid_grant", true)) {
		t.Fatal("xai terminal codes are its own two")
	}
	if isTerminalCodexOAuthRefreshError(nil) || isTerminalCodexOAuthRefreshError(os.ErrNotExist) {
		t.Fatal("non auth errors are never terminal")
	}
	for _, id := range []string{"nous", "openai-codex", "xai-oauth"} {
		if !OAuthProviderFlows[id].LogoutFromConfig {
			t.Fatalf("%s must log out from config", id)
		}
	}
	for _, id := range []string{"qwen-oauth", "minimax-oauth"} {
		if OAuthProviderFlows[id].LogoutFromConfig {
			t.Fatalf("%s must not log out from config", id)
		}
	}
}

// ---- config helpers ----

func readModelCfg(t *testing.T, h string) map[string]any {
	t.Helper()
	doc, err := config.ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := doc["model"].(map[string]any)
	return model
}

func seedCustomProviderConfig(t *testing.T, h, apiMode string) {
	t.Helper()
	writeConfigYAML(t, h, "model:\n  provider: custom\n  base_url: https://api.minimax.io/anthropic\n  api_key: mxp-stale-key\n  api_mode: "+apiMode+"\n  default: claude-sonnet-4-6\n")
}

func TestClearModelEndpointCredentialsRemovesKeyAliasAndMode(t *testing.T) {
	modelCfg := map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.6", "api_key": "sk-stale", "api": "sk-legacy-stale", "api_mode": "anthropic_messages"}
	returned := clearModelEndpointCredentials(modelCfg, true, true, false)
	if len(returned) != 2 || returned["provider"] != "openrouter" {
		t.Fatalf("unexpected %v", returned)
	}
	for _, k := range []string{"api_key", "api", "api_mode"} {
		if _, has := modelCfg[k]; has {
			t.Fatalf("%s survived", k)
		}
	}
}

func TestClearModelEndpointCredentialsTreatsKeyEnvLikeTheInlineKey(t *testing.T) {
	modelCfg := map[string]any{"provider": "custom_x", "key_env": "CUSTOM_X_API_KEY", "api_key_env": "LEGACY_PTR", "api_mode": "openai"}
	clearModelEndpointCredentials(modelCfg, false, true, false)
	if modelCfg["key_env"] != "CUSTOM_X_API_KEY" {
		t.Fatal("key_env cleared under clear_api_key=false")
	}
	if _, has := modelCfg["api_mode"]; has {
		t.Fatal("api_mode survived")
	}
	clearModelEndpointCredentials(modelCfg, true, true, false)
	if _, has := modelCfg["key_env"]; has {
		t.Fatal("key_env survived")
	}
	if _, has := modelCfg["api_key_env"]; has {
		t.Fatal("api_key_env survived")
	}
}

func TestSwitchingToOpenRouterClearsAPIKeyAndAPIMode(t *testing.T) {
	h, env := codexTestEnv(t)
	seedCustomProviderConfig(t, h, "anthropic_messages")
	if _, err := updateConfigForProvider(env, "openrouter", "https://openrouter.ai/api/v1", "anthropic/claude-sonnet-4.6", false); err != nil {
		t.Fatal(err)
	}
	modelCfg := readModelCfg(t, h)
	if modelCfg["provider"] != "openrouter" || modelCfg["base_url"] != "https://openrouter.ai/api/v1" {
		t.Fatalf("model cfg %v", modelCfg)
	}
	if _, has := modelCfg["api_key"]; has {
		t.Fatal("stale custom api_key would leak into OpenRouter requests")
	}
	if _, has := modelCfg["api_mode"]; has {
		t.Fatal("stale api_mode would mis-route OpenRouter requests")
	}
	if modelCfg["default"] != "claude-sonnet-4-6" {
		t.Fatalf("a direct style default was replaced: %v", modelCfg["default"])
	}
	active, _ := env.Store.ActiveProvider()
	if active != "openrouter" {
		t.Fatalf("active provider %q", active)
	}
}

func TestSwitchingClearsCodexResponsesAPIMode(t *testing.T) {
	h, env := codexTestEnv(t)
	seedCustomProviderConfig(t, h, "codex_responses")
	if _, err := updateConfigForProvider(env, "openrouter", "https://openrouter.ai/api/v1", "", false); err != nil {
		t.Fatal(err)
	}
	if _, has := readModelCfg(t, h)["api_mode"]; has {
		t.Fatal("api_mode survived")
	}
}

func TestUpdateConfigForProviderDefaultModelAndClearDefault(t *testing.T) {
	h, env := codexTestEnv(t)
	writeConfigYAML(t, h, "model: anthropic/claude-opus-4.6\n")
	if _, err := updateConfigForProvider(env, "openai-codex", "", "gpt-5.5", false); err != nil {
		t.Fatal(err)
	}
	modelCfg := readModelCfg(t, h)
	if modelCfg["default"] != "gpt-5.5" || modelCfg["provider"] != "openai-codex" {
		t.Fatalf("model cfg %v", modelCfg)
	}
	if _, has := modelCfg["base_url"]; has {
		t.Fatal("a stale base_url survived a provider switch without one")
	}
	if _, err := updateConfigForProvider(env, "nous", "", "", true); err != nil {
		t.Fatal(err)
	}
	if _, has := readModelCfg(t, h)["default"]; has {
		t.Fatal("clear_default left the previous model")
	}
}

func TestRequireReadableConfigRefusesABrokenFile(t *testing.T) {
	h, _ := codexTestEnv(t)
	writeConfigYAML(t, h, "model: [unclosed\n")
	_, err := requireReadableConfigBeforeWrite(h)
	var refused *ConfigWriteRefusedError
	if err == nil || !strings.Contains(err.Error(), "formatting error") {
		t.Fatalf("expected a refusal, got %v", err)
	}
	if !errors.As(err, &refused) {
		t.Fatalf("wrong error type %T", err)
	}
	if _, err := os.Stat(filepath.Join(h, "config.yaml")); err != nil {
		t.Fatal("config removed")
	}
	writeConfigYAML(t, h, "- a list\n")
	if _, err := requireReadableConfigBeforeWrite(h); err == nil {
		t.Fatal("a non mapping root was accepted")
	}
	_ = os.Remove(filepath.Join(h, "config.yaml"))
	config.ResetCache()
	loaded, err := requireReadableConfigBeforeWrite(h)
	if err != nil || len(loaded) != 0 {
		t.Fatalf("missing file must read as empty: %v %v", loaded, err)
	}
}

// ---- logout ----

func TestLogoutResetsCodexConfigWhenAuthStateAlreadyCleared(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}, "credential_pool": map[string]any{}})
	writeConfigYAML(t, h, "model:\n  default: gpt-5.3-codex\n  provider: openai-codex\n  base_url: https://chatgpt.com/backend-api/codex\n")
	var out bytes.Buffer
	if err := LogoutCommand(env, "openai-codex", &out); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	text := string(raw)
	if !strings.Contains(text, "provider: auto") || !strings.Contains(text, "base_url: https://openrouter.ai/api/v1") {
		t.Fatalf("config not reset:\n%s", text)
	}
	if !strings.Contains(out.String(), "Logged out of") || !strings.Contains(out.String(), "Run `icebath model` or configure an API key to use Icebath.") {
		t.Fatalf("output %q", out.String())
	}
}

func TestLogoutFallsBackToConfigSelectedOAuthProvider(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"openai-codex": map[string]any{"tokens": map[string]any{"access_token": "a", "refresh_token": "r"}}}})
	writeConfigYAML(t, h, "model:\n  provider: openai-codex\n")
	var out bytes.Buffer
	if err := LogoutCommand(env, "", &out); err != nil {
		t.Fatal(err)
	}
	providers, _ := readAuthStore(t, h)["providers"].(map[string]any)
	if _, has := providers["openai-codex"]; has {
		t.Fatal("codex state survived logout")
	}
	if readModelCfg(t, h)["provider"] != "auto" {
		t.Fatal("config provider not reset")
	}
	out.Reset()
	if err := LogoutCommand(env, "", &out); err != nil || !strings.Contains(out.String(), "No provider is currently logged in.") {
		t.Fatalf("second logout %v %q", err, out.String())
	}
}

func TestLogoutUnknownProviderAndUntouchedConfig(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"xai-oauth": map[string]any{"tokens": map[string]any{"access_token": "a"}}}})
	writeConfigYAML(t, h, "model:\n  provider: openrouter\n")
	var out bytes.Buffer
	err := LogoutCommand(env, "no-such-provider", &out)
	if err == nil || err.Error() != "Unknown provider: no-such-provider" {
		t.Fatalf("unexpected %v", err)
	}
	if err := LogoutCommand(env, "xai-oauth", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Model provider configuration was unchanged.") || readModelCfg(t, h)["provider"] != "openrouter" {
		t.Fatalf("output %q cfg %v", out.String(), readModelCfg(t, h))
	}
	out.Reset()
	if err := LogoutCommand(env, "xai-oauth", &out); err != nil || !strings.Contains(out.String(), "No auth state found for") {
		t.Fatalf("repeat logout %v %q", err, out.String())
	}
}

func TestLoginCommandPrintsRemovalNotice(t *testing.T) {
	var out bytes.Buffer
	LoginCommand(&out)
	if !strings.HasPrefix(out.String(), "The 'icebath login' command has been removed.") {
		t.Fatalf("output %q", out.String())
	}
}

// ---- api key and minimax status ----

func TestGetAPIKeyProviderStatus(t *testing.T) {
	h, env := codexTestEnv(t)
	t.Setenv("ZAI_API_KEY", "")
	if status := GetAPIKeyProviderStatus(env, "openai-codex"); status["configured"] != false || len(status) != 1 {
		t.Fatalf("oauth provider %v", status)
	}
	status := GetAPIKeyProviderStatus(env, "zai")
	if status["configured"] != false || status["logged_in"] != false || status["provider"] != "zai" {
		t.Fatalf("unset key %v", status)
	}
	writeEnvFile(t, h, "ZAI_API_KEY=zai-test-key-1234\n")
	env = testEnv(t, h)
	status = GetAPIKeyProviderStatus(env, "zai")
	if status["configured"] != true || status["logged_in"] != true || anyString(status["base_url"]) == "" {
		t.Fatalf("set key %v", status)
	}
}

func TestGetMinimaxOAuthAuthStatus(t *testing.T) {
	h, env := codexTestEnv(t)
	if status := GetMinimaxOAuthAuthStatus(env); status["logged_in"] != false {
		t.Fatalf("no state %v", status)
	}
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"minimax-oauth": map[string]any{"access_token": "tok", "expires_at": isoIn(3600), "region": "cn"}}})
	status := GetMinimaxOAuthAuthStatus(env)
	if status["logged_in"] != true || status["region"] != "cn" {
		t.Fatalf("valid %v", status)
	}
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"minimax-oauth": map[string]any{"access_token": "tok", "expires_at": isoIn(-3600)}}})
	status = GetMinimaxOAuthAuthStatus(env)
	if status["logged_in"] != false || status["region"] != "global" {
		t.Fatalf("expired %v", status)
	}
}

func TestGetAzureFoundryAuthStatus(t *testing.T) {
	h, env := codexTestEnv(t)
	t.Setenv("AZURE_FOUNDRY_API_KEY", "")
	if status := getAzureFoundryAuthStatus(env); status["auth_mode"] != "api_key" || status["logged_in"] != false {
		t.Fatalf("no key %v", status)
	}
	writeConfigYAML(t, h, "model:\n  provider: azure-foundry\n  auth_mode: entra_id\n  base_url: https://foo.services.ai.azure.com\n  entra:\n    scope: https://cognitiveservices.azure.com/.default\n")
	env = testEnv(t, h)
	status := getAzureFoundryAuthStatus(env)
	if status["auth_mode"] != "entra_id" || status["scope"] != "https://cognitiveservices.azure.com/.default" || status["credential_probe"] != "not_run" || status["logged_in"] != true {
		t.Fatalf("entra %v", status)
	}
}

func TestFormatAuthErrorCopy(t *testing.T) {
	codexTestEnv(t)
	if got := formatAuthError(newAuthErr("zai", "No active paid subscription found.", "subscription_required", false)); got != genericEntitlementMessages["subscription_required"] {
		t.Fatalf("entitlement %q", got)
	}
	if got := formatAuthError(newAuthErr("zai", "busy", "temporarily_unavailable", false)); got != "busy Please retry in a few seconds." {
		t.Fatalf("unavailable %q", got)
	}
	if got := formatAuthError(newAuthErr("zai", "plain", "other", false)); got != "plain" {
		t.Fatalf("plain %q", got)
	}
	if got := formatAuthError(newAuthErr("zai", "dead", "invalid_grant", true)); got != "dead Run `icebath model` to re-authenticate." {
		t.Fatalf("relogin %q", got)
	}
}

// External process test profile launching acme over stdio with env overrides for its command and argv
func acmeACPProfile() Profile {
	return Profile{Name: "acme-acp", DisplayName: "acme-acp", AuthType: AuthTypeExternalProcess, BaseURL: "acp://acme",
		BaseURLEnv: "ACME_ACP_BASE_URL", ProcessCommand: "acme", ProcessArgs: []string{"--acp", "--stdio"},
		ProcessCommandEnvVars: []string{"ICEBATH_ACME_ACP_COMMAND"}, ProcessArgsEnvVar: "ICEBATH_ACME_ACP_ARGS"}
}
