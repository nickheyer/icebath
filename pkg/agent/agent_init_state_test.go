package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func neverMinted(context.Context) (string, error) { panic("the banner invoked the token source") }

func TestKeyBannerLabelsACallableKeyByItsSource(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cases := []struct {
		name  string
		key   any
		label string
	}{
		{"entra", provider.TagCredentialSource(neverMinted, provider.CredentialSourceEntraID), "Microsoft Entra ID"},
		{"vertex", provider.TagCredentialSource(neverMinted, provider.CredentialSourceVertexOAuth), "Google Vertex OAuth"},
		{"key_cmd", &provider.CommandTokenSource{Command: "print-token", Label: "corp"}, "key_cmd token"},
		{"minimax", provider.TagCredentialSource(neverMinted, provider.CredentialSourceMinimaxOAuth), "MiniMax OAuth"},
		{"unknown", func(context.Context) (string, error) { return neverMinted(nil) }, "token provider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStatusStdout(t)
			printKeyBanner(tc.key, "API key", false)
			if !strings.Contains(out.String(), tc.label) {
				t.Fatalf("banner %q lacks %q", out.String(), tc.label)
			}
			if tc.name != "entra" && strings.Contains(out.String(), "Microsoft Entra ID") {
				t.Fatalf("banner %q names entra", out.String())
			}
		})
	}
}

func TestStringKeysKeepTheMaskedForm(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := captureStatusStdout(t)
	printKeyBanner("sk-live-abcdefghijklmnop3456", "API key", false)
	if got := strings.TrimSpace(out.String()); got != "🔑 Using API key: sk-live-...3456" {
		t.Fatalf("banner = %q", got)
	}
	out.Reset()
	printKeyBanner("dummy-key", "API key", false)
	if out.Len() != 0 {
		t.Fatalf("short key printed %q", out.String())
	}
	printKeyBanner("dummy-key", "API key", true)
	if got := strings.TrimSpace(out.String()); got != "⚠️  Warning: API key appears invalid or missing" {
		t.Fatalf("warning = %q", got)
	}
}

func writePromptCachingConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRealAgentResolvesAutoFromItsPlatform(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	writePromptCachingConfig(t, "prompt_caching:\n  cache_ttl: auto\n")
	interactive := &AIAgent{provider: "anthropic", baseURL: "https://api.anthropic.com", model: "claude-fable-5.1", apiMode: "anthropic_messages", platform: "cli"}
	scheduled := &AIAgent{provider: "anthropic", baseURL: "https://api.anthropic.com", model: "claude-fable-5.1", apiMode: "anthropic_messages", platform: "cron"}
	initPromptCacheConfig(context.Background(), interactive)
	initPromptCacheConfig(context.Background(), scheduled)
	if interactive.cacheTTL != "1h" || scheduled.cacheTTL != "5m" {
		t.Fatalf("cli ttl %q, cron ttl %q", interactive.cacheTTL, scheduled.cacheTTL)
	}
	if !interactive.usePromptCaching || !interactive.useNativeCacheLayout {
		t.Fatalf("native anthropic route policy = %v %v", interactive.usePromptCaching, interactive.useNativeCacheLayout)
	}
}

func TestDisabledCacheTTLTurnsPromptCachingOff(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, body := range []string{"prompt_caching:\n  cache_ttl: off\n", "prompt_caching:\n  cache_ttl: false\n", "prompt_caching:\n  cache_ttl: null\n"} {
		writePromptCachingConfig(t, body)
		a := &AIAgent{provider: "anthropic", baseURL: "https://api.anthropic.com", model: "claude-fable-5.1", apiMode: "anthropic_messages"}
		initPromptCacheConfig(context.Background(), a)
		if a.usePromptCaching || a.useNativeCacheLayout || a.cacheTTL != "" || !a.cacheDisabled {
			t.Fatalf("%q left caching %v %v ttl %q disabled %v", body, a.usePromptCaching, a.useNativeCacheLayout, a.cacheTTL, a.cacheDisabled)
		}
	}
	writePromptCachingConfig(t, "prompt_caching:\n  cache_ttl: 1h\n")
	a := &AIAgent{provider: "anthropic", baseURL: "https://api.anthropic.com", model: "claude-fable-5.1", apiMode: "anthropic_messages"}
	initPromptCacheConfig(context.Background(), a)
	if a.cacheTTL != "1h" || a.cacheDisabled {
		t.Fatalf("ttl %q disabled %v", a.cacheTTL, a.cacheDisabled)
	}
}

