package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
)

const codexRestoreURL = "https://chatgpt.com/backend-api/codex"

// Codex primary on a fixture agent whose snapshot and pool mirror the reselect suite
func newReselectAgent(t *testing.T, entries []map[string]any) *AIAgent {
	t.Helper()
	a := newRestoreAgent(t, "credential_pool_strategies:\n  openai-codex: round_robin\n", nil, "custom", restoreBaseURL, nil)
	rows := make([]provider.PooledCredential, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, provider.PooledCredentialFromMap("openai-codex", e))
	}
	a.model, a.provider, a.apiMode, a.apiKey = "gpt-5.5", "openai-codex", "codex_responses", "original-key-entry-1"
	a.setBaseURL(codexRestoreURL)
	a.clientKwargs = map[string]any{"api_key": "original-key-entry-1", "base_url": codexRestoreURL}
	a.credentialPool = restorePool(a, "openai-codex", rows...)
	a.fallbackActivated, a.fallbackIndex, a.rateLimitedUntil = true, 1, 0
	a.usePromptCaching, a.useNativeCacheLayout = false, false
	a.primaryRuntime = map[string]any{
		"model": "gpt-5.5", "provider": "openai-codex", "base_url": codexRestoreURL, "api_mode": "codex_responses",
		"api_key": "original-key-entry-1", "client_kwargs": map[string]any{"api_key": "original-key-entry-1", "base_url": codexRestoreURL},
		"use_prompt_caching": false, "use_native_cache_layout": false, "compressor_model": "gpt-5.5",
		"compressor_base_url": codexRestoreURL, "compressor_api_key": "original-key-entry-1", "compressor_provider": "openai-codex",
		"compressor_context_length": 128000, "compressor_threshold_tokens": 0.8,
	}
	return a
}

func reselectEntry(label, token string, priority int) map[string]any {
	return map[string]any{"id": label, "label": label, "provider": "openai-codex", "auth_type": provider.AuthTypeOAuth,
		"source": "device_code", "priority": priority, "access_token": token, "refresh_token": "rt-" + label, "base_url": codexRestoreURL}
}

func TestRestorePrimaryPoolReselectRestoreUsesFreshestAvailableEntry(t *testing.T) {
	first := reselectEntry("entry-1", "key-1", 0)
	first["last_status"], first["last_status_at"] = "exhausted", float64(time.Now().Unix()+3600)
	a := newReselectAgent(t, []map[string]any{first, reselectEntry("entry-2", "key-2", 1), reselectEntry("entry-3", "key-3", 2)})
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if a.apiKey != "key-2" || a.clientKwargs["api_key"] != "key-2" {
		t.Fatalf("key %s kwargs %v", a.apiKey, a.clientKwargs["api_key"])
	}
}

func TestRestorePrimaryPoolReselectRestoreUpdatesBaseURLFromPoolEntry(t *testing.T) {
	entry := reselectEntry("entry-1", "key-1", 0)
	entry["base_url"] = "https://custom-endpoint.example.com/v1"
	a := newReselectAgent(t, []map[string]any{entry})
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if !strings.Contains(a.baseURL, "custom-endpoint.example.com") || !strings.Contains(pyStrOr(a.clientKwargs["base_url"], ""), "custom-endpoint.example.com") {
		t.Fatalf("base %s kwargs %v", a.baseURL, a.clientKwargs["base_url"])
	}
}

const revertBase = "https://api.anthropic.com"

func revertEntry(id, label string, priority int, authType, token string) provider.PooledCredential {
	raw := map[string]any{"id": id, "label": label, "auth_type": authType, "priority": priority, "access_token": token, "base_url": revertBase, "source": "manual"}
	if authType == "oauth" {
		raw["refresh_token"] = "rt-" + id
		raw["expires_at_ms"] = (time.Now().Unix() + 30*86400) * 1000
	}
	return provider.PooledCredentialFromMap("anthropic", raw)
}

