package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"gopkg.in/yaml.v3"
)

const bccGrok = "global.xai.grok-4.6"

var bccBaseURLs = []string{"", "https://bedrock-runtime.us-east-1.amazonaws.com"}

// Isolated home with a scripted probe, 0 standing for a failed probe, and the probe calls returned
func bccHome(t *testing.T, answers ...int) (string, *[]string) {
	t.Helper()
	bedrockIsolatedEnv(t)
	resetMetadataCaches()
	t.Cleanup(resetMetadataCaches)
	return os.Getenv("ICEBATH_HOME"), bedrockStubProbe(t, answers...)
}

// Probe seam failing the test, standing in for a restarted process that must not reprobe
func bccForbidProbe(t *testing.T, why string) {
	prev := bedrockProbeFn
	bedrockProbeFn = func(context.Context, string, string) int { t.Errorf("probe called: %s", why); return 0 }
	t.Cleanup(func() { bedrockProbeFn = prev })
}

func bccWriteYAML(t *testing.T, path string, doc any) {
	t.Helper()
	data, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func bccReadCache(t *testing.T, h string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h, "context_length_cache.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func bccResolve(ctx context.Context, model, provider, baseURL string) int {
	return GetModelContextLength(ctx, ContextLengthRequest{Model: model, Provider: provider, BaseURL: baseURL})
}

func bccCacheURL(baseURL string) string {
	if baseURL == "" {
		return "bedrock://"
	}
	return baseURL
}

func TestBedrockResolutionMigratesAmbiguousCacheAndPreservesProbe(t *testing.T) {
	for _, baseURL := range bccBaseURLs {
		for _, legacy := range []int{0, 128_000, 700_000} {
			for _, probed := range []int{0, 128_000, 800_000} {
				t.Run(fmt.Sprintf("%q/legacy=%d/probed=%d", baseURL, legacy, probed), func(t *testing.T) {
					h, calls := bccHome(t, probed)
					ctx := context.Background()
					key := contextCacheKey(bccGrok, bccCacheURL(baseURL))
					cacheFile := filepath.Join(h, "context_length_cache.yaml")
					if legacy != 0 {
						bccWriteYAML(t, cacheFile, map[string]any{"context_lengths": map[string]any{key: legacy}})
					}
					expected := 500_000
					if probed != 0 {
						expected = probed
					}
					for range 2 {
						if got := bccResolve(ctx, bccGrok, "bedrock", baseURL); got != expected {
							t.Fatalf("context %d want %d", got, expected)
						}
					}
					if len(*calls) != 1 || (*calls)[0] != bccGrok+"|us-east-1" {
						t.Fatalf("probe calls %v", *calls)
					}
					got, ok := GetCachedContextLength(ctx, bccGrok, bccCacheURL(baseURL), true)
					if (probed == 0) == ok || (ok && got != probed) {
						t.Fatalf("confirmed cache %d %v want %d", got, ok, probed)
					}
					SaveContextLength(ctx, "other-model", "https://other.example/v1", 64_000, "")
					invalidateCachedContextLength(ctx, "other-model", "https://other.example/v1")
					if probed == 0 {
						lengths, _ := bccReadCache(t, h)["context_lengths"].(map[string]any)
						if v, has := lengths[key]; (legacy == 0) == has || (has && v != legacy) {
							t.Fatalf("legacy row %v %v want %d", v, has, legacy)
						}
						return
					}
					resetMetadataCaches()
					bccForbidProbe(t, "reprobed persisted success")
					if got := bccResolve(ctx, bccGrok, "bedrock", baseURL); got != expected {
						t.Fatalf("after restart %d want %d", got, expected)
					}
				})
			}
		}
	}
}

func TestBedrockFailureMemoRetryScopeAndExpiry(t *testing.T) {
	for _, base := range bccBaseURLs {
		for _, retry := range []string{"ttl", "invalidate", "restart", "profile", "endpoint"} {
			t.Run(fmt.Sprintf("%q/%s", base, retry), func(t *testing.T) {
				baseURL := base
				h, calls := bccHome(t, 0, 96_000)
				ctx := context.Background()
				for range 2 {
					if got := bccResolve(ctx, bccGrok, "bedrock", baseURL); got != 500_000 {
						t.Fatalf("fallback %d", got)
					}
				}
				if len(*calls) != 1 {
					t.Fatalf("probe calls %v", *calls)
				}
				if _, err := os.Stat(filepath.Join(h, "context_length_cache.yaml")); !os.IsNotExist(err) {
					t.Fatalf("failed probe wrote the cache: %v", err)
				}
				switch retry {
				case "ttl":
					bedrockProbeFailureMu.Lock()
					for k := range bedrockProbeFailureCache {
						bedrockProbeFailureCache[k] = monotonicNow().Add(-bedrockProbeFailureTTL - 1e9)
					}
					bedrockProbeFailureMu.Unlock()
				case "invalidate":
					invalidateCachedContextLength(ctx, bccGrok, bccCacheURL(baseURL))
				case "restart":
					bedrockProbeFailureMu.Lock()
					clear(bedrockProbeFailureCache)
					bedrockProbeFailureMu.Unlock()
				case "profile":
					other := filepath.Join(h, "other-profile")
					if err := os.MkdirAll(other, 0o755); err != nil {
						t.Fatal(err)
					}
					t.Setenv("ICEBATH_HOME", other)
				default:
					baseURL = "https://bedrock-runtime.us-east-1.amazonaws.com/other"
				}
				if got := bccResolve(ctx, bccGrok, "bedrock", baseURL); got != 96_000 || len(*calls) != 2 {
					t.Fatalf("retry %d calls %v", got, *calls)
				}
				if retry == "ttl" {
					bedrockProbeFailureMu.Lock()
					left := len(bedrockProbeFailureCache)
					bedrockProbeFailureMu.Unlock()
					if left != 0 {
						t.Fatalf("expired memo rows kept %d", left)
					}
				}
			})
		}
	}
}

func TestBedrockExplicitContextOverrideWins(t *testing.T) {
	model := "us.xai.grok-4.6"
	for _, baseURL := range bccBaseURLs {
		for _, override := range []string{"argument", "config"} {
			t.Run(fmt.Sprintf("%q/%s", baseURL, override), func(t *testing.T) {
				h, _ := bccHome(t, 0)
				bccForbidProbe(t, "explicit override must not probe")
				req := ContextLengthRequest{Model: model, Provider: "bedrock", BaseURL: baseURL}
				if override == "argument" {
					req.ConfigContextLength = 80_000
				} else {
					bccWriteYAML(t, filepath.Join(h, "config.yaml"), map[string]any{"model_overrides": map[string]any{"bedrock": map[string]any{model: map[string]any{"context_window": 80_000}}}})
				}
				if got := GetModelContextLength(context.Background(), req); got != 80_000 {
					t.Fatalf("override lost %d", got)
				}
				if _, err := os.Stat(filepath.Join(h, "context_length_cache.yaml")); !os.IsNotExist(err) {
					t.Fatalf("override wrote the cache: %v", err)
				}
			})
		}
	}
}

func TestBedrockUnknownModelFallbackAndHostInference(t *testing.T) {
	bccHome(t, 0)
	ctx := context.Background()
	base := "https://bedrock-runtime.us-east-1.amazonaws.com"
	if got := bccResolve(ctx, "unknown.future-model", "", base); got != BedrockDefaultContextLength {
		t.Fatalf("unknown %d", got)
	}
	if got := bccResolve(ctx, "xai.grok-4.6", "", base); got != 500_000 {
		t.Fatalf("grok %d", got)
	}
	if n, ok := GetCachedContextLength(ctx, "unknown.future-model", base, false); ok {
		t.Fatalf("fallback cached %d", n)
	}
}

func TestBedrockProviderConfirmedWritersSurviveRestart(t *testing.T) {
	for _, baseURL := range bccBaseURLs {
		t.Run(fmt.Sprintf("%q", baseURL), func(t *testing.T) {
			bccHome(t, 0)
			ctx := context.Background()
			SaveProviderContextLength(ctx, bccGrok, baseURL, 96_000, "bedrock")
			if n, ok := GetCachedContextLength(ctx, bccGrok, bccCacheURL(baseURL), false); !ok || n != 96_000 {
				t.Fatalf("provider limit not cached %d %v", n, ok)
			}
			resetMetadataCaches()
			bccForbidProbe(t, "lost provider limit")
			if got := bccResolve(ctx, bccGrok, "bedrock", baseURL); got != 96_000 {
				t.Fatalf("after restart %d", got)
			}
		})
	}
}

func TestBedrockReadonlyLegacyCacheDoesNotResetProbeCooldown(t *testing.T) {
	for _, baseURL := range bccBaseURLs {
		t.Run(fmt.Sprintf("%q", baseURL), func(t *testing.T) {
			h, calls := bccHome(t, 0)
			cacheFile := filepath.Join(h, "context_length_cache.yaml")
			bccWriteYAML(t, cacheFile, map[string]any{"context_lengths": map[string]any{contextCacheKey(bccGrok, bccCacheURL(baseURL)): 128_000}})
			if err := os.Chmod(cacheFile, 0o444); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(h, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(h, 0o755); _ = os.Chmod(cacheFile, 0o644) })
			if f, err := os.OpenFile(filepath.Join(h, "probe-write"), os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				f.Close()
				t.Fatal("home stayed writable, the cache write cannot fail")
			}
			for range 3 {
				if got := bccResolve(context.Background(), bccGrok, "bedrock", baseURL); got != 500_000 {
					t.Fatalf("context %d", got)
				}
			}
			if len(*calls) != 1 {
				t.Fatalf("probe calls %v", *calls)
			}
		})
	}
}

