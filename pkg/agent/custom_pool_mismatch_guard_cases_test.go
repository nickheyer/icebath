package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

const geminiNoFilterURL = "https://generativelanguage.googleapis.com/v1beta"

// Writes the config the patched _iter_custom_providers or pool key lookup answers from
func writeCustomProvidersConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home.Dir(context.Background()), "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

const geminiConfig = "custom_providers:\n  - name: Gemini No Filter\n    provider_key: gemini-no-filter\n    base_url: " + geminiNoFilterURL + "\n"

// Pool statuses keyed by entry id, the observable state a guarded recovery must leave alone
func poolStatuses(pool *provider.Pool) map[string]string {
	out := map[string]string{}
	for _, e := range pool.Entries() {
		out[e.ID] = e.LastStatus
	}
	return out
}

func geminiPool(t *testing.T) *provider.Pool {
	t.Helper()
	pool := seededCredentialPool(t, "custom:gemini-no-filter", poolRow(0, "key-a", map[string]any{"id": "key-a"}), poolRow(1, "key-b", map[string]any{"id": "key-b"}))
	writeCustomProvidersConfig(t, geminiConfig)
	return pool
}

func TestCustomPoolMismatchGuard(t *testing.T) {
	ctx := context.Background()
	t.Run("test_named_custom_provider_rotates_its_matching_pool", func(t *testing.T) {
		pool := geminiPool(t)
		swaps := recordSwaps(t)
		a := &AIAgent{provider: "gemini-no-filter", apiKey: "key-a", credentialPool: pool}
		a.setBaseURL(geminiNoFilterURL)
		recovered, retried, err := a.recoverWithCredentialPool(ctx, intPtr(429), true, provider.ReasonRateLimit, nil, false)
		if err != nil || !recovered || retried || len(*swaps) != 1 || (*swaps)[0].ID != "key-b" {
			t.Fatalf("recovered %v retried %v err %v swaps %v", recovered, retried, err, *swaps)
		}
	})
	t.Run("test_exact_custom_identity_requires_matching_endpoint", func(t *testing.T) {
		pool := geminiPool(t)
		swaps := recordSwaps(t)
		before := poolStatuses(pool)
		a := &AIAgent{provider: "custom:gemini-no-filter", credentialPool: pool}
		a.setBaseURL("https://fallback.example/v1")
		recovered, retried, err := a.recoverWithCredentialPool(ctx, intPtr(429), true, provider.ReasonRateLimit, nil, false)
		if err != nil || recovered || !retried || len(*swaps) != 0 {
			t.Fatalf("recovered %v retried %v err %v swaps %v", recovered, retried, err, *swaps)
		}
		if after := poolStatuses(pool); len(after) != len(before) || after["key-a"] != before["key-a"] || after["key-b"] != before["key-b"] {
			t.Fatalf("pool mutated %v -> %v", before, after)
		}
	})
	t.Run("test_exact_custom_identity_rotates_at_matching_endpoint", func(t *testing.T) {
		pool := geminiPool(t)
		swaps := recordSwaps(t)
		a := &AIAgent{provider: "custom:gemini-no-filter", apiKey: "key-a", credentialPool: pool}
		a.setBaseURL(geminiNoFilterURL)
		recovered, retried, err := a.recoverWithCredentialPool(ctx, intPtr(429), true, provider.ReasonRateLimit, nil, false)
		if err != nil || !recovered || retried || len(*swaps) != 1 || (*swaps)[0].ID != "key-b" {
			t.Fatalf("recovered %v retried %v err %v swaps %v", recovered, retried, err, *swaps)
		}
	})
	t.Run("test_unrelated_custom_pool_still_guarded", func(t *testing.T) {
		pool := seededCredentialPool(t, "custom:fireworks", poolRow(0, "fw-a", nil), poolRow(1, "fw-b", nil))
		writeCustomProvidersConfig(t, "custom_providers:\n  - name: fireworks\n    base_url: https://api.fireworks.ai/inference/v1\n  - name: other\n    base_url: https://other-endpoint.example/v1\n")
		swaps := recordSwaps(t)
		before := poolStatuses(pool)
		a := &AIAgent{provider: "custom", credentialPool: pool}
		a.setBaseURL("https://other-endpoint.example/v1")
		recovered, _, err := a.recoverWithCredentialPool(ctx, intPtr(401), false, provider.ReasonAuth, nil, false)
		if err != nil || recovered || len(*swaps) != 0 {
			t.Fatalf("recovered %v err %v swaps %v", recovered, err, *swaps)
		}
		if after := poolStatuses(pool); after["cred-0"] != before["cred-0"] || after["cred-1"] != before["cred-1"] {
			t.Fatalf("pool mutated %v -> %v", before, after)
		}
	})
	t.Run("test_fallback_provider_still_guarded", func(t *testing.T) {
		pool := seededCredentialPool(t, "custom:fireworks", poolRow(0, "fw-a", nil), poolRow(1, "fw-b", nil))
		swaps := recordSwaps(t)
		before := poolStatuses(pool)
		a := &AIAgent{provider: "openai-codex", credentialPool: pool}
		a.setBaseURL("https://chatgpt.com/backend-api")
		recovered, _, err := a.recoverWithCredentialPool(ctx, intPtr(401), false, provider.ReasonAuth, nil, false)
		if err != nil || recovered || len(*swaps) != 0 {
			t.Fatalf("recovered %v err %v swaps %v", recovered, err, *swaps)
		}
		if after := poolStatuses(pool); after["cred-0"] != before["cred-0"] || after["cred-1"] != before["cred-1"] {
			t.Fatalf("pool mutated %v -> %v", before, after)
		}
	})
}

// Real openrouter pool with a healthy first entry and the failed key second
func hintPool(t *testing.T) *provider.Pool {
	t.Helper()
	return seededCredentialPool(t, "openrouter",
		map[string]any{"id": "cred-healthy", "label": "healthy", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-or-healthy"},
		map[string]any{"id": "cred-failed", "label": "failed", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": "sk-or-failed"})
}

func TestApiKeyHintRealPool(t *testing.T) {
	t.Run("test_hint_marks_failed_entry_not_current", func(t *testing.T) {
		pool := hintPool(t)
		if sel, ok := pool.Select(""); !ok || sel.AccessToken != "sk-or-healthy" {
			t.Fatalf("select %+v %v", sel, ok)
		}
		next, ok := pool.MarkExhaustedAndRotate(provider.RotationRequest{StatusCode: 429, ErrorContext: map[string]any{"reason": "rate_limit_exceeded"}, APIKeyHint: "sk-or-failed"})
		statuses := poolStatuses(pool)
		if statuses["cred-failed"] != provider.StatusExhausted || (statuses["cred-healthy"] != "" && statuses["cred-healthy"] != "ok") {
			t.Fatalf("statuses %v", statuses)
		}
		if !ok || next.AccessToken != "sk-or-healthy" {
			t.Fatalf("next %+v %v", next, ok)
		}
	})
	t.Run("test_without_hint_current_entry_is_marked", func(t *testing.T) {
		pool := hintPool(t)
		if sel, ok := pool.Select(""); !ok || sel.AccessToken != "sk-or-healthy" {
			t.Fatalf("select %+v %v", sel, ok)
		}
		pool.MarkExhaustedAndRotate(provider.RotationRequest{StatusCode: 429})
		statuses := poolStatuses(pool)
		if statuses["cred-healthy"] != provider.StatusExhausted || (statuses["cred-failed"] != "" && statuses["cred-failed"] != "ok") {
			t.Fatalf("statuses %v", statuses)
		}
	})
}
