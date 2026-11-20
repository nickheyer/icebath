package agent

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

const softFailureBaseURL = "https://chatgpt.com/backend-api/codex"

// Codex agent on a two credential pool with its retry verdict for one soft failed response
func softFailureRun(t *testing.T, code, message string) (*AIAgent, *provider.Pool, *[]provider.PooledCredential, *LoopState, PhaseVerdict) {
	t.Helper()
	pool := seededCredentialPool(t, "openai-codex",
		poolRow(0, "tok-0-1234567890", map[string]any{"label": "acct-0", "base_url": softFailureBaseURL}),
		poolRow(1, "tok-1-1234567890", map[string]any{"label": "acct-1", "base_url": softFailureBaseURL}))
	a := newFixtureAgent(t, AIAgentOptions{Provider: "openai-codex", APIMode: "codex_responses", Model: "gpt-5.1-codex",
		BaseURL: softFailureBaseURL, APIKey: "tok-0-1234567890", EnabledToolsets: []string{}})
	t.Cleanup(func() { a.Close() })
	a.credentialPool, a.fallbackIndex = pool, 0
	a.fallbackChain = []map[string]any{{"provider": "soft-failure-unknown-provider", "model": "none"}}
	swaps := recordSwaps(t)
	savedSleep := recoverySleep
	recoverySleep = func(time.Duration) {}
	t.Cleanup(func() { recoverySleep = savedSleep })
	s := &LoopState{Response: map[string]any{"status": "failed", "output": []any{}, "output_text": "",
		"error": map[string]any{"code": code, "message": message}},
		Retry: &TurnRetryState{}, MaxRetries: 3, APICallCount: 1, APIRequestID: "r", APIDuration: 0.4, EffectiveTaskID: "t", TurnID: "turn"}
	verdict, err := retryInvalidResponse(context.Background(), a, s, []string{"response.status=failed"})
	if err != nil {
		t.Fatal(err)
	}
	return a, pool, swaps, s, verdict
}

func TestQuotaSoftFailureRotatesPoolBeforeProviderFallback(t *testing.T) {
	a, pool, swaps, _, verdict := softFailureRun(t, "usage_limit_reached", "You've hit your usage limit. Try again at 3:00 PM.")
	if verdict.Action != "continue" {
		t.Fatalf("verdict %+v", verdict)
	}
	if len(*swaps) != 1 || (*swaps)[0].ID != "cred-1" || (*swaps)[0].AccessToken != "tok-1-1234567890" {
		t.Fatalf("swaps %+v", *swaps)
	}
	if benched := poolEntryByID(pool, "cred-0"); benched.LastStatus != provider.StatusExhausted || benched.LastErrorReason != "usage_limit_reached" {
		t.Fatalf("benched %+v", benched)
	}
	if a.fallbackIndex != 0 {
		t.Fatalf("provider fallback walked to %d before the pool", a.fallbackIndex)
	}
}

func TestContentPolicySoftFailureLeavesPoolAlone(t *testing.T) {
	a, pool, swaps, s, verdict := softFailureRun(t, "content_policy_violation", "Your request was rejected by our safety system.")
	if verdict.Action != "continue" || s.RetryCount != 1 {
		t.Fatalf("verdict %+v retries %d", verdict, s.RetryCount)
	}
	if len(*swaps) != 0 || a.apiKey != "tok-0-1234567890" {
		t.Fatalf("swaps %+v key %q", *swaps, a.apiKey)
	}
	for _, e := range pool.Entries() {
		if e.LastStatus != "" {
			t.Fatalf("pool entry touched %+v", e)
		}
	}
	if a.fallbackIndex != 1 {
		t.Fatalf("provider fallback not attempted, index %d", a.fallbackIndex)
	}
}