func TestBedrockProvenanceIsBackwardReadableAndGenericWritesClearIt(t *testing.T) {
	model, base := "xai.grok-4.6", "https://bedrock-runtime.us-east-1.amazonaws.com"
	for _, rewrite := range []int{0, 96_000, 120_000} {
		t.Run(fmt.Sprint(rewrite), func(t *testing.T) {
			h, _ := bccHome(t, 96_000, 110_000)
			ctx := context.Background()
			if got := bccResolve(ctx, model, "", base); got != 96_000 {
				t.Fatalf("probed %d", got)
			}
			lengths := bccReadCache(t, h)["context_lengths"].(map[string]any)
			if v, ok := lengths[contextCacheKey(model, base)].(int); !ok || v+1 != 96_001 {
				t.Fatalf("legacy readers get %#v", lengths[contextCacheKey(model, base)])
			}
			want := 96_000
			if rewrite != 0 {
				SaveContextLength(ctx, model, base, rewrite, "")
				want = 110_000
			}
			if got := bccResolve(ctx, model, "", base); got != want {
				t.Fatalf("after rewrite %d got %d want %d", rewrite, got, want)
			}
		})
	}
}

func TestBedrockMalformedLengthsDoNotBlockProviderPersistence(t *testing.T) {
	for i, lengths := range []any{true, []any{128_000}, "bad"} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			h, _ := bccHome(t, 96_000)
			bccWriteYAML(t, filepath.Join(h, "context_length_cache.yaml"), map[string]any{"context_lengths": lengths})
			ctx := context.Background()
			if got := bccResolve(ctx, "xai.grok-4.6", "bedrock", ""); got != 96_000 {
				t.Fatalf("context %d", got)
			}
			if n, ok := GetCachedContextLength(ctx, "xai.grok-4.6", "bedrock://", false); !ok || n != 96_000 {
				t.Fatalf("cached %d %v", n, ok)
			}
		})
	}
}