// Long lived session on a real pool whose credential swaps only rebind the key
func newLiveRevertAgent(t *testing.T, pool *provider.Pool) *AIAgent {
	t.Helper()
	a := &AIAgent{provider: "anthropic", model: "claude-opus-5",
		primaryRuntime: map[string]any{"provider": "anthropic", "model": "claude-opus-5", "base_url": revertBase}}
	a.baseURL = revertBase
	a.credentialPool = pool
	first, _ := pool.Select("")
	a.credentialPoolEntryID, a.apiKey = first.ID, first.RuntimeAPIKey()
	saved := swapPoolCredential
	t.Cleanup(func() { swapPoolCredential = saved })
	swapPoolCredential = func(_ context.Context, a *AIAgent, e provider.PooledCredential) (bool, error) {
		a.apiKey, a.credentialPoolEntryID = e.RuntimeAPIKey(), e.ID
		return true, nil
	}
	return a
}

// Moves the pool clock past windows quota cooldowns from real
func expireCooldowns(t *testing.T, real func() time.Time, windows int) {
	t.Helper()
	t.Cleanup(provider.SetPoolClock(func() time.Time {
		return real().Add(time.Duration(windows) * (provider.ExhaustedTTL429 + 120*time.Second))
	}))
}

func recoverStatus(t *testing.T, a *AIAgent, status int, retried bool, message string) bool {
	t.Helper()
	recovered, _, err := a.recoverWithCredentialPool(context.Background(), &status, retried, "", map[string]any{"message": message}, false)
	if err != nil {
		t.Fatal(err)
	}
	return recovered
}

func TestLiveSessionRevertsToQuotaBenchedCredentialOnceCooldownLifts(t *testing.T) {
	env := agentPoolEnv(tempHomeContext(t))
	pool := provider.NewPool(env, "anthropic", []provider.PooledCredential{
		revertEntry("pref0000", "subscription-oauth", 0, "oauth", "sk-ant-oat01-PREF"),
		revertEntry("fall0000", "paid-api-key", 1, "api_key", "sk-ant-api03-FALL")})
	a := newLiveRevertAgent(t, pool)
	if a.apiKey != "sk-ant-oat01-PREF" {
		t.Fatalf("start key %s", a.apiKey)
	}
	recoverStatus(t, a, 429, false, "Error")
	if !recoverStatus(t, a, 429, true, "Error") || a.apiKey != "sk-ant-api03-FALL" {
		t.Fatalf("rotation key %s", a.apiKey)
	}
	if mustRestore(t, a) || a.apiKey != "sk-ant-api03-FALL" {
		t.Fatalf("reverted while benched, key %s", a.apiKey)
	}
	expireCooldowns(t, time.Now, 1)
	if mustRestore(t, a) {
		t.Fatal("credential revert ran the primary runtime restore")
	}
	if a.apiKey != "sk-ant-oat01-PREF" || a.credentialPoolEntryID != "pref0000" {
		t.Fatalf("key %s entry %s", a.apiKey, a.credentialPoolEntryID)
	}
	if a.fallbackActivated || a.model != "claude-opus-5" || a.credentialPoolRevertID != "" {
		t.Fatalf("fallback %v model %s revert %q", a.fallbackActivated, a.model, a.credentialPoolRevertID)
	}
	for _, e := range pool.Entries() {
		if e.ID == "pref0000" && e.LastStatus == provider.StatusExhausted {
			t.Fatal("preferred entry still benched")
		}
	}
	provider.SetPoolClock(time.Now)
	pool2 := provider.NewPool(env, "anthropic", []provider.PooledCredential{
		revertEntry("pref0000", "subscription-oauth", 0, "oauth", "sk-ant-oat01-PREF"),
		revertEntry("fall0000", "paid-api-key", 1, "api_key", "sk-ant-api03-FALL")})
	pool2.MarkExhaustedAndRotate(provider.RotationRequest{StatusCode: 429, CredentialID: "pref0000", FailureReason: "rate_limit", ErrorContext: map[string]any{"message": "Error"}})
	late := newLiveRevertAgent(t, pool2)
	if late.apiKey != "sk-ant-api03-FALL" {
		t.Fatalf("late start key %s", late.apiKey)
	}
	expireCooldowns(t, time.Now, 1)
	recoverStatus(t, late, 429, false, "Error")
	if !recoverStatus(t, late, 429, true, "Error") || late.apiKey != "sk-ant-oat01-PREF" {
		t.Fatalf("late rotation key %s", late.apiKey)
	}
	if late.credentialPoolRevertID != "" {
		t.Fatalf("upward rotation armed revert %q", late.credentialPoolRevertID)
	}
	expireCooldowns(t, time.Now, 2)
	if mustRestore(t, late) {
		t.Fatal("late restore ran")
	}
	if next, _ := pool2.Select(""); late.apiKey != "sk-ant-oat01-PREF" || next.ID != "pref0000" {
		t.Fatalf("late key %s select %s", late.apiKey, next.ID)
	}
}

