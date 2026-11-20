package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMoAContextLengthMoaCustomContextConfiguresCompressorThreshold(t *testing.T) {
	const configuredContext = 600_000
	var probes atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(endpoint.Close)
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	payload := map[string]any{
		"moa": map[string]any{"default_preset": "p", "presets": map[string]any{"p": map[string]any{
			"enabled": true, "reference_models": []any{map[string]any{"provider": "openrouter", "model": "openai/gpt-5.5"}},
			"aggregator": map[string]any{"provider": "custom:example", "model": "example-model"}}}},
		"providers": map[string]any{"example": map[string]any{
			"api": endpoint.URL + "/v1", "default_model": "example-model",
			"models": map[string]any{"example-model": map[string]any{"context_length": configuredContext}}}},
	}
	raw, err := yaml.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	o := DefaultContextCompressorOptions("p")
	o.BaseURL, o.Provider, o.ThresholdPercent, o.QuietMode = "http://127.0.0.1/v1", "moa", 0.50, true
	c := NewContextCompressor(context.Background(), o)
	if got := c.ContextLength(); got != configuredContext {
		t.Fatalf("context %d, want %d", got, configuredContext)
	}
	if got := c.ThresholdTokens(); got != configuredContext/2 {
		t.Fatalf("threshold %d, want %d", got, configuredContext/2)
	}
	if n := probes.Load(); n != 0 {
		t.Fatalf("the configured window must answer without probing the endpoint, %d requests", n)
	}
}
