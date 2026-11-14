package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

const entitlementModel, entitlementOtherModel = "gpt-5.3-codex", "gpt-5.3-codex-mini"

var entitlementTokens = []string{"tok-account-a", "tok-account-b"}

// Two oauth Codex accounts in priority order
func codexEntitlementPool(t *testing.T) *provider.Pool {
	t.Helper()
	var rows []map[string]any
	for i, tok := range entitlementTokens {
		rows = append(rows, map[string]any{"id": fmt.Sprintf("cred-%d", i), "label": fmt.Sprintf("acct-%d", i), "auth_type": "oauth",
			"priority": i, "source": "manual", "access_token": tok, "refresh_token": fmt.Sprintf("rt-%d", i), "expires_at_ms": 4_000_000_000_000})
	}
	return seededCredentialPool(t, "openai-codex", rows...)
}

// Codex 400 with the given JSON body the way the SDK raises it
func codexStatusError(t *testing.T, status int, body map[string]any) error {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &provider.APIError{Provider: "openai-codex", Model: entitlementModel, StatusCode: status, Body: string(raw),
		Message: fmt.Sprintf("Error code: %d - %s", status, raw)}
}

func codexEntitlementAgent(pool *provider.Pool) *AIAgent {
	return &AIAgent{provider: "openai-codex", model: entitlementModel, baseURL: "https://chatgpt.com/backend-api/codex",
		apiKey: entitlementTokens[0], credentialPool: pool, credentialPoolEntryID: "cred-0"}
}

func TestEntitlement400BenchesOnlyThatModelAndRotates(t *testing.T) {
	pool := codexEntitlementPool(t)
	entitlement := codexStatusError(t, 400, map[string]any{"detail": "The '" + entitlementModel + "' model is not supported when using Codex with a ChatGPT account."})
	verdict := provider.ClassifyError(provider.ClassifyInput{Err: entitlement, Status: 400, Provider: "openai-codex", Model: entitlementModel})
	if verdict.Reason != provider.ReasonModelEntitlement || !verdict.ShouldRotateCredential || !verdict.ShouldFallback || verdict.Retryable {
		t.Fatalf("verdict %+v", verdict)
	}
	generic := provider.ClassifyError(provider.ClassifyInput{Err: codexStatusError(t, 400, map[string]any{"detail": "Invalid request: bad field"}),
		Status: 400, Provider: "openai-codex", Model: entitlementModel})
	if generic.Reason != provider.ReasonFormatError || generic.ShouldRotateCredential {
		t.Fatalf("generic %+v", generic)
	}
	if e, ok := pool.Select(entitlementModel); !ok || e.ID != "cred-0" {
		t.Fatalf("selected %v %v", e.ID, ok)
	}
	swaps := recordSwaps(t)
	a := codexEntitlementAgent(pool)
	recovered, retried, err := a.recoverWithCredentialPool(context.Background(), intPtr(400), false, verdict.Reason, nil, false)
	if err != nil || !recovered || retried {
		t.Fatalf("recovered %v retried %v err %v", recovered, retried, err)
	}
	if len(*swaps) != 1 || (*swaps)[0].ID != "cred-1" {
		t.Fatalf("swapped %v", *swaps)
	}
	first := pool.Entries()[0]
	if first.LastStatus != "" || len(first.ModelCooldowns) != 1 {
		t.Fatalf("first entry %+v", first)
	}
	if until, ok := first.ModelCooldowns[entitlementModel]; !ok || until <= nowSeconds()+24*3600 {
		t.Fatalf("model cooldown %v", first.ModelCooldowns)
	}
	if e, ok := pool.Select(entitlementOtherModel); !ok || e.ID != "cred-0" {
		t.Fatalf("other model selected %v %v", e.ID, ok)
	}
	if pool.ResetStatuses() < 1 || len(pool.Entries()[0].ModelCooldowns) != 0 {
		t.Fatal("reset must clear the model bench")
	}
}

func TestAPlanLimitBenchesOnlyTheRefusedModelUntilItsReset(t *testing.T) {
	pool := codexEntitlementPool(t)
	recordSwaps(t)
	reset := float64(time.Now().UnixNano())/1e9 + 3*3600
	a := codexEntitlementAgent(pool)
	if _, _, err := a.recoverWithCredentialPool(context.Background(), intPtr(402), false, provider.ReasonBilling,
		map[string]any{"usage_window": true, "reset_at": reset}, false); err != nil {
		t.Fatal(err)
	}
	first := pool.Entries()[0]
	if first.LastStatus != "" || len(first.ModelCooldowns) != 1 || math.Abs(first.ModelCooldowns[entitlementModel]-reset) >= 2 {
		t.Fatalf("first entry %+v", first)
	}
	if e, ok := pool.Select(entitlementOtherModel); !ok || e.ID != "cred-0" {
		t.Fatalf("other model selected %v %v", e.ID, ok)
	}
}
