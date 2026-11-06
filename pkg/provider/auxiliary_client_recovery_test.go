package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Recovery pool double recording every rotation request
type auxStubRecoveryPool struct{ rotations []RotationRequest }

func (p *auxStubRecoveryPool) HasCredentials() bool { return true }
func (p *auxStubRecoveryPool) TryRefreshCurrent() (PooledCredential, bool) {
	return PooledCredential{}, false
}
func (p *auxStubRecoveryPool) MarkExhaustedAndRotate(req RotationRequest) (PooledCredential, bool) {
	p.rotations = append(p.rotations, req)
	return PooledCredential{ID: "next"}, true
}

// Base URL and key only client for host and origin inference
type auxBareClient struct{ base, key string }

func (c auxBareClient) BaseURL() string { return c.base }
func (c auxBareClient) APIKey() string  { return c.key }
func (c auxBareClient) Close() error    { return nil }
func (c auxBareClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, nil
}

// Temp home with isolated user home and Claude config dirs so no ambient login seeds a pool
func auxRecoveryHome(t *testing.T, configYAML string) context.Context {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	prev := auxReadClaudeCodeCredentialsFn
	auxReadClaudeCodeCredentialsFn = func(context.Context) map[string]any { return nil }
	t.Cleanup(func() { auxReadClaudeCodeCredentialsFn = prev })
	return auxMainTestHome(t, configYAML)
}

func TestAuxAuthRefreshProviderForRoute(t *testing.T) {
	ctx := auxRecoveryHome(t, "")
	for effective, want := range map[string]string{"": "xai-oauth", "auto": "xai-oauth", "xai-oauth": "xai-oauth", "xai": "auto"} {
		if got := auxAuthRefreshProviderForRoute(ctx, "auto", "https://api.x.ai/v1", effective); got != want {
			t.Fatalf("effective %q: got %q want %q", effective, got, want)
		}
	}
	if got := auxAuthRefreshProviderForRoute(ctx, "auto", "https://unknown.example.com/v1", ""); got != "auto" {
		t.Fatalf("unknown host: %q", got)
	}
}

func TestAuxXaiOAuth403AuthError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{auxExc("Error code: 403 - {'code': 'The caller does not have permission to execute the specified operation', 'error': 'The OAuth2 access token could not be validated. [WKE=unauthenticated:bad-credentials]'}", 403), true},
		{auxExc("Error code: 403 - unauthenticated:bad-credentials", 0), true},
		{auxExc("Error code: 403 - rate limit exceeded", 403), false},
		{auxExc("unauthenticated request", 0), false},
	}
	for _, c := range cases {
		if got := auxIsAuthError(c.err); got != c.want {
			t.Fatalf("%v: got %v", c.err, got)
		}
	}
}

func TestAuxRecoverablePoolProviderXaiOAuth(t *testing.T) {
	ctx := auxRecoveryHome(t, "")
	if got, _ := auxRecoverablePoolProvider(ctx, "xai-oauth", nil, nil); got != "xai-oauth" {
		t.Fatalf("explicit: %q", got)
	}
	if got, _ := auxRecoverablePoolProvider(ctx, "auto", auxBareClient{base: "https://api.x.ai/v1/"}, nil); got != "xai-oauth" {
		t.Fatalf("host match: %q", got)
	}
	if got, _ := auxRecoverablePoolProvider(ctx, "auto", auxBareClient{base: "https://unknown.example.com/v1/"}, nil); got != "" {
		t.Fatalf("unknown host: %q", got)
	}
}

func TestAuxRecoverablePoolProviderSessionOrigin(t *testing.T) {
	ctx := auxRecoveryHome(t, "")
	session := map[string]any{"provider": "openai-api", "model": "gpt-5.4", "base_url": "https://proxy.example:8443/v1", "api_key": "sk-session"}
	for _, base := range []string{"https://api.openai.com/v1/", "https://proxy.example:9443/v1/", "http://proxy.example:8443/v1/"} {
		got, err := auxRecoverablePoolProvider(ctx, "openai-api", auxBareClient{base: base, key: "sk-session"}, session)
		if err != nil || got != "" {
			t.Fatalf("%s: got %q err %v", base, got, err)
		}
	}
	if got, _ := auxRecoverablePoolProvider(ctx, "openai-api", auxBareClient{base: "https://proxy.example:8443/v1/", key: "sk-session"}, session); got != "openai-api" {
		t.Fatalf("same origin: %q", got)
	}
	if got, _ := auxRecoverablePoolProvider(ctx, "openai-api", auxBareClient{base: "https://api.openai.com/v1/", key: "sk-aux-pool-1"}, session); got != "openai-api" {
		t.Fatalf("independent pool: %q", got)
	}
}

