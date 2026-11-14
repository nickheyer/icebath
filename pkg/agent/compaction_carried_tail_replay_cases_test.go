package agent

import (
	"fmt"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const carriedSummaryText = "[CONTEXT COMPACTION] summary of the earlier turns"

// Store holding four alternating old turns in one session, the hermes db fixture
func carriedTailDB(t *testing.T) *state.SessionDB {
	t.Helper()
	db := restoreRealDB(t, "sess1", "test")
	for i := range 4 {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		if _, err := db.AppendMessage(t.Context(), "sess1", map[string]any{"role": role, "content": fmt.Sprintf("old turn %d", i)}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Content the next provider request carries per live row through the turn sidecar substitution
func carriedReplayed(t *testing.T, db *state.SessionDB) []any {
	t.Helper()
	var replayed []any
	for _, m := range persistConversation(t, db, "sess1", false) {
		api := shallowRow(m)
		substituteAPIContent(api)
		replayed = append(replayed, api["content"])
	}
	return replayed
}

func TestCompactionCarriedTailNotCloned(t *testing.T) {
	caseHome(t)
	t.Run("test_a_claim_keeps_the_rows_current_replay_sidecar", func(t *testing.T) {
		db := carriedTailDB(t)
		watermark, err := db.GetActiveMessageWatermark(t.Context(), "sess1")
		if err != nil {
			t.Fatal(err)
		}
		flushed := map[string]any{"role": "user", "content": "hello", "api_content": "first exact provider input"}
		if _, err := db.AppendMessagesBatch(t.Context(), "sess1", []map[string]any{flushed}, state.AppendGuards{}, nil); err != nil {
			t.Fatal(err)
		}
		held := shallowRow(flushed)
		rowID, ok := intLike(flushed["_row_id"])
		if !ok {
			t.Fatalf("flushed dict carries no row id %v", flushed)
		}
		if _, err := db.SetMessageAPIContent(t.Context(), "sess1", rowID, "hello", "new exact provider input"); err != nil {
			t.Fatal(err)
		}
		summary := map[string]any{"role": "user", "content": carriedSummaryText}
		if _, err := db.ArchiveAndCompact(t.Context(), "sess1", []map[string]any{summary, held}, state.ArchiveCompactOptions{Watermark: &watermark, TailCount: 1}); err != nil {
			t.Fatal(err)
		}
		if got := carriedReplayed(t, db); !slices.Contains(got, any("new exact provider input")) {
			t.Fatalf("replayed %v", got)
		}
	})
}
