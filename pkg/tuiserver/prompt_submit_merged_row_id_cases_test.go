package tuiserver

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Durable row with its id, role, content and display kind, empty kind for a plain row
type mergeRow struct {
	id      int64
	role    string
	content string
	kind    string
}

// Replaces the session transcript with the physical rows, display kinds included
func seedMergeRows(t *testing.T, db *state.DB, key string, rows []mergeRow) {
	t.Helper()
	err := db.ExecuteWrite(context.Background(), func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(context.Background(), "DELETE FROM messages WHERE session_id = ?", key); err != nil {
			return err
		}
		for i, row := range rows {
			var kind any
			if row.kind != "" {
				kind = row.kind
			}
			if _, err := conn.ExecContext(context.Background(),
				"INSERT INTO messages (id, session_id, role, content, timestamp, display_kind) VALUES (?, ?, ?, ?, ?, ?)",
				row.id, key, row.role, row.content, 1000+i, kind); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Message dicts of the physical rows, with or without their row id stamps
func mergeDicts(rows []mergeRow, stamped bool) []map[string]any {
	var out []map[string]any
	for _, row := range rows {
		m := map[string]any{"role": row.role, "content": row.content}
		if row.kind != "" {
			m["display_kind"] = row.kind
		}
		if stamped {
			m["_row_id"] = row.id
		}
		out = append(out, m)
	}
	return out
}

func mergeHistory(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

func repairedMerge(t *testing.T, rows []mergeRow, merges int) []map[string]any {
	t.Helper()
	repaired := mergeDicts(rows, true)
	n, err := state.RepairMessageSequence(&repaired)
	if err != nil || n != merges {
		t.Fatalf("repair merged %d %v", n, err)
	}
	return repaired
}

// Submits the cut and returns the active rows the store keeps before the submitted prompt
func submitMergedCut(t *testing.T, db *state.DB, sid string, rowID int64, text string) [][3]any {
	t.Helper()
	resp := submitRPC(sid, map[string]any{"text": text, "truncate_before_row_id": rowID, "confirm_truncate": true})
	if resp["error"] != nil {
		t.Fatalf("submit %v", resp["error"])
	}
	written := activeRows(t, db, "session-key")
	if n := len(written); n == 0 || written[n-1][1] != "user" || written[n-1][2] != text {
		t.Fatalf("submitted prompt not the last row %v", written)
	}
	return written[:len(written)-1]
}

func TestPromptSubmitResolvesRowIdAbsorbedIntoMarkerMerge(t *testing.T) {
	db, _ := submitCaseDB(t, "session-key")
	verbatim := []mergeRow{{12131, "user", "first", ""}, {12133, "assistant", "reply 1", ""},
		{12134, "user", "[System: The active model for this chat has changed to ds4.]", "model_switch"}, {12135, "user", "the dropped prompt", ""}}
	repaired := repairedMerge(t, verbatim, 1)
	if len(repaired) != 3 || !pyjson.Equal(repaired[2]["_row_id"], 12135) || repaired[2]["display_kind"] != nil {
		t.Fatalf("repaired %v", repaired)
	}
	seedMergeRows(t, db, "session-key", verbatim)
	session := parkedSubmitSession(t, "merged-marker-sid", map[string]any{"history": mergeHistory(repairedMerge(t, verbatim, 1))})
	kept := submitMergedCut(t, db, "merged-marker-sid", 12135, "new turn")
	if !reflect.DeepEqual(kept, [][3]any{{int64(12131), "user", "first"}, {int64(12133), "assistant", "reply 1"}}) {
		t.Fatalf("kept %v", kept)
	}
	if history, _ := session.Get("history").([]any); len(history) != 2 {
		t.Fatalf("history %v", session.Get("history"))
	}
}

func TestPromptSubmitResolvesRowIdSwallowedByMarkerMerge(t *testing.T) {
	db, _ := submitCaseDB(t, "session-key")
	verbatim := []mergeRow{{201, "user", "first", ""}, {202, "assistant", "reply 1", ""},
		{203, "user", "[System: The active model for this chat has changed to ds4.]", "model_switch"},
		{204, "user", "[System: The active model for this chat has changed to kk3.]", "model_switch"}, {205, "user", "the interrupted prompt", ""}}
	if repaired := repairedMerge(t, verbatim, 2); len(repaired) != 3 {
		t.Fatalf("repaired %v", repaired)
	}
	seedMergeRows(t, db, "session-key", verbatim)
	session := parkedSubmitSession(t, "marker-merge-sid", map[string]any{"history": mergeHistory(mergeDicts(verbatim, false))})
	kept := submitMergedCut(t, db, "marker-merge-sid", 205, "edited prompt")
	var contents []any
	for _, row := range kept {
		contents = append(contents, row[2])
	}
	if !reflect.DeepEqual(contents, []any{"first", "reply 1", verbatim[2].content, verbatim[3].content}) {
		t.Fatalf("kept %v", kept)
	}
	if history, _ := session.Get("history").([]any); len(history) != 4 {
		t.Fatalf("history %v", session.Get("history"))
	}
}

func TestPromptSubmitResolvesRowIdSwallowedByPlainUserMerge(t *testing.T) {
	db, _ := submitCaseDB(t, "session-key")
	verbatim := []mergeRow{{301, "user", "first", ""}, {302, "assistant", "reply 1", ""},
		{303, "user", "prompt whose turn was cut off", ""}, {304, "user", "the resent prompt", ""}}
	repaired := repairedMerge(t, verbatim, 1)
	if len(repaired) != 3 || !pyjson.Equal(repaired[2]["_row_id"], 303) {
		t.Fatalf("repaired %v", repaired)
	}
	seedMergeRows(t, db, "session-key", verbatim)
	session := parkedSubmitSession(t, "plain-merge-sid", map[string]any{"history": mergeHistory(repairedMerge(t, verbatim, 1))})
	kept := submitMergedCut(t, db, "plain-merge-sid", 304, "edited resend")
	var contents []any
	for _, row := range kept {
		contents = append(contents, row[2])
	}
	if !reflect.DeepEqual(contents, []any{"first", "reply 1", "prompt whose turn was cut off"}) {
		t.Fatalf("kept %v", kept)
	}
	if history, _ := session.Get("history").([]any); len(history) != 3 {
		t.Fatalf("history %v", session.Get("history"))
	}
}
