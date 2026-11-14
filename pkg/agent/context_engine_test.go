package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

type stubEngine struct {
	ContextEngineBase
	compressCalled bool
}

func newStubEngine(contextLength int, thresholdPct float64) *stubEngine {
	e := &stubEngine{ContextEngineBase: NewContextEngineBase()}
	e.ContextLength = contextLength
	e.ThresholdTokens = int(float64(contextLength) * thresholdPct)
	return e
}

func (e *stubEngine) Name() string { return "stub" }

func (e *stubEngine) UpdateFromResponse(usage map[string]any) {
	e.LastPromptTokens, _ = usage["prompt_tokens"].(int)
	e.LastCompletionTokens, _ = usage["completion_tokens"].(int)
	e.LastTotalTokens, _ = usage["total_tokens"].(int)
}

func (e *stubEngine) ShouldCompress(promptTokens *int) bool {
	tokens := e.LastPromptTokens
	if promptTokens != nil {
		tokens = *promptTokens
	}
	return tokens >= e.ThresholdTokens
}

func (e *stubEngine) Compress(messages []map[string]any, currentTokens *int, focusTopic *string, force bool, memoryContext string) ([]map[string]any, error) {
	e.compressCalled = true
	e.CompressionCount++
	return messages, nil
}

func (e *stubEngine) CloneForAgent() ContextEngine {
	c := *e
	c.ContextEngineBase = e.CloneBase()
	return &c
}

func TestDefaultGetStatus(t *testing.T) {
	e := newStubEngine(200000, 0.50)
	e.LastPromptTokens = 50000
	status := e.GetStatus()
	if status["last_prompt_tokens"] != 50000 || status["context_length"] != 200000 || status["threshold_tokens"] != 100000 {
		t.Fatalf("status %v", status)
	}
	if pct, ok := status["usage_percent"].(float64); !ok || pct <= 0 || pct > 100 {
		t.Fatalf("usage %v", status["usage_percent"])
	}
	e.LastPromptTokens = 400000
	if e.GetStatus()["usage_percent"] != 100 {
		t.Fatalf("capped usage %v", e.GetStatus()["usage_percent"])
	}
}

func TestOnSessionReset(t *testing.T) {
	e := newStubEngine(200000, 0.50)
	e.LastPromptTokens, e.CompressionCount = 999, 3
	e.OnSessionReset()
	if e.LastPromptTokens != 0 || e.CompressionCount != 0 {
		t.Fatalf("reset %+v", e.ContextEngineBase)
	}
}

func TestContextEngineBaseDefaults(t *testing.T) {
	var e ContextEngine = newStubEngine(1000, 0.5)
	if got := e.HandleToolCall("nope", nil, nil); got != `{"error": "Unknown context engine tool: nope"}` {
		t.Fatalf("tool call %s", got)
	}
	tokens := 600
	if ok, reason := ShouldCompressInfo(e, &tokens); !ok || reason != nil {
		t.Fatalf("compress info %v %v", ok, reason)
	}
	if msg := AutomaticCompactionStatusMessage(e, "start", "  Compacting context  ", nil); msg == nil || *msg != "Compacting context" {
		t.Fatalf("status message %v", msg)
	}
	if msg := AutomaticCompactionStatusMessage(e, "start", "   ", nil); msg != nil {
		t.Fatalf("blank status message %q", *msg)
	}
	e.Base().EmitAutomaticCompactionStatus = false
	if msg := AutomaticCompactionStatusMessage(e, "start", "Compacting", nil); msg != nil {
		t.Fatalf("suppressed status message %q", *msg)
	}
	if !e.HasContentToCompress(nil) || e.ShouldCompressPreflight(nil) || e.SelectContext(nil, nil, nil, 0) != nil {
		t.Fatal("base hook defaults")
	}
}

func TestSanitizeMemoryContextStripsAndKeepsShortText(t *testing.T) {
	if got := SanitizeMemoryContext(nil, "  remembered fact  \n"); got != "remembered fact" {
		t.Fatalf("sanitized %q", got)
	}
	long := strings.Repeat("a", MemoryContextMaxChars+500)
	if got := SanitizeMemoryContext(nil, long); utf8.RuneCountInString(got) >= utf8.RuneCountInString(long) || !strings.HasPrefix(got, strings.Repeat("a", 4000)) {
		t.Fatalf("elided length %d", utf8.RuneCountInString(got))
	}
}
