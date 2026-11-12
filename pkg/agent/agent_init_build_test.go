package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

type toolSchemaEngine struct {
	*stubEngine
	schemas []map[string]any
	started map[string]any
}

func (e *toolSchemaEngine) GetToolSchemas() []map[string]any { return e.schemas }

func (e *toolSchemaEngine) OnSessionStart(sessionID string, kwargs map[string]any) {
	e.started = kwargs
	e.started["session_id"] = sessionID
}

func TestMinimumContextRefusesSmallWindowsWithTheMatchingRemedy(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := routingAgent("openrouter", "https://openrouter.ai/api/v1")
	a.contextCompressor = newStubEngine(32000, 0.5)
	err := enforceMinimumContext(a)
	if err == nil || !strings.Contains(err.Error(), "has a context window of 32,000 tokens, which is below the minimum 64,000") || !strings.Contains(err.Error(), "Choose a model with at least 64K context") {
		t.Fatalf("remote refusal = %v", err)
	}
	local := routingAgent("custom", "http://localhost:11434/v1")
	local.contextCompressor = newStubEngine(8192, 0.5)
	if err := enforceMinimumContext(local); err == nil || !strings.Contains(err.Error(), "Your local server is serving a 8,192-token window") {
		t.Fatalf("local refusal = %v", err)
	}
	local.ollamaNumCtx = 131072
	if err := enforceMinimumContext(local); err != nil {
		t.Fatalf("ollama num_ctx above the floor refused: %v", err)
	}
	lm := routingAgent("lmstudio", "http://localhost:1234/v1")
	lm.contextCompressor = newStubEngine(16000, 0.5)
	lm.configContextLength = 16000
	if err := enforceMinimumContext(lm); err != nil {
		t.Fatalf("explicit LM Studio length refused: %v", err)
	}
}

func TestExplicitClientKwargsSplitTheQueryAndPickTheHostHeaders(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := home.WithOverride(context.Background(), t.TempDir())
	a := routingAgent("custom", "https://api.kimi.com/coding/v1?api-version=2024&empty=")
	timeout := 90.0
	kwargs, err := explicitClientKwargs(ctx, a, "sk-test", a.baseURL, &timeout)
	if err != nil {
		t.Fatal(err)
	}
	if kwargs["base_url"] != "https://api.kimi.com/coding/v1" || kwargs["timeout"] != 90.0 {
		t.Fatalf("kwargs = %v", kwargs)
	}
	query := kwargs["default_query"].(map[string]string)
	if len(query) != 1 || query["api-version"] != "2024" {
		t.Fatalf("default query = %v", query)
	}
	if h := kwargs["default_headers"].(map[string]string); h["User-Agent"] != "claude-code/0.1.0" {
		t.Fatalf("kimi headers = %v", h)
	}
	codex, err := explicitClientKwargs(ctx, routingAgent("openai-codex", "https://chatgpt.com/backend-api/codex"), "tok", "https://chatgpt.com/backend-api/codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := codex["default_headers"].(map[string]string); h["originator"] != "icebath-agent" {
		t.Fatalf("codex headers = %v", h)
	}
	if _, has := codex["timeout"]; has {
		t.Fatal("timeout set without a provider timeout")
	}
	if hostDefaultHeadersFactory("https://example.test/v1") != nil {
		t.Fatal("unlisted host matched a header factory")
	}
}

func TestDisplayConfigReadsStreamingLoadModeAndCommentary(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{}
	applyDisplayConfig(a, map[string]any{
		"display": map[string]any{"show_commentary": false},
		"agent":   map[string]any{"fast_auto_seconds": 15},
		"model":   map[string]any{"lmstudio_load_mode": "JIT", "streaming": "off"},
	}, "cli")
	if a.showCommentary || a.fastAutoSeconds != 15 || a.lmstudioLoadMode != "jit" || !a.RetrySession().DisableStreaming || a.toolGuardrails == nil {
		t.Fatalf("display state = commentary %v fast %v mode %q streaming off %v", a.showCommentary, a.fastAutoSeconds, a.lmstudioLoadMode, a.RetrySession().DisableStreaming)
	}
	b := &AIAgent{}
	applyDisplayConfig(b, map[string]any{"model": map[string]any{"lmstudio_load_mode": "lazy", "streaming": "maybe"}}, "telegram")
	if !b.showCommentary || b.fastAutoSeconds != 60 || b.lmstudioLoadMode != "explicit" || b.RetrySession().DisableStreaming {
		t.Fatalf("defaults = commentary %v fast %v mode %q streaming off %v", b.showCommentary, b.fastAutoSeconds, b.lmstudioLoadMode, b.RetrySession().DisableStreaming)
	}
}

func TestContextEngineToolsJoinTheSurfaceOnceAndStartTheSession(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := home.WithOverride(context.Background(), t.TempDir())
	engine := &toolSchemaEngine{stubEngine: newStubEngine(200000, 0.5), schemas: []map[string]any{
		{"name": "lcm_grep", "description": "search", "parameters": map[string]any{"type": "object"}},
		{"name": "read_file"},
		{"description": "no name"},
	}}
	a := &AIAgent{contextCompressor: engine, sessionID: "s1", model: "m", gatewaySessionKey: "gk",
		tools:          []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}},
		validToolNames: map[string]bool{"read_file": true}}
	injectContextEngineTools(ctx, a)
	if len(a.tools) != 2 || !a.validToolNames["lcm_grep"] || !a.contextEngineToolNames["lcm_grep"] || a.contextEngineToolNames["read_file"] {
		t.Fatalf("tools = %v engine names = %v", a.tools, a.contextEngineToolNames)
	}
	if engine.started["session_id"] != "s1" || engine.started["platform"] != "cli" || engine.started["conversation_id"] != "gk" || engine.started["context_length"] != 200000 {
		t.Fatalf("session start kwargs = %v", engine.started)
	}
	gated := &AIAgent{contextCompressor: engine, enabledToolsets: []string{"web"}, tools: []any{}}
	injectContextEngineTools(ctx, gated)
	if len(gated.tools) != 0 {
		t.Fatalf("toolset gate ignored: %v", gated.tools)
	}
}

func TestCompressorMaxTokensReservesTheGeminiDefault(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := compressorMaxTokens(&AIAgent{provider: "google-gemini"}); got != provider.GeminiDefaultMaxOutputTokens {
		t.Fatalf("gemini reservation = %v", got)
	}
	if got := compressorMaxTokens(&AIAgent{provider: "openrouter", maxTokens: 4096}); got != 4096 {
		t.Fatalf("explicit max tokens = %v", got)
	}
	if got := compressorMaxTokens(&AIAgent{provider: "openrouter", baseURL: "https://openrouter.ai/api/v1"}); got != nil {
		t.Fatalf("non gemini reservation = %v", got)
	}
}
