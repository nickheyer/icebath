package agent

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func overridePersistAgent(t *testing.T, sid string, override, timestamp any) (*AIAgent, *state.SessionDB) {
	t.Helper()
	a, db := newPersistTestAgent(t, sid)
	idx := 0
	a.persistUserMessageIdx, a.persistUserMessageOverride, a.persistUserMessageTimestamp = &idx, override, timestamp
	return a, db
}

func TestFlushOverrideMovesSentBytesToSidecar(t *testing.T) {
	a, db := overridePersistAgent(t, "sess-ov", "actual question", nil)
	live := "[gateway note] observed context\n\nactual question"
	messages := []map[string]any{{"role": "user", "content": live}}
	mustFlush(t, a, messages)
	rows := persistConversation(t, db, "sess-ov", false)
	if rows[0]["content"] != "actual question" || rows[0]["api_content"] != live || messages[0]["content"] != live {
		t.Fatalf("rows %v live %v", rows, messages)
	}
}

func TestFlushStampedSidecarWinsOverOverrideDerivation(t *testing.T) {
	a, db := overridePersistAgent(t, "sess-ov2", "clean text", nil)
	mustFlush(t, a, []map[string]any{{"role": "user", "content": "live text", "api_content": "live text\n\nPLUGIN-CTX"}})
	rows := persistConversation(t, db, "sess-ov2", false)
	if rows[0]["content"] != "clean text" || rows[0]["api_content"] != "live text\n\nPLUGIN-CTX" {
		t.Fatalf("rows %v", rows)
	}
}

func TestFlushOverrideSkippedForCompressionMergedRow(t *testing.T) {
	a, db := overridePersistAgent(t, "sess-merged", "actual question", nil)
	merged := "[prior context]\ncompaction summary\n\nactual question"
	mustFlush(t, a, []map[string]any{{"role": "user", "content": merged, state.CompressedSummaryMetadataKey: true}})
	rows := persistConversation(t, db, "sess-merged", false)
	if _, sidecar := rows[0]["api_content"]; rows[0]["content"] != merged || sidecar {
		t.Fatalf("rows %v", rows)
	}
}

func TestLiveOverrideSkippedForCompressionMergedRow(t *testing.T) {
	a, _ := overridePersistAgent(t, "sess-merged-live", "actual question", 1730000000)
	merged := "[prior context]\ncompaction summary\n\nactual question"
	messages := []map[string]any{{"role": "user", "content": merged, state.CompressedSummaryMetadataKey: true}}
	a.applyPersistUserMessageOverride(messages)
	if messages[0]["content"] != merged || messages[0]["timestamp"] != 1730000000 {
		t.Fatalf("the compaction summary was erased from the continuation history %v", messages)
	}
}

func TestLiveOverrideStillAppliesWithoutMergeMarker(t *testing.T) {
	a, _ := overridePersistAgent(t, "sess-plain-live", "actual question", nil)
	messages := []map[string]any{{"role": "user", "content": "[gateway note] observed\n\nactual question"}}
	a.applyPersistUserMessageOverride(messages)
	if messages[0]["content"] != "actual question" {
		t.Fatalf("messages %v", messages)
	}
}

func TestFlushCapturesUserContentSanitizeWouldRewrite(t *testing.T) {
	a, db := newPersistTestAgent(t, "sess-fence")
	raw := "what does a literal <memory-context> tag do?"
	if state.SanitizeContext(raw) == raw {
		t.Fatal("precondition: sanitize must rewrite the fence")
	}
	mustFlush(t, a, []map[string]any{{"role": "user", "content": raw}, {"role": "assistant", "content": "it fences recalled memory <memory-context>"}})
	rows := persistConversation(t, db, "sess-fence", false)
	if rows[0]["content"] != strings.TrimSpace(state.SanitizeContext(raw)) || rows[0]["api_content"] != raw || rows[1]["api_content"] != "it fences recalled memory <memory-context>" {
		t.Fatalf("rows %v", rows)
	}
}

func TestFlushLeavesPlainWhitespacePaddingUncaptured(t *testing.T) {
	a, db := newPersistTestAgent(t, "sess-ws")
	mustFlush(t, a, []map[string]any{{"role": "user", "content": "  hello  \n"}})
	if _, ok := persistConversation(t, db, "sess-ws", false)[0]["api_content"]; ok {
		t.Fatal("whitespace padding produced a sidecar")
	}
}
