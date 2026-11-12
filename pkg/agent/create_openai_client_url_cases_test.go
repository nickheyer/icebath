package agent

import (
	"context"
	"testing"
)

// Captures the kwargs the OpenAI constructor receives for the rest of the test
func captureOpenAIKwargs(t *testing.T) *map[string]any {
	t.Helper()
	captured := map[string]any{}
	prev := openAIClientConstructor
	openAIClientConstructor = func(kwargs map[string]any) (any, error) {
		for k, v := range kwargs {
			captured[k] = v
		}
		return &struct{}{}, nil
	}
	t.Cleanup(func() { openAIClientConstructor = prev })
	return &captured
}

func TestCreateOpenaiClientNormalizesKimiCodingBaseURL(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	captured := captureOpenAIKwargs(t)
	a := &AIAgent{provider: "kimi-coding"}
	client, err := a.createOpenAIClient(context.Background(), map[string]any{"api_key": "sk-kimi-test12345", "base_url": "https://api.kimi.com/coding"}, "test", false)
	if err != nil || client == nil || (*captured)["base_url"] != "https://api.kimi.com/coding/v1" {
		t.Fatalf("client %v err %v kwargs %v", client, err, *captured)
	}
}

func TestCreateOpenaiClientPreservesAlreadySuffixedKimiURL(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	captured := captureOpenAIKwargs(t)
	a := &AIAgent{provider: "kimi-coding"}
	if _, err := a.createOpenAIClient(context.Background(), map[string]any{"api_key": "sk-kimi-test12345", "base_url": "https://api.kimi.com/coding/v1"}, "test", false); err != nil {
		t.Fatal(err)
	}
	if (*captured)["base_url"] != "https://api.kimi.com/coding/v1" {
		t.Fatalf("kwargs %v", *captured)
	}
}

func TestPrimaryClientIgnoresStaleAuxiliaryRouter(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	captured := captureOpenAIKwargs(t)
	a := &AIAgent{provider: "openai-codex"}
	if _, err := a.createOpenAIClient(context.Background(), map[string]any{"api_key": "token", "base_url": "https://chatgpt.com/backend-api/codex"}, "test", false); err != nil {
		t.Fatal(err)
	}
	headers := stringHeaders((*captured)["default_headers"])
	if headers["originator"] != "icebath-agent" {
		t.Fatalf("headers %v", (*captured)["default_headers"])
	}
}

func TestTrueIsAllApiModes(t *testing.T) {
	for _, am := range []string{"chat_completions", "anthropic", "codex_responses"} {
		if got := intentAckContinuationMode(&AIAgent{intentAckContinuation: true, apiMode: am}); got != "all" {
			t.Fatalf("%s -> %s", am, got)
		}
	}
	for _, s := range []string{"true", "always", "yes", "on", "ON"} {
		if got := intentAckContinuationMode(&AIAgent{intentAckContinuation: s, apiMode: "chat_completions"}); got != "all" {
			t.Fatalf("%s -> %s", s, got)
		}
	}
}

func TestMissingAttrDefaultsToAuto(t *testing.T) {
	if got := intentAckContinuationMode(&AIAgent{apiMode: "chat_completions", model: "x"}); got != "off" {
		t.Fatalf("chat -> %s", got)
	}
	if got := intentAckContinuationMode(&AIAgent{apiMode: "codex_responses", model: "x"}); got != "codex_only" {
		t.Fatalf("codex -> %s", got)
	}
}