func TestAuxRefreshProviderCredentialsFailures(t *testing.T) {
	ctx := auxRecoveryHome(t, "")
	if auxRefreshProviderCredentials(ctx, "unknown-provider-xyz", "") {
		t.Fatal("unknown provider refreshed")
	}
	prev := auxVertexConfigFn
	auxVertexConfigFn = func(context.Context) (string, string) { return "", "" }
	t.Cleanup(func() { auxVertexConfigFn = prev })
	if auxRefreshProviderCredentials(ctx, "vertex", "") {
		t.Fatal("unminted vertex refreshed")
	}
}

func TestAuxUpstreamCapacity429IsNotACredentialToBench(t *testing.T) {
	ctx := auxRecoveryHome(t, "")
	cases := []struct {
		body    string
		benches bool
	}{
		{"Error code: 429 - {'status': 429, 'message': \"The requested model is temporarily at capacity upstream. This is not your API key's rate limit — please retry shortly.\"}", false},
		{"Error code: 429 - {'error': {'message': 'Rate limit exceeded for this API key', 'code': 429}}", true},
	}
	for _, c := range cases {
		exc := auxExc(c.body, 429)
		if !auxIsRateLimitError(exc) || auxIsOverloadedError(exc) != !c.benches {
			t.Fatalf("predicates for %q", c.body)
		}
		wantReason := ReasonOverloaded
		if c.benches {
			wantReason = ReasonRateLimit
		}
		if got := ClassifyError(ClassifyInput{Err: exc, Status: 429, Body: c.body}); got.Reason != wantReason {
			t.Fatalf("classifier reason %v want %v", got.Reason, wantReason)
		}
		pool := &auxStubRecoveryPool{}
		prev := auxRecoveryPoolFn
		auxRecoveryPoolFn = func(context.Context, string, string) (auxRecoveryPool, error) { return pool, nil }
		recovered := auxRecoverProviderPool(ctx, "openrouter", exc, "sk-failed")
		auxRecoveryPoolFn = prev
		if recovered != c.benches || len(pool.rotations) != map[bool]int{true: 1, false: 0}[c.benches] {
			t.Fatalf("recovered %v rotations %d", recovered, len(pool.rotations))
		}
		if c.benches && (pool.rotations[0].StatusCode != 429 || pool.rotations[0].APIKeyHint != "sk-failed") {
			t.Fatalf("rotation %+v", pool.rotations[0])
		}
	}
}