func TestAuthBenchDoesNotArmARevert(t *testing.T) {
	pool := provider.NewPool(agentPoolEnv(tempHomeContext(t)), "anthropic", []provider.PooledCredential{
		revertEntry("pref0000", "primary-key", 0, "api_key", "sk-ant-api03-PREF"),
		revertEntry("fall0000", "backup-key", 1, "api_key", "sk-ant-api03-FALL")})
	a := newLiveRevertAgent(t, pool)
	if !recoverStatus(t, a, 401, false, "invalid") || a.apiKey != "sk-ant-api03-FALL" {
		t.Fatalf("auth rotation key %s", a.apiKey)
	}
	expireCooldowns(t, time.Now, 1)
	if mustRestore(t, a) || a.apiKey != "sk-ant-api03-FALL" || a.credentialPoolRevertID != "" {
		t.Fatalf("key %s revert %q", a.apiKey, a.credentialPoolRevertID)
	}
}

// Temp home context for a bare agent pool
func tempHomeContext(t *testing.T) context.Context {
	t.Helper()
	promptTestEnv(t)
	return context.Background()
}

func writeExhaustedCodexPool(t *testing.T, h string, resetIn float64) {
	t.Helper()
	now := float64(time.Now().UnixNano()) / 1e9
	entry := map[string]any{"id": "codex-1", "label": "codex-login", "auth_type": "oauth", "priority": 0, "source": "device_code",
		"access_token": "token", "refresh_token": "refresh", "base_url": codexRestoreURL, "last_status": "exhausted",
		"last_status_at": now, "last_error_code": 429, "last_error_reason": "usage_limit_reached",
		"last_error_message": "The usage limit has been reached", "last_error_reset_at": now + resetIn}
	raw, err := json.Marshal(map[string]any{"version": 1, "credential_pool": map[string]any{"openai-codex": []any{entry}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Session on its openrouter fallback whose snapshot names a Codex primary
func codexFallbackAgent(t *testing.T) *AIAgent {
	t.Helper()
	a := newRestoreAgent(t, "", openrouterFallback, "custom", restoreBaseURL, nil)
	writeExhaustedCodexPool(t, a.env.Home, 3*86400)
	a.primaryRuntime["provider"], a.primaryRuntime["base_url"] = "openai-codex", codexRestoreURL
	recordRestoreSwaps()
	pinToFallback(t, a)
	return a
}

func pinToFallback(t *testing.T, a *AIAgent) {
	t.Helper()
	a.fallbackIndex = 0
	mustActivate(t, a, "")
	a.rateLimitedUntil = pythread.Monotonic() + 3*86400
}

func restoreWithQuota(t *testing.T, a *AIAgent, quotaRestored bool) bool {
	t.Helper()
	t.Cleanup(provider.SetCodexQuotaProbe(func(provider.PoolEnv, string, string) (bool, bool) { return quotaRestored, true }))
	return mustRestore(t, a)
}

func TestReturnsToPrimaryWhenQuotaReopensBeforeDeclaredReset(t *testing.T) {
	a := codexFallbackAgent(t)
	if !restoreWithQuota(t, a, true) || a.fallbackActivated {
		t.Fatalf("still on fallback %v", a.fallbackActivated)
	}
}

func TestStaysOnFallbackWhileQuotaIsStillSpent(t *testing.T) {
	a := codexFallbackAgent(t)
	if restoreWithQuota(t, a, false) || !a.fallbackActivated {
		t.Fatalf("left the fallback %v", a.fallbackActivated)
	}
}

func TestAWrongRestoredVerdictDoesNotFlipBackEveryTurn(t *testing.T) {
	a := codexFallbackAgent(t)
	if !restoreWithQuota(t, a, true) {
		t.Fatal("first restore declined")
	}
	writeExhaustedCodexPool(t, a.env.Home, 3*86400)
	pinToFallback(t, a)
	if restoreWithQuota(t, a, true) || !a.fallbackActivated {
		t.Fatalf("flipped back %v", a.fallbackActivated)
	}
}

// Restores after a zai fallback with the primary snapshot naming primaryProvider and primaryModel
func restoreAfterZaiFallback(t *testing.T, primaryProvider, primaryModel string, rejected map[[2]string]bool) (*AIAgent, bool, []string) {
	t.Helper()
	a := newRestoreAgent(t, "", []map[string]any{{"provider": "zai", "model": "glm-5.2", "api_key": "fallback-key-1234"}}, "custom", restoreBaseURL, nil)
	a.primaryRuntime["model"], a.primaryRuntime["provider"] = primaryModel, primaryProvider
	a.entitlementRejectedModels = rejected
	mustActivate(t, a, "")
	emitted := captureStatuses(a)
	restored := mustRestore(t, a)
	return a, restored, *emitted
}

func noticeContains(items []string, sub string) bool {
	for _, s := range items {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestRestoreSkipsAPrimaryAlreadyKnownToBeNonChat(t *testing.T) {
	a, restored, emitted := restoreAfterZaiFallback(t, "alibaba-token-plan", "wan2.7-image-pro", nil)
	if restored || a.provider != "zai" || a.model != "glm-5.2" || !a.fallbackActivated {
		t.Fatalf("restored %v %s %s %v", restored, a.provider, a.model, a.fallbackActivated)
	}
	if noticeContains(emitted, "Primary model restored") {
		t.Fatalf("announced %q", emitted)
	}
	other, otherRestored, _ := restoreAfterZaiFallback(t, "alibaba-token-plan", "acme/text-to-image", nil)
	if otherRestored || other.model != "glm-5.2" {
		t.Fatalf("restored %v %s", otherRestored, other.model)
	}
}

func TestRestoreStillReturnsAChatPrimary(t *testing.T) {
	_, restored, emitted := restoreAfterZaiFallback(t, "alibaba-token-plan", "qwen3.7-plus", nil)
	if !restored || !noticeContains(emitted, "Primary model restored") {
		t.Fatalf("restored %v notices %q", restored, emitted)
	}
}

func TestRestorePrimaryRuntimeIsGatedOnRejectedPrimarySlug(t *testing.T) {
	a, restored, emitted := restoreAfterZaiFallback(t, "openai-codex", "gpt-5.6-sol", map[[2]string]bool{{"openai-codex", "gpt-5.6-sol"}: true})
	if restored || a.provider != "zai" || a.model != "glm-5.2" || !a.fallbackActivated || noticeContains(emitted, "Primary model restored") {
		t.Fatalf("restored %v %s %s %v %q", restored, a.provider, a.model, a.fallbackActivated, emitted)
	}
	_, restored, emitted = restoreAfterZaiFallback(t, "openai-codex", "gpt-5.6-sol", map[[2]string]bool{{"openai-codex", "some-other-slug"}: true})
	if !restored || !noticeContains(emitted, "Primary model restored") {
		t.Fatalf("restored %v notices %q", restored, emitted)
	}
}

// Scripted reset a pool reports, counting the probes
type scriptedReset struct {
	nextAt *time.Time
	raise  bool
	calls  int
}

// Answers each pool's reset probe from its script, a pool without one answering for real
func scriptResets(t *testing.T, scripts map[*provider.Pool]*scriptedReset) {
	t.Helper()
	saved := restoreNextAvailableAt
	t.Cleanup(func() { restoreNextAvailableAt = saved })
	restoreNextAvailableAt = func(p *provider.Pool, model string) (time.Time, bool) {
		s, ok := scripts[p]
		if !ok {
			return saved(p, model)
		}
		s.calls++
		if s.raise {
			panic(errors.New("boom"))
		}
		if s.nextAt == nil {
			return time.Time{}, false
		}
		return *s.nextAt, true
	}
}

func resetIn(d time.Duration) *scriptedReset {
	at := time.Now().Add(d)
	return &scriptedReset{nextAt: &at}
}

func resetGateAgent(t *testing.T) *AIAgent {
	t.Helper()
	a := newRestoreAgent(t, "", openrouterFallback, "custom", restoreBaseURL, nil)
	mustActivate(t, a, "")
	return a
}

func TestResetAwareRestoreGateStaysOnFallbackUntilReset(t *testing.T) {
	a := resetGateAgent(t)
	a.rateLimitedUntil = 0
	a.credentialPool = restorePool(a, "custom")
	scriptResets(t, map[*provider.Pool]*scriptedReset{a.credentialPool: resetIn(time.Hour)})
	if mustRestore(t, a) || !a.fallbackActivated || a.provider != "openrouter" || a.model != "anthropic/claude-sonnet-4" {
		t.Fatalf("left fallback %v %s %s", a.fallbackActivated, a.provider, a.model)
	}
}

func TestResetAwareRestoreGateRestoresOnceResetElapsed(t *testing.T) {
	a := newRestoreAgent(t, "", openrouterFallback, "custom", restoreBaseURL, nil)
	originalModel := a.model
	mustActivate(t, a, "")
	a.rateLimitedUntil = 0
	a.credentialPool = restorePool(a, "custom")
	scriptResets(t, map[*provider.Pool]*scriptedReset{a.credentialPool: {}})
	if !mustRestore(t, a) || a.fallbackActivated || a.model != originalModel || a.provider != "custom" {
		t.Fatalf("restore %v %s %s", a.fallbackActivated, a.model, a.provider)
	}
}

func TestResetAwareRestoreGatePastResetTimeDoesNotBlock(t *testing.T) {
	a := resetGateAgent(t)
	a.rateLimitedUntil = 0
	a.credentialPool = restorePool(a, "custom")
	scriptResets(t, map[*provider.Pool]*scriptedReset{a.credentialPool: resetIn(-5 * time.Second)})
	if !mustRestore(t, a) || a.fallbackActivated {
		t.Fatal("past reset blocked the restore")
	}
}

func TestResetAwareRestoreGateFailsOpenOnPoolError(t *testing.T) {
	a := resetGateAgent(t)
	a.rateLimitedUntil = 0
	a.credentialPool = restorePool(a, "custom")
	scriptResets(t, map[*provider.Pool]*scriptedReset{a.credentialPool: {raise: true}})
	if !mustRestore(t, a) || a.fallbackActivated {
		t.Fatal("gate error blocked the restore")
	}
}

func TestResetAwareRestoreGateCrossProviderFallbackLoadsPrimaryPool(t *testing.T) {
	a := resetGateAgent(t)
	a.rateLimitedUntil = 0
	a.credentialPool = restorePool(a, "openrouter")
	primaryPool := restorePool(a, "custom")
	primary := resetIn(time.Hour)
	scriptResets(t, map[*provider.Pool]*scriptedReset{a.credentialPool: {}, primaryPool: primary})
	keys := recordRestorePoolLoads(primaryPool)
	if mustRestore(t, a) {
		t.Fatal("restored before the primary reset")
	}
	if !reflect.DeepEqual(*keys, []string{"custom"}) || primary.calls != 1 || !a.fallbackActivated {
		t.Fatalf("loads %v probes %d fallback %v", *keys, primary.calls, a.fallbackActivated)
	}
}

func TestResetAwareRestoreGateTransientCooldownStillRespected(t *testing.T) {
	a := resetGateAgent(t)
	a.rateLimitedUntil = pythread.Monotonic() + 60
	a.credentialPool = restorePool(a, "custom")
	script := &scriptedReset{}
	scriptResets(t, map[*provider.Pool]*scriptedReset{a.credentialPool: script})
	if mustRestore(t, a) || script.calls != 0 {
		t.Fatalf("probes %d", script.calls)
	}
}

func TestBackoffCounterResetsOnSuccessfulPrimaryRestore(t *testing.T) {
	a := newRestoreAgent(t, "", []map[string]any{{"provider": "openai", "model": "gpt-4o", "api_key": "fallback-key-1234"}}, "", "https://openrouter.ai/api/v1", nil)
	snapshot := [3]string{a.provider, a.model, a.baseURL}
	backOnPrimary := func() {
		a.provider, a.model = snapshot[0], snapshot[1]
		a.setBaseURL(snapshot[2])
		a.fallbackActivated, a.fallbackIndex = false, 0
	}
	a.rateLimitedUntil = 0
	a.tryActivateFallback(context.Background(), provider.ReasonRateLimit)
	backOnPrimary()
	a.tryActivateFallback(context.Background(), provider.ReasonRateLimit)
	if a.rateLimitBackoffCount != 2 {
		t.Fatalf("backoff %d", a.rateLimitBackoffCount)
	}
	a.fallbackActivated, a.rateLimitedUntil = true, 0
	if !mustRestore(t, a) || a.rateLimitBackoffCount != 0 {
		t.Fatalf("backoff %d after restore", a.rateLimitBackoffCount)
	}
	before := pythread.Monotonic()
	a.tryActivateFallback(context.Background(), provider.ReasonRateLimit)
	after := pythread.Monotonic()
	if a.rateLimitedUntil < before+60 || a.rateLimitedUntil > after+60 {
		t.Fatalf("cooldown %v outside %v..%v", a.rateLimitedUntil, before+60, after+60)
	}
}

func TestFallbackReasoningOverrideFallbackRecoveryRestoresPrimaryReasoning(t *testing.T) {
	a := newRestoreAgent(t, "", nil, "custom", restoreBaseURL, nil)
	a.primaryRuntime = map[string]any{
		"model": "gemini-flash", "provider": "google", "base_url": "", "api_mode": "openai", "api_key": "key",
		"client_kwargs": map[string]any{}, "use_prompt_caching": false, "use_native_cache_layout": false,
		"runtime_capabilities": map[string]any{"native_compaction": true}, "reasoning_config": map[string]any{"enabled": true, "effort": "medium"},
		"compressor_model": "gemini-flash", "compressor_base_url": "", "compressor_api_key": "", "compressor_provider": "",
		"compressor_context_length": 0, "compressor_api_mode": "", "compressor_threshold_tokens": 0,
	}
	a.fallbackActivated, a.fallbackIndex, a.fallbackChain, a.rateLimitedUntil = true, 0, nil, 0
	a.model, a.provider = "claude-opus-4.5", "anthropic"
	a.setBaseURL("")
	a.reasoningConfig = map[string]any{"enabled": true, "effort": "xhigh"}
	a.runtimeCapabilities = map[string]bool{"native_compaction": false}
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if !reflect.DeepEqual(a.reasoningConfig, map[string]any{"enabled": true, "effort": "medium"}) {
		t.Fatalf("reasoning %v", a.reasoningConfig)
	}
	if !reflect.DeepEqual(a.runtimeCapabilities, map[string]bool{"native_compaction": true}) {
		t.Fatalf("capabilities %v", a.runtimeCapabilities)
	}
}
