package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Temp home holding only the given pool rows for the provider, the loaded pool returned
func seededCredentialPool(t *testing.T, providerName string, rows ...map[string]any) *provider.Pool {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, "icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, h)
	t.Setenv("HOME", filepath.Join(root, "userhome"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	for _, v := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENROUTER_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		t.Setenv(v, "")
	}
	home.ResetKeyCache()
	config.ResetCache()
	raw, err := json.Marshal(map[string]any{"version": 1, "providers": map[string]any{}, "credential_pool": map[string]any{providerName: rows}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	pool := provider.LoadPool(agentPoolEnv(context.Background()), providerName)
	var ids []string
	for _, e := range pool.Entries() {
		ids = append(ids, e.ID)
	}
	if len(ids) != len(rows) {
		t.Fatalf("pool fixture leaked host credentials into the test pool: %v", ids)
	}
	return pool
}

func poolRow(idx int, key string, overrides map[string]any) map[string]any {
	row := map[string]any{"id": "cred-" + string(rune('0'+idx)), "label": "key-" + string(rune('0'+idx)), "auth_type": "api_key", "priority": idx, "source": "manual", "access_token": key}
	for k, v := range overrides {
		row[k] = v
	}
	return row
}

// Records every entry pool recovery adopts, answering true like the MagicMock swap
func recordSwaps(t *testing.T) *[]provider.PooledCredential {
	t.Helper()
	var swapped []provider.PooledCredential
	orig := swapPoolCredential
	swapPoolCredential = func(_ context.Context, _ *AIAgent, entry provider.PooledCredential) (bool, error) {
		swapped = append(swapped, entry)
		return true, nil
	}
	t.Cleanup(func() { swapPoolCredential = orig })
	return &swapped
}

func poolEntryByID(pool *provider.Pool, id string) provider.PooledCredential {
	for _, e := range pool.Entries() {
		if e.ID == id {
			return e
		}
	}
	return provider.PooledCredential{}
}

func intPtr(n int) *int { return &n }

// Agent on the rotation cycle pool, its live key the first entry's
func rotationCycleAgent(t *testing.T, entries int) (*AIAgent, *provider.Pool) {
	t.Helper()
	var rows []map[string]any
	for i := 0; i < entries; i++ {
		key := "sk-or-" + string(rune('a'+i))
		if i == 0 {
			key = "test-api-key"
		}
		rows = append(rows, poolRow(i, key, nil))
	}
	pool := seededCredentialPool(t, "openrouter", rows...)
	return &AIAgent{provider: "openrouter", baseURL: "https://openrouter.ai/api/v1", apiKey: "test-api-key", credentialPool: pool}, pool
}

func TestPoolRotationCycle(t *testing.T) {
	ctx := context.Background()
	t.Run("test_first_429_sets_retry_flag_no_rotation", func(t *testing.T) {
		a, pool := rotationCycleAgent(t, 3)
		swaps := recordSwaps(t)
		recovered, retried, err := a.recoverWithCredentialPool(ctx, intPtr(429), false, "", nil, false)
		if err != nil || recovered || !retried {
			t.Fatalf("got recovered=%v retried=%v err=%v", recovered, retried, err)
		}
		for _, e := range pool.Entries() {
			if e.LastStatus == provider.StatusExhausted {
				t.Fatalf("first 429 must not rotate: %+v", e)
			}
		}
		if len(*swaps) != 0 {
			t.Fatalf("swapped %v", *swaps)
		}
	})
	t.Run("test_second_429_rotates_to_next", func(t *testing.T) {
		a, pool := rotationCycleAgent(t, 3)
		swaps := recordSwaps(t)
		recovered, retried, err := a.recoverWithCredentialPool(ctx, intPtr(429), true, "", nil, false)
		if err != nil || !recovered || retried {
			t.Fatalf("got recovered=%v retried=%v err=%v", recovered, retried, err)
		}
		failed := poolEntryByID(pool, "cred-0")
		if failed.LastStatus != provider.StatusExhausted || failed.FailureReason() != "rate_limit" {
			t.Fatalf("failed entry not benched for rate_limit: %+v", failed)
		}
		if len(*swaps) != 1 || (*swaps)[0].ID != "cred-1" {
			t.Fatalf("swapped %v", *swaps)
		}
	})
	t.Run("test_pool_exhaustion_returns_false", func(t *testing.T) {
		a, _ := rotationCycleAgent(t, 1)
		recordSwaps(t)
		_, retried, err := a.recoverWithCredentialPool(ctx, intPtr(429), false, "", nil, false)
		if err != nil || !retried {
			t.Fatalf("first 429 retried=%v err=%v", retried, err)
		}
		recovered, _, err := a.recoverWithCredentialPool(ctx, intPtr(429), true, "", nil, false)
		if err != nil || recovered {
			t.Fatalf("exhausted pool recovered=%v err=%v", recovered, err)
		}
	})
	t.Run("test_402_immediate_rotation", func(t *testing.T) {
		a, pool := rotationCycleAgent(t, 3)
		recordSwaps(t)
		recovered, retried, err := a.recoverWithCredentialPool(ctx, intPtr(402), false, "", nil, false)
		if err != nil || !recovered || retried {
			t.Fatalf("got recovered=%v retried=%v err=%v", recovered, retried, err)
		}
		if failed := poolEntryByID(pool, "cred-0"); failed.LastStatus != provider.StatusExhausted || failed.FailureReason() != "billing" {
			t.Fatalf("failed entry not benched for billing: %+v", failed)
		}
	})
	t.Run("test_api_key_hint_from_pool_current_when_agent_key_missing", func(t *testing.T) {
		pool := seededCredentialPool(t, "openrouter", poolRow(0, "pool-current-key", nil), poolRow(1, "sk-or-b", nil))
		if cur, ok := pool.Select(""); !ok || cur.AccessToken != "pool-current-key" {
			t.Fatalf("select %+v %v", cur, ok)
		}
		a := &AIAgent{provider: "openrouter", baseURL: "https://openrouter.ai/api/v1", credentialPool: pool}
		swaps := recordSwaps(t)
		recovered, _, err := a.recoverWithCredentialPool(ctx, intPtr(402), false, "", nil, false)
		if err != nil || !recovered {
			t.Fatalf("got recovered=%v err=%v", recovered, err)
		}
		if failed := poolEntryByID(pool, "cred-0"); failed.LastStatus != provider.StatusExhausted || failed.FailureReason() != "billing" {
			t.Fatalf("hinted entry not benched: %+v", failed)
		}
		if len(*swaps) != 1 || (*swaps)[0].ID != "cred-1" {
			t.Fatalf("swapped %v", *swaps)
		}
	})
}

func failureAttributionAgent(pool *provider.Pool, failingKey, credentialID string) *AIAgent {
	return &AIAgent{provider: "anthropic", apiKey: failingKey, credentialPool: pool, credentialPoolEntryID: credentialID}
}

func TestFailureAttribution(t *testing.T) {
	ctx := context.Background()
	t.Run("test_pre_exhausted_check_uses_failing_key", func(t *testing.T) {
		pool := seededCredentialPool(t, "anthropic", poolRow(0, "key-a", nil),
			poolRow(1, "key-b", map[string]any{"last_status": "exhausted", "last_status_at": float64(time.Now().Unix()), "last_error_code": 429}))
		a := failureAttributionAgent(pool, "key-b", "")
		swaps := recordSwaps(t)
		recovered, retried, err := a.recoverWithCredentialPool(ctx, intPtr(429), false, "", nil, false)
		if err != nil || !recovered || retried {
			t.Fatalf("got recovered=%v retried=%v err=%v", recovered, retried, err)
		}
		if poolEntryByID(pool, "cred-0").LastStatus == provider.StatusExhausted {
			t.Fatal("healthy entry exhausted")
		}
		if len(*swaps) != 1 || (*swaps)[0].ID != "cred-0" {
			t.Fatalf("swapped %v", *swaps)
		}
	})
	t.Run("test_auth_refresh_targets_failing_key_not_pointer", func(t *testing.T) {
		pool := seededCredentialPool(t, "anthropic", poolRow(0, "key-a", nil), poolRow(1, "key-b", nil))
		if sel, ok := pool.Select(""); !ok || sel.ID != "cred-0" {
			t.Fatalf("select %+v", sel)
		}
		if cur, ok := pool.Current(); !ok || cur.ID != "cred-0" {
			t.Fatalf("current %+v", cur)
		}
		a := failureAttributionAgent(pool, "key-b", "")
		swaps := recordSwaps(t)
		recovered, _, err := a.recoverWithCredentialPool(ctx, intPtr(401), false, "", nil, false)
		if err != nil || !recovered {
			t.Fatalf("got recovered=%v err=%v", recovered, err)
		}
		if poolEntryByID(pool, "cred-1").LastStatus != provider.StatusExhausted || poolEntryByID(pool, "cred-0").LastStatus == provider.StatusExhausted {
			t.Fatalf("statuses %+v", pool.Entries())
		}
		if len(*swaps) == 0 || (*swaps)[len(*swaps)-1].ID != "cred-0" {
			t.Fatalf("swapped %v", *swaps)
		}
	})
	t.Run("test_unmatched_key_does_not_retry_only_pool_entry", func(t *testing.T) {
		pool := seededCredentialPool(t, "anthropic", poolRow(0, "pool-runtime-key", nil))
		a := failureAttributionAgent(pool, "wrapper-runtime-key", "")
		swaps := recordSwaps(t)
		recovered, _, err := a.recoverWithCredentialPool(ctx, intPtr(401), false, "", nil, false)
		if err != nil || recovered {
			t.Fatalf("got recovered=%v err=%v", recovered, err)
		}
		if poolEntryByID(pool, "cred-0").LastStatus == provider.StatusExhausted {
			t.Fatal("sole entry exhausted")
		}
		if len(*swaps) != 0 {
			t.Fatalf("swapped %v", *swaps)
		}
	})
	t.Run("test_classified_billing_403_recorded_on_entry", func(t *testing.T) {
		pool := seededCredentialPool(t, "anthropic", poolRow(0, "key-a", nil), poolRow(1, "key-b", nil))
		a := failureAttributionAgent(pool, "key-b", "")
		recordSwaps(t)
		if _, _, err := a.recoverWithCredentialPool(ctx, intPtr(403), false, provider.ReasonBilling, nil, false); err != nil {
			t.Fatal(err)
		}
		if failed := poolEntryByID(pool, "cred-1"); failed.LastStatus != provider.StatusExhausted || failed.FailureReason() != "billing" {
			t.Fatalf("billing 403 not recorded: %+v", failed)
		}
	})
	t.Run("test_unclassified_403_records_no_billing_reason", func(t *testing.T) {
		pool := seededCredentialPool(t, "anthropic", poolRow(0, "key-a", nil), poolRow(1, "key-b", nil))
		a := failureAttributionAgent(pool, "key-b", "")
		recordSwaps(t)
		if _, _, err := a.recoverWithCredentialPool(ctx, intPtr(403), false, "", nil, false); err != nil {
			t.Fatal(err)
		}
		if failed := poolEntryByID(pool, "cred-1"); failed.FailureReason() == "billing" {
			t.Fatalf("unclassified 403 invented billing: %+v", failed)
		}
	})
}

func TestRotateImmediatelyWhenCredentialAlreadyExhausted(t *testing.T) {
	pool := seededCredentialPool(t, "openrouter",
		poolRow(0, "key-0", map[string]any{"last_status": "exhausted", "last_status_at": float64(time.Now().Unix()), "last_error_code": 429}),
		poolRow(1, "key-1", nil))
	a := &AIAgent{provider: "openrouter", baseURL: "https://openrouter.ai/api/v1", apiKey: "key-0", credentialPool: pool}
	swaps := recordSwaps(t)
	recovered, retried, err := a.recoverWithCredentialPool(context.Background(), intPtr(429), false, provider.ReasonRateLimit, nil, false)
	if err != nil || !recovered || retried {
		t.Fatalf("got recovered=%v retried=%v err=%v", recovered, retried, err)
	}
	if len(*swaps) != 1 || (*swaps)[0].ID != "cred-1" {
		t.Fatalf("swapped %v", *swaps)
	}
}

func TestRotationOnMixedPoolNeverRebindsToAnEntryForAnotherEndpoint(t *testing.T) {
	const azure, public = "https://res.cognitiveservices.azure.com/openai/v1", "https://api.openai.com/v1"
	entry := func(id, url string) map[string]any {
		return map[string]any{"id": id, "label": id, "auth_type": "api_key", "priority": 0, "source": "env:" + id, "access_token": "key-" + id, "base_url": url}
	}
	pool := seededCredentialPool(t, "openai", entry("az", azure), entry("pub", public))
	a := &AIAgent{provider: "openai", model: "gpt-5.4", baseURL: azure, apiKey: "key-az", credentialPool: pool, credentialPoolEntryID: "az"}
	orig := swapPoolCredential
	swapPoolCredential = func(_ context.Context, a *AIAgent, e provider.PooledCredential) (bool, error) {
		a.apiKey, a.baseURL, a.credentialPoolEntryID = e.RuntimeAPIKey(), e.BaseURL, e.ID
		return true, nil
	}
	t.Cleanup(func() { swapPoolCredential = orig })
	recovered, _, err := a.recoverWithCredentialPool(context.Background(), intPtr(429), true, "", map[string]any{"message": "Rate limit"}, false)
	if err != nil || recovered {
		t.Fatalf("got recovered=%v err=%v", recovered, err)
	}
	if a.apiKey != "key-az" || a.baseURL != azure || a.credentialPoolEntryID != "az" {
		t.Fatalf("session rebound to %s %s %s", a.apiKey, a.baseURL, a.credentialPoolEntryID)
	}
	if poolEntryByID(pool, "az").LastStatus != provider.StatusExhausted {
		t.Fatal("failed azure entry not benched")
	}
}