func TestBedrockMalformedOrMismatchedProvenanceRequiresRevalidation(t *testing.T) {
	model := "xai.grok-4.6"
	for i, marker := range []any{true, "96000", 128_000, []any{96_000}, map[string]any{"source": "probe"}} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			h, calls := bccHome(t, 100_000)
			key := contextCacheKey(model, "bedrock://")
			bccWriteYAML(t, filepath.Join(h, "context_length_cache.yaml"), map[string]any{
				"context_lengths": map[string]any{key: 96_000}, "bedrock_confirmed_v1": map[string]any{key: marker}})
			if got := bccResolve(context.Background(), model, "bedrock", ""); got != 100_000 || len(*calls) != 1 {
				t.Fatalf("context %d calls %v", got, *calls)
			}
		})
	}
}

func TestBedrockContextLocalProfileMemosDoNotCrossAndExpiredRowsArePruned(t *testing.T) {
	h, calls := bccHome(t, 0, 0, 96_000)
	ctx := context.Background()
	if got := bccResolve(ctx, bccGrok, "bedrock", ""); got != 500_000 {
		t.Fatalf("fallback %d", got)
	}
	if got := bccResolve(ctx, "unknown.future", "bedrock", ""); got != BedrockDefaultContextLength {
		t.Fatalf("unknown %d", got)
	}
	bedrockProbeFailureMu.Lock()
	for k := range bedrockProbeFailureCache {
		if k.model == "unknown.future" {
			bedrockProbeFailureCache[k] = monotonicNow().Add(-bedrockProbeFailureTTL - 1e9)
		}
	}
	bedrockProbeFailureMu.Unlock()
	routed := home.WithOverride(ctx, filepath.Join(h, "routed-profile"))
	if got := bccResolve(routed, bccGrok, "bedrock", ""); got != 96_000 {
		t.Fatalf("routed profile %d", got)
	}
	if len(*calls) != 3 {
		t.Fatalf("probe calls %v", *calls)
	}
	bedrockProbeFailureMu.Lock()
	for k := range bedrockProbeFailureCache {
		if k.model == "unknown.future" {
			t.Errorf("expired row kept %+v", k)
		}
	}
	bedrockProbeFailureMu.Unlock()
	if got := bccResolve(ctx, bccGrok, "bedrock", ""); got != 500_000 || len(*calls) != 3 {
		t.Fatalf("launch profile %d calls %v", got, *calls)
	}
}
