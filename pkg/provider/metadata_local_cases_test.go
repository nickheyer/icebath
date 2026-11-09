package provider

import (
	"context"
	"testing"
	"time"
)

func TestGenericPreCatalogStaleGuardAbsorbedPr37684Models(t *testing.T) {
	if !stalePreCatalogCacheEntry("qwen3.6-plus", 131_072) || !stalePreCatalogCacheEntry("alibaba/qwen3.6-plus", 131_072) {
		t.Fatal("the qwen catch all value persisted for qwen3.6-plus must be dropped")
	}
	for _, c := range []struct {
		model string
		n     int
	}{{"qwen3.6-plus", 1_048_576}, {"qwen3.6-plus", 262_144}, {"qwen3-coder", 131_072}} {
		if stalePreCatalogCacheEntry(c.model, c.n) {
			t.Fatalf("%s %d must survive", c.model, c.n)
		}
	}
}

func TestQueryLocalContextLengthVllmVllmMaxModelLen(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:8000/v1", "vllm")
	ft.on("GET", "http://127.0.0.1:8000/v1/models/omnicoder-9b", 200, map[string]any{"id": "omnicoder-9b", "max_model_len": 100000})
	mustEqual(t, queryLocalContextLength(ctx, "omnicoder-9b", "http://localhost:8000/v1", ""), 100000, "max_model_len")
}

func TestContextLengthFromModelPayloadPrefersMaxModelLenOverMaxTokens(t *testing.T) {
	n, ok := contextLengthFromModelPayload(map[string]any{"id": "local-model", "max_model_len": 131072, "max_tokens": 4096})
	if !ok || n != 131072 {
		t.Fatalf("got %d %v", n, ok)
	}
}

func TestLocalContextProbeTTLCacheSecondCallWithinTtlDoesNotReprobe(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	seedServerType("http://localhost:11434/v1", "ollama")
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, map[string]any{"model_info": map[string]any{"llama.context_length": 32768}})
	ft.on("GET", "http://127.0.0.1:11434/v1/models", 404, map[string]any{})
	mustEqual(t, queryLocalContextLength(ctx, "m", "http://localhost:11434/v1", ""), 32768, "first")
	requests := ft.count()
	if requests == 0 {
		t.Fatal("the first call must probe the server")
	}
	mustEqual(t, queryLocalContextLength(ctx, "m", "http://localhost:11434/v1", ""), 32768, "second")
	mustEqual(t, ft.count(), requests, "served from the probe cache")
}

func TestContextCacheKeyExportMatchesUnexported(t *testing.T) {
	mustEqual(t, ContextCacheKey("m", "http://x/v1/"), contextCacheKey("m", "http://x/v1/"), "key")
}

func TestSwapBedrockProbeAndRegionRestorePrevious(t *testing.T) {
	prevProbe := SwapBedrockProbe(func(context.Context, string, string) int { return 7 })
	prevRegion := SwapResolveBedrockRegion(func() string { return "eu-west-1" })
	t.Cleanup(func() {
		SwapBedrockProbe(prevProbe)
		SwapResolveBedrockRegion(prevRegion)
	})
	mustEqual(t, bedrockProbeFn(context.Background(), "m", "r"), 7, "probe installed")
	mustEqual(t, resolveBedrockRegionFn(), "eu-west-1", "region installed")
	installed := SwapBedrockProbe(prevProbe)
	mustEqual(t, installed(context.Background(), "m", "r"), 7, "previous probe handed back")
	mustEqual(t, SwapResolveBedrockRegion(prevRegion)(), "eu-west-1", "previous region handed back")
}

func TestInvalidateCachedContextLengthExportDropsTheRow(t *testing.T) {
	ctx, _, _ := metaTest(t)
	SaveContextLength(ctx, "m", "https://e.example/v1", 64_000, "")
	InvalidateCachedContextLength(ctx, "m", "https://e.example/v1")
	if n, ok := GetCachedContextLength(ctx, "m", "https://e.example/v1", false); ok {
		t.Fatalf("row survived as %d", n)
	}
}

func TestResetBedrockProbeFailureCacheEmptiesTheMemo(t *testing.T) {
	bedrockProbeFailureMu.Lock()
	bedrockProbeFailureCache[bedrockProbeKey{}] = time.Now()
	bedrockProbeFailureMu.Unlock()
	ResetBedrockProbeFailureCache()
	bedrockProbeFailureMu.Lock()
	n := len(bedrockProbeFailureCache)
	bedrockProbeFailureMu.Unlock()
	mustEqual(t, n, 0, "memo cleared")
}
