package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func auxQuota429(lane string) error {
	return auxExc(fmt.Sprintf("Error code: 429 - {'error': {'message': 'Weekly usage limit reached', 'type': 'usage_limit_reached', 'code': 'rate_limit_exceeded', 'lane': '%s'}}", lane), 429)
}

func auxPlain429(lane string) error {
	return auxExc(fmt.Sprintf("Error code: 429 - {'error': {'message': 'Rate limit exceeded, retry in 20s', 'lane': '%s'}}", lane), 429)
}

func auxPayment402(lane string) error {
	return auxExc(fmt.Sprintf("Error code: 402 - {'error': {'message': 'Insufficient credits', 'lane': '%s'}}", lane), 402)
}

// Seconds the custom endpoint stays quarantined
func auxQuarantineRemaining(ctx context.Context, baseURL string) float64 {
	auxUnhealthyMu.Lock()
	defer auxUnhealthyMu.Unlock()
	return auxUnhealthyUntil[auxUnhealthyCacheKey(ctx, "custom", baseURL)] - float64(time.Now().UnixNano())/1e9
}

type auxWalkLane struct {
	client AuxClient
	model  string
	label  string
}

// Provider auto primary, an empty per task chain, the main chain handing out lanes in order, empty discovery
func auxWalkFixture(t *testing.T, primary AuxClient, lanes []auxWalkLane) (context.Context, *int, *int) {
	t.Helper()
	auxClearUnhealthy(t)
	ctx := auxMainTestHome(t, "")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "auto"})
	auxSwapCachedClient(t, primary, "modelA")
	auxSwapFallbackSeams(t)
	mainCalls, discovery := new(int), new(int)
	auxTryConfiguredFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
	auxTryMainFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		i := *mainCalls
		*mainCalls++
		if i < len(lanes) {
			return lanes[i].client, lanes[i].model, lanes[i].label, nil
		}
		return nil, "", "", nil
	}
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		*discovery++
		return nil, "", "", nil
	}
	return ctx, mainCalls, discovery
}

func TestSyncWalkAdvancesPastQuotaLimitedCandidateToNextConfiguredEntry(t *testing.T) {
	primary := auxScriptedCallClient("http://127.0.0.1:1/v1", "k", auxQuota429("A"))
	laneB := auxScriptedCallClient("http://127.0.0.1:2/v1", "k", auxQuota429("B"))
	laneC := auxScriptedCallClient("http://127.0.0.1:3/v1", "k", auxCallOK("OK"))
	ctx, _, _ := auxWalkFixture(t, primary, []auxWalkLane{{laneB, "modelB", "custom"}, {laneC, "modelC", "custom"}})

	result, err := auxCallLLM(ctx, auxCallRequest{Task: "title_generation", Messages: []any{map[string]any{"role": "user", "content": "Reply OK"}}})
	if err != nil || auxActualContent(t, result) != "OK" {
		t.Fatalf("got (%v, %v)", result, err)
	}
	if auxCallCount(laneB) != 1 || auxCallCount(laneC) != 1 {
		t.Fatalf("lane calls b=%d c=%d", auxCallCount(laneB), auxCallCount(laneC))
	}
	if !auxIsProviderUnhealthy(ctx, "custom", "http://127.0.0.1:2/v1") || auxIsProviderUnhealthy(ctx, "custom", "http://127.0.0.1:3/v1") {
		t.Fatal("the quota limited candidate alone must be quarantined")
	}
}

func TestAsyncWalkExhaustsEveryConfiguredLaneOnceThenRaisesPrimaryError(t *testing.T) {
	primary := auxScriptedCallClient("http://127.0.0.1:1/v1", "k", auxQuota429("A"))
	laneB := auxScriptedCallClient("http://127.0.0.1:2/v1", "k", auxQuota429("B"))
	laneC := auxScriptedCallClient("http://127.0.0.1:3/v1", "k", auxQuota429("C"))
	ctx, mainCalls, discovery := auxWalkFixture(t, primary, []auxWalkLane{{laneB, "modelB", "custom"}, {laneC, "modelC", "custom"}})
	prevAsync := auxToAsyncClientFn
	t.Cleanup(func() { auxToAsyncClientFn = prevAsync })
	auxToAsyncClientFn = func(_ context.Context, c AuxClient, m string, _ bool) (AuxClient, string) { return c, m }

	_, err := AsyncCallLLM(ctx, map[string]any{"task": "compression", "messages": []any{map[string]any{"role": "user", "content": "summarize"}}})
	if err == nil || !strings.Contains(err.Error(), "'lane': 'A'") {
		t.Fatalf("got %v", err)
	}
	if auxCallCount(laneB) != 1 || auxCallCount(laneC) != 1 {
		t.Fatalf("lane calls b=%d c=%d", auxCallCount(laneB), auxCallCount(laneC))
	}
	if *mainCalls != 3 {
		t.Fatalf("main chain consulted %d times", *mainCalls)
	}
	if *discovery < 1 {
		t.Fatal("discovery was not consulted on exhaustion")
	}
}

func TestCandidateQuarantineTtlIsShortForTransient429AndLongForPayment(t *testing.T) {
	logs := auxCaptureLogs(t)
	primary := auxScriptedCallClient("http://127.0.0.1:1/v1", "k", auxPlain429("A"))
	laneB := auxScriptedCallClient("http://127.0.0.1:2/v1", "k", auxPlain429("B"))
	laneC := auxScriptedCallClient("http://127.0.0.1:3/v1", "k", auxPayment402("C"))
	laneD := auxScriptedCallClient("http://127.0.0.1:4/v1", "k", auxCallOK("OK"))
	ctx, _, _ := auxWalkFixture(t, primary, []auxWalkLane{{laneB, "modelB", "custom"}, {laneC, "modelC", "custom"}, {laneD, "modelD", "custom"}})

	result, err := auxCallLLM(ctx, auxCallRequest{Task: "title_generation", Messages: []any{map[string]any{"role": "user", "content": "Reply OK"}}})
	if err != nil || auxActualContent(t, result) != "OK" {
		t.Fatalf("got (%v, %v)", result, err)
	}
	if remaining := auxQuarantineRemaining(ctx, "http://127.0.0.1:2/v1"); remaining <= 0 || remaining > 60 {
		t.Fatalf("transient 429 quarantine %v", remaining)
	}
	if remaining := auxQuarantineRemaining(ctx, "http://127.0.0.1:3/v1"); remaining <= 60 {
		t.Fatalf("payment quarantine %v", remaining)
	}
	marks := logs.matching("marking local/custom unhealthy")
	if len(marks) == 0 || strings.Contains(marks[0].msg, "payment") || !strings.Contains(marks[0].msg, "rate limit") {
		t.Fatalf("marks %v", marks)
	}
}
