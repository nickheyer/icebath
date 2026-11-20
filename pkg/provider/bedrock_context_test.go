package provider

import (
	"context"
	"os"
	"testing"
	"time"
)

// Probe and region seams with a scripted probe answer sequence, returning the probe call log
func bedrockFakeProbe(t *testing.T, answers ...int) *[]string {
	calls := &[]string{}
	prevProbe, prevRegion := bedrockProbeFn, resolveBedrockRegionFn
	bedrockProbeFn = func(_ context.Context, model, region string) int {
		*calls = append(*calls, model+"|"+region)
		return answers[min(len(*calls), len(answers))-1]
	}
	resolveBedrockRegionFn = func() string { return "us-east-1" }
	clearMemo := func() {
		bedrockProbeFailureMu.Lock()
		clear(bedrockProbeFailureCache)
		bedrockProbeFailureMu.Unlock()
	}
	clearMemo()
	t.Cleanup(func() { bedrockProbeFn, resolveBedrockRegionFn = prevProbe, prevRegion; clearMemo() })
	return calls
}

func TestBedrockClaude46IgnoresStale200kCache(t *testing.T) {
	bedrockIsolatedEnv(t)
	bedrockFakeProbe(t, 0)
	base := "https://bedrock-runtime.us-east-2.amazonaws.com"
	ctx := context.Background()
	SaveContextLength(ctx, "us.anthropic.claude-sonnet-4-6", base, 200_000, "")
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: "us.anthropic.claude-sonnet-4-6", Provider: "bedrock", BaseURL: base}); got != 1_000_000 {
		t.Fatalf("bedrock claude 4.6 context %d", got)
	}
}

func TestBedrockContextCachePersistenceFailedProbeDoesNotPersistStaticFallback(t *testing.T) {
	bedrockIsolatedEnv(t)
	calls := bedrockFakeProbe(t, 0)
	ctx := context.Background()
	model := "amazon.future-model-v1:0"
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: model, Provider: "bedrock"}); got != BedrockDefaultContextLength {
		t.Fatalf("fallback %d", got)
	}
	if n, ok := GetCachedContextLength(ctx, model, "bedrock://", false); ok {
		t.Fatalf("static fallback persisted as %d", n)
	}
	if _, err := os.Stat(contextCachePath(ctx)); !os.IsNotExist(err) {
		t.Fatalf("cache file written: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != model+"|us-east-1" {
		t.Fatalf("probe calls %v", *calls)
	}
}

func TestBedrockFailedProbeIsMemoisedUntilTheTTLLapses(t *testing.T) {
	bedrockIsolatedEnv(t)
	calls := bedrockFakeProbe(t, 0, 1_000_000)
	ctx := context.Background()
	model := "amazon.future-model-v1:0"
	req := ContextLengthRequest{Model: model, Provider: "bedrock"}
	for i := 0; i < 2; i++ {
		if got := GetModelContextLength(ctx, req); got != BedrockDefaultContextLength {
			t.Fatalf("resolution %d answered %d", i, got)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("failed probe re-sent %d times inside the memo window", len(*calls))
	}
	if _, err := os.Stat(contextCachePath(ctx)); !os.IsNotExist(err) {
		t.Fatalf("memo leaked to disk: %v", err)
	}
	bedrockProbeFailureMu.Lock()
	for k := range bedrockProbeFailureCache {
		bedrockProbeFailureCache[k] = time.Now().Add(-bedrockProbeFailureTTL - time.Second)
	}
	bedrockProbeFailureMu.Unlock()
	if got := GetModelContextLength(ctx, req); got != 1_000_000 {
		t.Fatalf("after memo lapse %d", got)
	}
	if n, ok := GetCachedContextLength(ctx, model, "bedrock://", false); !ok || n != 1_000_000 {
		t.Fatalf("probe window not persisted: %d %v", n, ok)
	}
	if len(*calls) != 2 {
		t.Fatalf("probe calls %d", len(*calls))
	}
}
