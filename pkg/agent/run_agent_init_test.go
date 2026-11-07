package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent built as the test_run_agent agent fixture builds it, quiet with context files and memory skipped
func newFixtureAgent(t *testing.T, o AIAgentOptions) *AIAgent {
	t.Helper()
	return newFixtureAgentWithConfig(t, "", o)
}

// Fixture agent over a temp home whose config.yaml holds configYAML
func newFixtureAgentWithConfig(t *testing.T, configYAML string, o AIAgentOptions) *AIAgent {
	t.Helper()
	env := promptTestEnv(t)
	if configYAML != "" {
		if err := os.WriteFile(filepath.Join(env.Home, "config.yaml"), []byte(configYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o.QuietMode, o.SkipContextFiles, o.SkipMemory = true, true, true
	if o.APIKey == "" {
		o.APIKey = "test-key-1234567890"
	}
	if o.Env == nil {
		o.Env = env
	}
	a, err := NewAIAgent(tools.WithToolEnv(context.Background(), o.Env), o)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAIAgentStripsMatchingNativeProviderPrefix(t *testing.T) {
	a := newFixtureAgent(t, AIAgentOptions{Model: "zai/glm-5.1", Provider: "zai", BaseURL: "https://api.z.ai/api/paas/v4"})
	if a.model != "glm-5.1" {
		t.Fatalf("model %q", a.model)
	}
}

func TestAnthropicBaseURLAccepted(t *testing.T) {
	calls := 0
	prev := anthropicClientBuilder
	anthropicClientBuilder = func(ctx context.Context, token any, baseURL string, timeout float64, drop1M bool) (any, error) {
		calls++
		return prev(ctx, token, baseURL, timeout, drop1M)
	}
	t.Cleanup(func() { anthropicClientBuilder = prev })
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://api.anthropic.com/v1/"})
	if a.apiMode != "anthropic_messages" || a.anthropicClient == nil || calls != 1 {
		t.Fatalf("api mode %q client %v builds %d", a.apiMode, a.anthropicClient, calls)
	}
}

func TestPromptCachingClaudeOpenRouter(t *testing.T) {
	a := newFixtureAgent(t, AIAgentOptions{Model: "anthropic/claude-sonnet-4-20250514", BaseURL: "https://openrouter.ai/api/v1"})
	if !a.usePromptCaching {
		t.Fatal("prompt caching off for claude on openrouter")
	}
}

func TestPromptCachingNonClaude(t *testing.T) {
	a := newFixtureAgent(t, AIAgentOptions{Model: "openai/gpt-4o", BaseURL: "https://openrouter.ai/api/v1"})
	if a.usePromptCaching {
		t.Fatal("prompt caching on for a non claude model")
	}
}

func TestPromptCachingNativeAnthropic(t *testing.T) {
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://api.anthropic.com/v1/"})
	if a.apiMode != "anthropic_messages" || !a.usePromptCaching {
		t.Fatalf("api mode %q caching %v", a.apiMode, a.usePromptCaching)
	}
}

func TestPromptCachingCacheTTLDefaultsWithoutConfig(t *testing.T) {
	a := newFixtureAgent(t, AIAgentOptions{Model: "anthropic/claude-sonnet-4-20250514", BaseURL: "https://openrouter.ai/api/v1"})
	if a.cacheTTL == "" || !a.usePromptCaching {
		t.Fatalf("ttl %q caching %v", a.cacheTTL, a.usePromptCaching)
	}
}

func TestPromptCachingDisabledByFalsyCacheTTL(t *testing.T) {
	for _, falsy := range []string{"false", "null", "off", "\"false\"", "disabled", "no", "none"} {
		t.Run(falsy, func(t *testing.T) {
			a := newFixtureAgentWithConfig(t, "prompt_caching:\n  cache_ttl: "+falsy+"\n", AIAgentOptions{Model: "anthropic/claude-sonnet-4-20250514", BaseURL: "https://openrouter.ai/api/v1"})
			if a.usePromptCaching || a.useNativeCacheLayout || a.cacheTTL != "" {
				t.Fatalf("caching %v native %v ttl %q", a.usePromptCaching, a.useNativeCacheLayout, a.cacheTTL)
			}
		})
	}
}

func TestPromptCachingDisableSurvivesPolicyRederivation(t *testing.T) {
	a := newFixtureAgentWithConfig(t, "prompt_caching:\n  cache_ttl: false\n", AIAgentOptions{Model: "anthropic/claude-sonnet-4-20250514", BaseURL: "https://openrouter.ai/api/v1"})
	if a.cacheTTL != "" {
		t.Fatalf("ttl %q", a.cacheTTL)
	}
	shouldCache, useNative := a.anthropicPromptCachePolicy(tools.WithToolEnv(context.Background(), a.env))
	if shouldCache || useNative || a.usePromptCaching {
		t.Fatalf("policy %v %v caching %v", shouldCache, useNative, a.usePromptCaching)
	}
}

func TestConstructorMaxTokensWinsOverConfig(t *testing.T) {
	a := newFixtureAgentWithConfig(t, "model:\n  max_tokens: 4096\n", AIAgentOptions{Provider: "custom", Model: "claude-opus-4-6-thinking", BaseURL: "http://proxy.example/v1", MaxTokens: 8192})
	if a.maxTokens != 8192 {
		t.Fatalf("max tokens %d", a.maxTokens)
	}
}

// Hint reads take the read timeout of the session profile home, not of the process home
func TestSubdirectoryHintsFollowTheSessionProfileHome(t *testing.T) {
	env := promptTestEnv(t)
	if err := os.WriteFile(filepath.Join(env.Home, "config.yaml"), []byte("context_file_read_timeout: 0.05\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	work := t.TempDir()
	sub := filepath.Join(work, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("Sub rules."), 0o600); err != nil {
		t.Fatal(err)
	}
	original := tools.ReadContextFileText
	tools.ReadContextFileText = func(path string) (string, error) {
		if filepath.Base(path) == "AGENTS.md" {
			time.Sleep(600 * time.Millisecond)
		}
		return original(path)
	}
	t.Cleanup(func() { tools.ReadContextFileText = original })
	a, err := NewAIAgent(tools.WithToolEnv(context.Background(), env), AIAgentOptions{APIKey: "test-key-1234567890", BaseURL: "https://openrouter.ai/api/v1", QuietMode: true, SkipMemory: true, Cwd: work, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	hint, ok := a.subdirectoryHints.(*tools.SubdirectoryHintTracker).CheckToolCall("read_file", map[string]any{"path": filepath.Join(sub, "x.py")})
	if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
		t.Fatalf("hint read blocked for %.2fs", elapsed.Seconds())
	}
	if ok || hint != "" {
		t.Fatalf("hint %q %v", hint, ok)
	}
}
