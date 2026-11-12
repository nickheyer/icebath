package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Engine compressing once to a fixed compacted set, recording the session row seen when compression starts
type sessionRowEngine struct {
	compressOnceEngine
	db            *state.SessionDB
	sessionID     string
	compacted     []map[string]any
	rowAtCompress *pyjson.Object
}

func (e *sessionRowEngine) Compress([]map[string]any, *int, *string, bool, string) ([]map[string]any, error) {
	e.rowAtCompress, _ = e.db.GetSession(context.Background(), e.sessionID)
	out := make([]map[string]any, len(e.compacted))
	for i, row := range e.compacted {
		out[i] = map[string]any{"role": row["role"], "content": row["content"]}
	}
	return out, nil
}

// Fresh first turn agent over db whose compressor compacts to a summary plus the current user content
func sessionRowAgent(t *testing.T, db *state.SessionDB, sessionID string, inPlace bool, currentUserContent any) (*AIAgent, *sessionRowEngine) {
	t.Helper()
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", Platform: "cli", SessionDB: db, SessionID: sessionID})
	t.Cleanup(func() { a.Close() })
	if a.sessionDBCreated {
		t.Fatal("session row created at init")
	}
	a.cachedSystemPrompt, a.compressionEnabled, a.compressionInPlace, a.compressionFeasibilityChecked = "SYSTEM", true, inPlace, true
	engine := &sessionRowEngine{compressOnceEngine: compressOnceEngine{compactionEngine: *newCompactionEngine(1, 1000)}, db: db, sessionID: sessionID,
		compacted: []map[string]any{{"role": "assistant", "content": "[CONTEXT COMPACTION] summary"}, {"role": "user", "content": currentUserContent}}}
	engine.ProtectFirstN, engine.ProtectLastN, engine.LastPromptTokens, engine.CompressionCount = 0, 0, -1, 1
	a.contextCompressor = engine
	return a, engine
}

func oversizedHistory() []map[string]any {
	big := strings.Repeat("x", 4000)
	return []map[string]any{{"role": "user", "content": big}, {"role": "assistant", "content": big}}
}

// Content normalized through JSON so typed and generic part lists compare equal
func jsonParts(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSessionRowExistsBeforePreflightCompaction(t *testing.T) {
	caseHome(t)
	t.Run("test_in_place_first_turn_compaction_persists", func(t *testing.T) {
		db := blockDB(t, "state.db")
		sid := "sess-fresh-inplace"
		a, engine := sessionRowAgent(t, db, sid, true, "hello")
		patchPluginHooks(t, func(string, map[string]any) []any { return []any{} })
		tc := prologueBuild(t, a, func(r *TurnContextRequest) { r.ConversationHistory = oversizedHistory() })
		if engine.rowAtCompress == nil || a.sessionID != sid || !a.lastCompactionInPlace {
			t.Fatalf("row %v session %q in place %v", engine.rowAtCompress, a.sessionID, a.lastCompactionInPlace)
		}
		rows, err := db.GetMessages(context.Background(), sid, state.GetMessagesOptions{})
		if err != nil {
			t.Fatal(err)
		}
		contents := []any{}
		for _, row := range rows {
			content, _ := row.Get("content")
			contents = append(contents, content)
		}
		if !slices.Contains(contents, any("[CONTEXT COMPACTION] summary")) {
			t.Fatalf("stored contents %v", contents)
		}
		if got := tc.Messages[tc.CurrentTurnUserIdx].Content; got != "hello" {
			t.Fatalf("live content %v", got)
		}
	})
	t.Run("test_in_place_compaction_multimodal_context_part_survives_reload", func(t *testing.T) {
		db := blockDB(t, "state.db")
		sid := "sess-inplace-mm"
		image := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}
		turn := func() []any { return []any{map[string]any{"type": "text", "text": "what is this"}, image} }
		a, _ := sessionRowAgent(t, db, sid, true, turn())
		patchPluginHooks(t, func(string, map[string]any) []any { return []any{map[string]any{"context": "PLUGIN-CTX"}} })
		tc := prologueBuild(t, a, func(r *TurnContextRequest) {
			r.UserMessage, r.ConversationHistory = turn(), oversizedHistory()
		})
		if !a.lastCompactionInPlace {
			t.Fatal("compaction not in place")
		}
		live := jsonParts(t, tc.Messages[tc.CurrentTurnUserIdx].Content)
		if want := jsonParts(t, append(turn(), map[string]any{"type": "text", "text": "PLUGIN-CTX"})); !reflect.DeepEqual(live, want) {
			t.Fatalf("live %v", live)
		}
		rows, err := db.GetMessagesAsConversation(context.Background(), sid, state.ConversationOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var users []map[string]any
		for _, row := range rows {
			if row["role"] == "user" {
				users = append(users, row)
			}
		}
		if len(users) == 0 || !reflect.DeepEqual(jsonParts(t, users[len(users)-1]["content"]), live) {
			t.Fatalf("reloaded users %v", users)
		}
	})
	t.Run("test_rotation_first_turn_compaction_creates_child", func(t *testing.T) {
		db := blockDB(t, "state.db")
		sid := "sess-fresh-rot"
		a, engine := sessionRowAgent(t, db, sid, false, "hello")
		patchPluginHooks(t, func(string, map[string]any) []any { return []any{} })
		prologueBuild(t, a, func(r *TurnContextRequest) { r.ConversationHistory = oversizedHistory() })
		if engine.rowAtCompress == nil {
			t.Fatal("no session row when compression started")
		}
		child := a.sessionID
		if child == sid {
			t.Fatal("no rotation")
		}
		childRow, err := db.GetSession(context.Background(), child)
		if err != nil || childRow == nil {
			t.Fatalf("child row %v %v", childRow, err)
		}
		if parent, _ := childRow.Get("parent_session_id"); parent != sid {
			t.Fatalf("child parent %v", parent)
		}
		parentRow, err := db.GetSession(context.Background(), sid)
		if err != nil || parentRow == nil {
			t.Fatalf("parent row %v %v", parentRow, err)
		}
		if reason, _ := parentRow.Get("end_reason"); reason != "compression" {
			t.Fatalf("parent end reason %v", reason)
		}
	})
}
