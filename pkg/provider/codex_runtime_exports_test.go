package provider

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestCodexRuntimeExportsRecordUsageAndCompaction(t *testing.T) {
	db := &fakeCodexDB{}
	comp := &fakeCodexCompressor{awaiting: true}
	a := newFakeCodexAgent()
	a.sessionID, a.db, a.compressor, a.model, a.provider = "s1", db, comp, "gpt-5.5", "openai-codex"
	usage := pyjson.NewObject()
	for k, v := range map[string]any{"inputTokens": 1000, "cachedInputTokens": 400, "outputTokens": 50, "reasoningOutputTokens": 7, "totalTokens": 1050} {
		usage.Set(k, v)
	}
	got := RecordCodexAppServerUsage(a, &CodexTurnResult{TokenUsageLast: usage, ModelContextWindow: 272000}, []map[string]any{{"role": "user", "content": "hi"}})
	if got["prompt_tokens"] != 1000 || got["input_tokens"] != 600 || got["cache_read_tokens"] != 400 || a.apiCalls != 1 || comp.contextLength != 272000 || len(db.queued) != 1 {
		t.Fatalf("usage %v %+v %v", got, comp, db.queued)
	}
	if RecordCodexAppServerCompaction(a, &CodexTurnResult{}, 0, false) {
		t.Fatal("uncompacted turn recorded")
	}
	if !RecordCodexAppServerCompaction(a, &CodexTurnResult{Compacted: true, ThreadID: "th", TurnID: "tu"}, 0, false) || comp.count != 1 || comp.recorded != 1 {
		t.Fatalf("compaction %+v", comp)
	}
	if !RecordCodexAppServerCompaction(a, &CodexTurnResult{}, 900, true) || comp.rough != 900 {
		t.Fatalf("forced %+v", comp)
	}
}
