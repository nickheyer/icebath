package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/home"
)

const grokBedrockModel = "global.xai.grok-4.6"

var bedrockBaseURLs = []string{"", "https://bedrock-runtime.us-east-1.amazonaws.com"}

// Cache url the Bedrock resolver keys the model under
func bedrockCacheURL(baseURL string) string {
	if baseURL == "" {
		return "bedrock://"
	}
	return baseURL
}

func bedrockResolve(ctx context.Context, model, baseURL string) int {
	return GetModelContextLength(ctx, ContextLengthRequest{Model: model, Provider: "bedrock", BaseURL: baseURL})
}

// Writes the raw context cache document of the home
func writeBedrockCacheDocument(t *testing.T, h string, document map[string]any) {
	t.Helper()
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "context_length_cache.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFailureMemoRetryScopeAndExpiry(t *testing.T) {
	for _, baseURL := range bedrockBaseURLs {
		for _, retry := range []string{"ttl", "invalidate", "restart", "profile", "endpoint"} {
			h := bedrockIsolatedEnv(t)
			calls := bedrockStubProbe(t, 0, 96_000)
			ctx := context.Background()
			if bedrockResolve(ctx, grokBedrockModel, baseURL) != 500_000 || bedrockResolve(ctx, grokBedrockModel, baseURL) != 500_000 {
				t.Fatalf("%s/%s: the static table must answer after a failed probe", baseURL, retry)
			}
			if len(*calls) != 1 {
				t.Fatalf("%s/%s: %d probes before the retry", baseURL, retry, len(*calls))
			}
			if fileExists(filepath.Join(h, "context_length_cache.yaml")) {
				t.Fatalf("%s/%s: a failed probe wrote the cache", baseURL, retry)
			}
			retryURL := baseURL
			switch retry {
			case "ttl":
				bedrockProbeFailureMu.Lock()
				for key := range bedrockProbeFailureCache {
					bedrockProbeFailureCache[key] = monotonicNow().Add(-bedrockProbeFailureTTL - time.Second)
				}
				bedrockProbeFailureMu.Unlock()
			case "invalidate":
				invalidateCachedContextLength(ctx, grokBedrockModel, bedrockCacheURL(baseURL))
			case "restart":
				bedrockProbeFailureMu.Lock()
				clear(bedrockProbeFailureCache)
				bedrockProbeFailureMu.Unlock()
			case "profile":
				other := filepath.Join(h, "other-profile")
				if err := os.MkdirAll(other, 0o755); err != nil {
					t.Fatal(err)
				}
				ctx = home.WithOverride(ctx, other)
			case "endpoint":
				retryURL = "https://bedrock-runtime.us-east-1.amazonaws.com/other"
			}
			if got := bedrockResolve(ctx, grokBedrockModel, retryURL); got != 96_000 {
				t.Fatalf("%s/%s: retry resolved %d", baseURL, retry, got)
			}
			if len(*calls) != 2 {
				t.Fatalf("%s/%s: %d probes after the retry", baseURL, retry, len(*calls))
			}
			if retry == "ttl" {
				bedrockProbeFailureMu.Lock()
				remaining := len(bedrockProbeFailureCache)
				bedrockProbeFailureMu.Unlock()
				if remaining != 0 {
					t.Fatalf("expired memo rows survived: %d", remaining)
				}
			}
		}
	}
}

func TestUnknownModelFallbackAndHostInference(t *testing.T) {
	bedrockIsolatedEnv(t)
	bedrockStubProbe(t, 0)
	ctx := context.Background()
	baseURL := "https://bedrock-runtime.us-east-1.amazonaws.com"
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: "unknown.future-model", BaseURL: baseURL}); got != BedrockDefaultContextLength {
		t.Fatalf("unknown model resolved %d", got)
	}
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: "xai.grok-4.6", BaseURL: baseURL}); got != 500_000 {
		t.Fatalf("grok resolved %d", got)
	}
	if _, ok := GetCachedContextLength(ctx, "unknown.future-model", baseURL, false); ok {
		t.Fatal("the fallback must not be cached")
	}
}

func TestReadonlyLegacyCacheDoesNotResetProbeCooldown(t *testing.T) {
	for _, baseURL := range bedrockBaseURLs {
		h := bedrockIsolatedEnv(t)
		writeBedrockCacheDocument(t, h, map[string]any{"context_lengths": map[string]any{contextCacheKey(grokBedrockModel, bedrockCacheURL(baseURL)): 128_000}})
		calls := bedrockStubProbe(t, 0)
		previous := writeContextCache
		writeContextCache = func(context.Context, map[string]int, map[string]any) error { return errors.New("read-only") }
		t.Cleanup(func() { writeContextCache = previous })
		for range 3 {
			if got := bedrockResolve(context.Background(), grokBedrockModel, baseURL); got != 500_000 {
				t.Fatalf("%s: resolved %d", baseURL, got)
			}
		}
		if len(*calls) != 1 {
			t.Fatalf("%s: %d probes, the cooldown was reset", baseURL, len(*calls))
		}
	}
}

