package tuiserver

import (
	"context"
	"testing"

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