func TestPromptCacheConfigFailureKeepsDefaults(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prev := promptCacheConfigLoader
	t.Cleanup(func() { promptCacheConfigLoader = prev })
	for _, load := range []func(context.Context) (map[string]any, error){
		func(context.Context) (map[string]any, error) { return nil, errors.New("boom") },
		func(context.Context) (map[string]any, error) { return map[string]any{"prompt_caching": "yes"}, nil },
		func(context.Context) (map[string]any, error) {
			return map[string]any{"prompt_caching": map[string]any{"cache_ttl": []any{"1h"}}}, nil
		},
	} {
		promptCacheConfigLoader = load
		a := &AIAgent{provider: "anthropic", baseURL: "https://api.anthropic.com", model: "claude-fable-5.1", apiMode: "anthropic_messages"}
		initPromptCacheConfig(context.Background(), a)
		if a.cacheTTL != "5m" || a.cacheDisabled || !a.usePromptCaching {
			t.Fatalf("ttl %q disabled %v caching %v", a.cacheTTL, a.cacheDisabled, a.usePromptCaching)
		}
	}
}

func TestInitStateTablesGiveEachAgentFreshState(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, b := &AIAgent{}, &AIAgent{}
	for _, x := range []*AIAgent{a, b} {
		setControlStateDefaults(x)
		initTurnState(x, 90)
		setStreamStateDefaults(x)
	}
	if a.toolGuardrails == nil || a.toolGuardrails == b.toolGuardrails || a.hardInterruptRequested == b.hardInterruptRequested ||
		a.clientLock == b.clientLock || a.streamContextScrubber == b.streamContextScrubber || a.streamThinkScrubber == nil {
		t.Fatal("factories were shared or missing")
	}
	if !a.supportsActiveTurnRedirect || a.toolWorkerThreads == nil || a.activeChildren == nil || a.deliveredInterimTexts == nil || a.anthropicImageFallbackCache == nil {
		t.Fatal("control or stream defaults missing")
	}
	if a.lastActivityDesc != "initializing" || a.lastActivityTS == 0 || a.runBudgetSeconds != 90 || a.CreditsCapture().Latch() == nil {
		t.Fatalf("turn state desc %q ts %v budget %v", a.lastActivityDesc, a.lastActivityTS, a.runBudgetSeconds)
	}
	initTurnState(a, nil)
	if a.runBudgetSeconds != 0 {
		t.Fatalf("a None budget kept %v", a.runBudgetSeconds)
	}
}

func TestSetupAgentLoggingEnablesVerboseOnlyWhenAsked(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prevSetup, prevVerbose := agentSetupLogging, agentSetupVerboseLogging
	t.Cleanup(func() { agentSetupLogging, agentSetupVerboseLogging = prevSetup, prevVerbose })
	var homes []string
	verbose := 0
	agentSetupLogging = func(_ context.Context, h string) error { homes = append(homes, h); return nil }
	agentSetupVerboseLogging = func() error { verbose++; return nil }
	if err := setupAgentLogging(context.Background(), &AIAgent{}); err != nil || verbose != 0 {
		t.Fatalf("quiet setup err %v verbose %d", err, verbose)
	}
	if err := setupAgentLogging(context.Background(), &AIAgent{verboseLogging: true}); err != nil || verbose != 1 {
		t.Fatalf("verbose setup err %v verbose %d", err, verbose)
	}
	if len(homes) != 2 || homes[0] != os.Getenv("ICEBATH_HOME") {
		t.Fatalf("homes = %v", homes)
	}
}