// Writes a single or double OAuth pool row set for a provider, the first row optionally cooled down for a model
func auxSeedPool(t *testing.T, ctx context.Context, provider string, tokens []string, cooldownModel string) {
	t.Helper()
	var rows []map[string]any
	for i, tok := range tokens {
		row := map[string]any{"id": "entry-" + string(rune('1'+i)), "label": "pooled", "auth_type": "oauth", "priority": i + 1,
			"source": "manual", "access_token": tok, "refresh_token": "rt-" + tok, "expires_at": float64(time.Now().Unix() + 3600)}
		if cooldownModel != "" {
			row["model_cooldowns"] = map[string]any{cooldownModel: float64(time.Now().Unix() + 600)}
		}
		rows = append(rows, row)
	}
	if _, err := NewStore(os.Getenv("ICEBATH_HOME")).WriteCredentialPool(provider, rows, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAuxPoolCacheHintFollowsRotation(t *testing.T) {
	ctx := auxRecoveryHome(t, "")
	auxSeedPool(t, ctx, "anthropic", []string{"tok-old"}, "claude-sonnet-4-5")
	before := auxPoolCacheHint(ctx, "anthropic", nil)
	auxSeedPool(t, ctx, "anthropic", []string{"tok-new"}, "claude-sonnet-4-5")
	after := auxPoolCacheHint(ctx, "anthropic", nil)
	if !strings.HasPrefix(before, "anthropic:") || !strings.HasPrefix(after, "anthropic:") || before == after || strings.Contains(before+after, "tok-") {
		t.Fatalf("cooldown hints %q %q", before, after)
	}
}

func TestAuxSiblingEntryRotationDoesNotChurnSelectedEntryKey(t *testing.T) {
	ctx := auxRecoveryHome(t, "")
	auxSeedPool(t, ctx, "anthropic", []string{"tok-a1", "tok-b1"}, "")
	first := auxPoolCacheHint(ctx, "anthropic", nil)
	if !strings.HasPrefix(first, "anthropic:entry-1:") {
		t.Fatalf("selected entry hint %q", first)
	}
	auxSeedPool(t, ctx, "anthropic", []string{"tok-a1", "tok-b2"}, "")
	if got := auxPoolCacheHint(ctx, "anthropic", nil); got != first {
		t.Fatalf("sibling rotation churned %q -> %q", first, got)
	}
	auxSeedPool(t, ctx, "anthropic", []string{"tok-a2", "tok-b2"}, "")
	if got := auxPoolCacheHint(ctx, "anthropic", nil); !strings.HasPrefix(got, "anthropic:entry-1:") || got == first || strings.Contains(got, "tok-") {
		t.Fatalf("selected rotation %q", got)
	}
}

func TestAuxAuthFailureOnEntryNeverRotatesBuiltinPool(t *testing.T) {
	const endpoint, entryKey, builtinKey = "http://127.0.0.1:18080/v1", "entry-key-A-0123456789", "builtin-key-B-0123456789"
	ctx := auxRecoveryHome(t, "model:\n  default: m\n  provider: custom:deepseek\nproviders:\n  deepseek:\n    name: deepseek\n    api: "+endpoint+"\n    transport: chat_completions\n    default_model: m\n    key_env: ENTRY_KEY_VAR\n")
	h := os.Getenv("ICEBATH_HOME")
	if err := os.WriteFile(filepath.Join(h, "auth.json"), []byte(`{"version": 1, "providers": {}, "credential_pool": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", builtinKey)
	t.Setenv("ENTRY_KEY_VAR", entryKey)
	LoadPool(modelsPoolEnv(ctx), "deepseek")
	poolProvider, err := auxRecoverablePoolProvider(ctx, "custom:deepseek", auxBareClient{base: endpoint, key: entryKey}, nil)
	if err != nil || poolProvider == "" {
		t.Fatalf("pool provider %q err %v", poolProvider, err)
	}
	authErr := &AuxSDKError{Class: "AuthError", Message: "Error code: 401", StatusCode: 401}
	if auxRecoverProviderPool(ctx, poolProvider, authErr, entryKey) || auxRecoverProviderPool(ctx, poolProvider, authErr, "") {
		t.Fatal("entry auth failure recovered through a pool")
	}
	var builtin []PooledCredential
	for _, e := range LoadPool(modelsPoolEnv(ctx), "deepseek").Entries() {
		if e.Source == "env:DEEPSEEK_API_KEY" {
			builtin = append(builtin, e)
		}
	}
	if len(builtin) == 0 {
		t.Fatal("built-in env row missing")
	}
	for _, e := range builtin {
		if e.LastStatus == "exhausted" {
			t.Fatalf("built-in row benched %+v", e)
		}
	}
}

func TestAuxFallbackChainEntry(t *testing.T) {
	ctx := auxRecoveryHome(t, "auxiliary:\n  compression:\n    fallback_chain:\n      - provider: openrouter\n        model: m1\n      - nope\n")
	if got := auxFallbackChainEntry(ctx, "compression", "fallback_chain[0](openrouter)"); got["provider"] != "openrouter" {
		t.Fatalf("entry %v", got)
	}
	for _, label := range []string{"fallback_chain[1](x)", "fallback_chain[9](x)", "main", ""} {
		if got := auxFallbackChainEntry(ctx, "compression", label); got != nil {
			t.Fatalf("%q resolved %v", label, got)
		}
	}
}

// Loop double whose closed flag the test flips
type auxTestLoop struct{ closed bool }

func (l *auxTestLoop) IsClosed() bool { return l.closed }

// Client double counting Close calls, optionally failing the close
type auxCloseCounter struct {
	auxBareClient
	closes int
	fail   bool
}

func (c *auxCloseCounter) Close() error {
	c.closes++
	if c.fail {
		panic(errors.New("close raised"))
	}
	return nil
}

func TestCleanupStaleAsyncClientsClosesOnlyDeadLoopEntries(t *testing.T) {
	auxIsolateClientCache(t)
	live, dead := &auxTestLoop{}, &auxTestLoop{closed: true}
	syncClient, liveClient, deadClient, failing := &auxCloseCounter{}, &auxCloseCounter{}, &auxCloseCounter{}, &auxCloseCounter{fail: true}
	auxClientCacheMu.Lock()
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "sync"}, auxCachedClient{Client: syncClient})
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "live"}, auxCachedClient{Client: liveClient, BoundLoop: live})
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "dead"}, auxCachedClient{Client: deadClient, BoundLoop: dead})
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "dead-failing"}, auxCachedClient{Client: failing, BoundLoop: dead})
	auxClientCacheMu.Unlock()
	CleanupStaleAsyncClients()
	auxClientCacheMu.Lock()
	keys := auxClientCacheKeysLocked()
	auxClientCacheMu.Unlock()
	if len(keys) != 2 || keys[0].Provider != "sync" || keys[1].Provider != "live" {
		t.Fatalf("remaining keys %+v", keys)
	}
	if deadClient.closes != 1 || failing.closes != 1 || syncClient.closes != 0 || liveClient.closes != 0 {
		t.Fatalf("closes dead %d failing %d sync %d live %d", deadClient.closes, failing.closes, syncClient.closes, liveClient.closes)
	}
}