func TestProvenanceIsBackwardReadableAndGenericWritesClearIt(t *testing.T) {
	for _, rewrite := range []int{0, 96_000, 120_000} {
		h := bedrockIsolatedEnv(t)
		bedrockStubProbe(t, 96_000, 110_000)
		ctx := context.Background()
		model, baseURL := "xai.grok-4.6", "https://bedrock-runtime.us-east-1.amazonaws.com"
		if got := GetModelContextLength(ctx, ContextLengthRequest{Model: model, BaseURL: baseURL}); got != 96_000 {
			t.Fatalf("rewrite %d: resolved %d", rewrite, got)
		}
		raw, err := os.ReadFile(filepath.Join(h, "context_length_cache.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := yaml.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		lengths, _ := document["context_lengths"].(map[string]any)
		if n, ok := intValue(lengths[contextCacheKey(model, baseURL)]); !ok || n+1 != 96_001 {
			t.Fatalf("rewrite %d: raw cache row %v", rewrite, lengths)
		}
		want := 96_000
		if rewrite != 0 {
			SaveContextLength(ctx, model, baseURL, rewrite, "")
			want = 110_000
		}
		if got := GetModelContextLength(ctx, ContextLengthRequest{Model: model, BaseURL: baseURL}); got != want {
			t.Fatalf("rewrite %d: resolved %d want %d", rewrite, got, want)
		}
	}
}

func TestMalformedLengthsDoNotBlockProviderPersistence(t *testing.T) {
	for _, lengths := range []any{true, []any{128_000}, "bad"} {
		h := bedrockIsolatedEnv(t)
		writeBedrockCacheDocument(t, h, map[string]any{"context_lengths": lengths})
		bedrockStubProbe(t, 96_000)
		ctx := context.Background()
		if got := bedrockResolve(ctx, "xai.grok-4.6", ""); got != 96_000 {
			t.Fatalf("%v: resolved %d", lengths, got)
		}
		if got, ok := GetCachedContextLength(ctx, "xai.grok-4.6", "bedrock://", false); !ok || got != 96_000 {
			t.Fatalf("%v: cached %d %t", lengths, got, ok)
		}
	}
}

func TestMalformedOrMismatchedProvenanceRequiresRevalidation(t *testing.T) {
	for _, marker := range []any{true, "96000", 128_000, []any{96_000}, map[string]any{"source": "probe"}} {
		h := bedrockIsolatedEnv(t)
		key := contextCacheKey("xai.grok-4.6", "bedrock://")
		writeBedrockCacheDocument(t, h, map[string]any{"context_lengths": map[string]any{key: 96_000}, "bedrock_confirmed_v1": map[string]any{key: marker}})
		calls := bedrockStubProbe(t, 100_000)
		if got := bedrockResolve(context.Background(), "xai.grok-4.6", ""); got != 100_000 {
			t.Fatalf("marker %v: resolved %d", marker, got)
		}
		if len(*calls) != 1 {
			t.Fatalf("marker %v: %d probes", marker, len(*calls))
		}
	}
}

func TestContextLocalProfileMemosDoNotCrossAndExpiredRowsArePruned(t *testing.T) {
	h := bedrockIsolatedEnv(t)
	calls := bedrockStubProbe(t, 0, 0, 96_000)
	ctx := context.Background()
	if got := bedrockResolve(ctx, grokBedrockModel, ""); got != 500_000 {
		t.Fatalf("resolved %d", got)
	}
	if got := bedrockResolve(ctx, "unknown.future", ""); got != BedrockDefaultContextLength {
		t.Fatalf("unknown resolved %d", got)
	}
	bedrockProbeFailureMu.Lock()
	for key := range bedrockProbeFailureCache {
		if strings.Contains(key.model, "unknown.future") {
			bedrockProbeFailureCache[key] = monotonicNow().Add(-bedrockProbeFailureTTL - time.Second)
		}
	}
	bedrockProbeFailureMu.Unlock()
	routed := filepath.Join(h, "routed-profile")
	if err := os.MkdirAll(routed, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := bedrockResolve(home.WithOverride(ctx, routed), grokBedrockModel, ""); got != 96_000 {
		t.Fatalf("routed profile resolved %d", got)
	}
	if len(*calls) != 3 {
		t.Fatalf("%d probes", len(*calls))
	}
	bedrockProbeFailureMu.Lock()
	for key := range bedrockProbeFailureCache {
		if strings.Contains(key.model, "unknown.future") {
			bedrockProbeFailureMu.Unlock()
			t.Fatal("the expired row was not pruned")
		}
	}
	bedrockProbeFailureMu.Unlock()
	if got := bedrockResolve(ctx, grokBedrockModel, ""); got != 500_000 || len(*calls) != 3 {
		t.Fatalf("launch profile resolved %d after %d probes", got, len(*calls))
	}
}
