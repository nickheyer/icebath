package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const compactionSecret = "TOPSECRET_TRANSCRIPT_TEXT"

func compactionCaseCompressor(t *testing.T, edit func(*ContextCompressorOptions)) *ContextCompressor {
	t.Helper()
	window := 100_000
	c := newCompressorWith(t, window, func(o *ContextCompressorOptions) {
		o.Model, o.Provider, o.ThresholdPercent, o.ConfigContextLength = "test/main-model", "test-provider", 0.50, &window
		if edit != nil {
			edit(o)
		}
	})
	c.SetTailTokenBudget(10)
	return c
}

// Drives the post tool caller with the attempt budget spent so it takes the proactive prune branch
func pruneAfterToolResults(t *testing.T, persistDisabled bool) ([]*state.Message, []*state.Message) {
	t.Helper()
	compressor := compactionCaseCompressor(t, func(o *ContextCompressorOptions) {
		o.ProactivePruneTokens, o.ProactivePruneMinResultChars, o.ProtectFirstN, o.ProtectLastN = 48_000, 8_000, 2, 4
	})
	compressor.LastPromptTokens = 120_000
	a := spAgent(t)
	a.contextCompressor, a.compressionEnabled, a.sessionID, a.persistDisabled = compressor, true, "prune-session", persistDisabled
	a.compressionFeasibilityChecked = true
	rows := []map[string]any{{"role": "system", "content": "sys"}}
	for i := range 8 {
		id := fmt.Sprintf("c%d", i)
		rows = append(rows, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}})
		content := "ok"
		if i < 3 {
			content = strings.Repeat(string(rune('A'+i))+compactionSecret, 600)
		}
		rows = append(rows, map[string]any{"role": "tool", "tool_call_id": id, "content": content})
	}
	msgs := rowsToMessages(rows)
	system := "sys"
	s := &LoopState{Messages: msgs, SystemMessage: &system, UserMessage: "ask", ActiveSystemPrompt: "sys", CompressionAttempts: 3,
		MaxCompressionAttempts: 3, EffectiveTaskID: "task"}
	if _, err := compressAfterToolResults(context.Background(), a, s); err != nil {
		t.Fatal(err)
	}
	return msgs, s.Messages
}

func TestCommittedProactivePrunePublishesOneRecord(t *testing.T) {
	caseHome(t)
	marks := compactionPublished(t)
	msgs, pruned := pruneAfterToolResults(t, false)
	if len(msgs) == len(pruned) && &msgs[0] == &pruned[0] {
		t.Fatal("nothing pruned")
	}
	if len(*marks) != 1 {
		t.Fatalf("marks %v", *marks)
	}
	m := (*marks)[0]
	p := m.data
	if m.sessionID != "prune-session" || m.name != "compaction" || p["kind"] != "prune_tool_results" || p["method"] != "deterministic_prune" ||
		p["trigger"] != "proactive_prune" {
		t.Fatalf("mark %v", m)
	}
	prunedCount, _ := p["tool_results_pruned"].(int)
	reclaimed, _ := p["tokens_reclaimed"].(int)
	if prunedCount <= 0 || reclaimed <= 0 || p["token_count_method"] != "estimate_rough" || p["messages_before"] != len(msgs) || p["messages_after"] != len(msgs) {
		t.Fatalf("payload %v", p)
	}
	if raw, err := json.Marshal(p); err != nil || strings.Contains(string(raw), compactionSecret) {
		t.Fatalf("payload leaked transcript text %v", err)
	}
}

func compactionCaseMessages() []map[string]any {
	filler := strings.TrimSpace(strings.Repeat("context ", 200))
	msgs := []map[string]any{{"role": "system", "content": "system prompt"}}
	for idx := range 10 {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("user message %d %s %s", idx, compactionSecret, filler)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("assistant reply %d %s %s", idx, compactionSecret, filler)})
	}
	return msgs
}

func TestAPersistenceDetachedForkPublishesNothing(t *testing.T) {
	caseHome(t)
	marks := compactionPublished(t)
	a := spAgent(t)
	a.contextCompressor = compactionCaseCompressor(t, nil)
	a.sessionID, a.platform, a.model, a.provider = "session-events-test", "cli", "test/main-model", "test-provider"
	a.compressionFeasibilityChecked, a.persistDisabled = true, true
	patchCallLLM(t, func(map[string]any) (any, error) {
		return summaryResponse(map[string]any{"role": "assistant", "content": "SANITIZED SUMMARY"}), nil
	})
	system, approx, trigger := "system prompt", 80_000, "pre_api"
	compressed, _, err := compressContext(context.Background(), a, compactionCaseMessages(), &system, compressContextOptions{ApproxTokens: &approx, Trigger: &trigger})
	if err != nil {
		t.Fatal(err)
	}
	msgs, pruned := pruneAfterToolResults(t, true)
	if len(compressed) >= len(compactionCaseMessages()) || (len(msgs) == len(pruned) && &msgs[0] == &pruned[0]) {
		t.Fatalf("compressed %d rows, prune changed nothing", len(compressed))
	}
	if len(*marks) != 0 {
		t.Fatalf("marks %v", *marks)
	}
}
