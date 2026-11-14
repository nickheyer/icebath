package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Session store recording each latest user sidecar backfill before delegating to the real store
type sidecarBackfillStore struct {
	*state.SessionDB
	calls [][3]any
}

func (s *sidecarBackfillStore) SetLatestUserAPIContent(ctx context.Context, sessionID string, content any, apiContent string) (int64, error) {
	s.calls = append(s.calls, [3]any{sessionID, content, apiContent})
	return s.SessionDB.SetLatestUserAPIContent(ctx, sessionID, content, apiContent)
}

// Engine whose gate opens for the first check only, so the preflight compresses exactly once
type compressOnceEngine struct {
	compactionEngine
	checks int
}

func (e *compressOnceEngine) ShouldCompress(*int) bool {
	e.checks++
	return e.checks == 1
}

func (e *compressOnceEngine) ShouldDeferPreflightToRealUsage(int) bool { return false }

func TestPrologueMoaAndInPlaceBackfill(t *testing.T) {
	caseHome(t)
	t.Run("test_no_stamp_for_moa_turns", func(t *testing.T) {
		a := prologueAgent(t)
		patchPluginHooks(t, func(string, map[string]any) []any { return []any{map[string]any{"context": "PLUGIN-CTX"}} })
		tc := prologueBuild(t, a, func(r *TurnContextRequest) { r.MoaActive = true })
		if msg := tc.Messages[tc.CurrentTurnUserIdx]; msg.APIContent != "" {
			t.Fatalf("sidecar %q", msg.APIContent)
		}
	})
	t.Run("test_inplace_compaction_backfills_sidecar_into_db", func(t *testing.T) {
		a := prologueAgent(t)
		store := &sidecarBackfillStore{SessionDB: blockDB(t, "state.db")}
		a.sessionDB, a.compressionEnabled = store, true
		engine := &compressOnceEngine{compactionEngine: *newCompactionEngine(1, 1000)}
		engine.ProtectFirstN, engine.ProtectLastN, engine.LastPromptTokens = 0, 0, -1
		a.contextCompressor = engine
		swapFacadeCompress(t, func(messages []map[string]any, _ *string, _ compressContextOptions) ([]map[string]any, string, error) {
			a.lastCompactionInPlace = true
			survivor := map[string]any{}
			for k, v := range messages[len(messages)-1] {
				survivor[k] = v
			}
			return []map[string]any{{"role": "assistant", "content": "compaction summary"}, survivor}, "SYSTEM", nil
		})
		patchPluginHooks(t, func(string, map[string]any) []any { return []any{map[string]any{"context": "PLUGIN-CTX"}} })
		big := strings.Repeat("x", 4000)
		tc := prologueBuild(t, a, func(r *TurnContextRequest) {
			r.ConversationHistory = []map[string]any{{"role": "user", "content": big}, {"role": "assistant", "content": big}}
		})
		msg := tc.Messages[tc.CurrentTurnUserIdx]
		if msg.Content != "hello" || msg.APIContent != "hello\n\nPLUGIN-CTX" {
			t.Fatalf("content %v sidecar %q", msg.Content, msg.APIContent)
		}
		if want := [][3]any{{"sess-1", "hello", "hello\n\nPLUGIN-CTX"}}; !reflect.DeepEqual(store.calls, want) {
			t.Fatalf("backfills %v", store.calls)
		}
	})
}
