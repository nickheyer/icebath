package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

type namedWebProvider struct {
	tools.WebProvider
	name string
}

func (p namedWebProvider) Name() string { return p.name }

// Swaps the registry resolution and the legacy probe for the duration of the test
func swapWebSearchResolution(t *testing.T, provider tools.WebProvider, legacy string) {
	t.Helper()
	prevP, prevL := webSearchActiveProvider, webSearchLegacyBackend
	webSearchActiveProvider = func(*tools.Env) tools.WebProvider { return provider }
	webSearchLegacyBackend = func(*tools.Env) string { return legacy }
	t.Cleanup(func() { webSearchActiveProvider, webSearchLegacyBackend = prevP, prevL })
}

func xaiPrefersNativeFromConfig(t *testing.T) bool {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	return xaiPrefersNativeWebSearch(codexParams{webSearchBackend: func() (string, error) { return activeWebSearchBackend(ctx) }})
}

func TestXAIPrefersNativeWebSearchExplicitFirecrawlPrefersClient(t *testing.T) {
	swapWebSearchResolution(t, namedWebProvider{name: "firecrawl"}, "xai")
	if xaiPrefersNativeFromConfig(t) {
		t.Fatal("firecrawl provider keeps the client web_search")
	}
}

func TestXAIPrefersNativeWebSearchExplicitSearchBackendXAIPrefersNative(t *testing.T) {
	swapWebSearchResolution(t, namedWebProvider{name: "xai"}, "firecrawl")
	if !xaiPrefersNativeFromConfig(t) {
		t.Fatal("xai provider selects the native web_search")
	}
}

func TestXAIPrefersNativeWebSearchNoProviderLegacyFallbackXAI(t *testing.T) {
	swapWebSearchResolution(t, nil, "xai")
	if !xaiPrefersNativeFromConfig(t) {
		t.Fatal("legacy xai backend selects the native web_search")
	}
}

func TestXAIPrefersNativeWebSearchNoProviderLegacyFallbackNonXAI(t *testing.T) {
	swapWebSearchResolution(t, nil, "firecrawl")
	if xaiPrefersNativeFromConfig(t) {
		t.Fatal("legacy non xai backend keeps the client web_search")
	}
}

func TestActiveWebSearchBackendReadsTheHomeConfigWithoutAToolEnv(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	var seen *tools.Env
	prevP, prevL := webSearchActiveProvider, webSearchLegacyBackend
	webSearchActiveProvider = func(env *tools.Env) tools.WebProvider { seen = env; return nil }
	webSearchLegacyBackend = tools.GetSearchBackend
	t.Cleanup(func() { webSearchActiveProvider, webSearchLegacyBackend = prevP, prevL })
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("web:\n  search_backend: tavily\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := activeWebSearchBackend(context.Background())
	if err != nil || got != "tavily" || seen == nil || seen.Home != h {
		t.Fatalf("got %q %v env %+v", got, err, seen)
	}
}
