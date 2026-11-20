package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/provider"
)

var bedrockRestartEnvVars = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN",
	"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_BEARER_TOKEN_BEDROCK", "AWS_ENDPOINT_URL",
	"AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "AWS_ENDPOINT_URL_BEDROCK", "AWS_IGNORE_CONFIGURED_ENDPOINT_URLS",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN"}

// Fresh home with no AWS environment, the isolated_home fixture
func bedrockRestartHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, k := range bedrockRestartEnvVars {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	h := filepath.Join(dir, "icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("HOME", dir)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "aws-config"))
	return h
}

// Cached window of key in the on disk cache document, zero when absent
func bedrockRestartCachedRow(t *testing.T, cacheFile, key string) int {
	t.Helper()
	raw, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ContextLengths map[string]int `yaml:"context_lengths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.ContextLengths[key]
}

func TestBedrockResolutionMigratesAmbiguousCacheAndPreservesProbe(t *testing.T) {
	const model = "global.xai.grok-4.6"
	for _, baseURL := range []string{"", "https://bedrock-runtime.us-east-1.amazonaws.com"} {
		for _, legacy := range []int{0, 128_000, 700_000} {
			for _, probed := range []int{0, 128_000, 800_000} {
				label := fmt.Sprintf("%q legacy=%d probed=%d", baseURL, legacy, probed)
				h := bedrockRestartHome(t)
				cacheURL := baseURL
				if cacheURL == "" {
					cacheURL = "bedrock://"
				}
				key := provider.ContextCacheKey(model, cacheURL)
				cacheFile := filepath.Join(h, "context_length_cache.yaml")
				if legacy != 0 {
					raw, err := yaml.Marshal(map[string]any{"context_lengths": map[string]any{key: legacy}})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(cacheFile, raw, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				calls := []string{}
				prevProbe := provider.SwapBedrockProbe(func(_ context.Context, m, region string) int {
					calls = append(calls, m+"|"+region)
					return probed
				})
				prevRegion := provider.SwapResolveBedrockRegion(func() string { return "us-east-1" })
				provider.ResetBedrockProbeFailureCache()
				t.Cleanup(func() {
					provider.SwapBedrockProbe(prevProbe)
					provider.SwapResolveBedrockRegion(prevRegion)
					provider.ResetBedrockProbeFailureCache()
				})
				expected := probed
				if probed == 0 {
					expected = 500_000
				}
				ratio := 0.5
				if expected < 512_000 {
					ratio = 0.75
				}
				expectedThreshold := int(float64(expected) * ratio)
				ctx := context.Background()
				o := DefaultContextCompressorOptions(model)
				o.Provider, o.BaseURL, o.QuietMode = "bedrock", baseURL, true
				c := NewContextCompressor(ctx, o)
				if got := c.ContextLength(); got != expected {
					t.Fatalf("%s: compressor context %d, want %d", label, got, expected)
				}
				if got := c.ThresholdTokens(); got != expectedThreshold {
					t.Fatalf("%s: threshold %d, want %d", label, got, expectedThreshold)
				}
				if got := provider.GetModelContextLength(ctx, provider.ContextLengthRequest{Model: model, Provider: "bedrock", BaseURL: baseURL}); got != expected {
					t.Fatalf("%s: resolved %d, want %d", label, got, expected)
				}
				if len(calls) != 1 || calls[0] != model+"|us-east-1" {
					t.Fatalf("%s: probe calls %v", label, calls)
				}
				confirmed, ok := provider.GetCachedContextLength(ctx, model, cacheURL, true)
				if probed == 0 && ok {
					t.Fatalf("%s: failed probe reads back as confirmed %d", label, confirmed)
				}
				if probed != 0 && (!ok || confirmed != probed) {
					t.Fatalf("%s: confirmed cache %d %v, want %d", label, confirmed, ok, probed)
				}
				provider.SaveContextLength(ctx, "other-model", "https://other.example/v1", 64_000, "")
				provider.InvalidateCachedContextLength(ctx, "other-model", "https://other.example/v1")
				if probed == 0 {
					if legacy == 0 {
						if _, err := os.Stat(cacheFile); err == nil {
							if row := bedrockRestartCachedRow(t, cacheFile, key); row != 0 {
								t.Fatalf("%s: failed probe wrote %d", label, row)
							}
						}
					} else if row := bedrockRestartCachedRow(t, cacheFile, key); row != legacy {
						t.Fatalf("%s: legacy row %d, want %d", label, row, legacy)
					}
					continue
				}
				provider.ResetBedrockProbeFailureCache()
				provider.SwapBedrockProbe(func(context.Context, string, string) int {
					t.Errorf("%s: reprobed persisted success", label)
					return 0
				})
				restarted := NewContextCompressor(ctx, o)
				if got := restarted.ContextLength(); got != expected {
					t.Fatalf("%s: restarted context %d, want %d", label, got, expected)
				}
				if got := restarted.ThresholdTokens(); got != expectedThreshold {
					t.Fatalf("%s: restarted threshold %d, want %d", label, got, expectedThreshold)
				}
			}
		}
	}
}
