package tuiserver

import (
	"context"
	"fmt"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Replaces the session transcript with rows under the given ids, the second column role and the third content
func seedRowsWithIDs(t *testing.T, db *state.DB, key string, rows ...[3]any) {
	t.Helper()
	err := db.ExecuteWrite(context.Background(), func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(context.Background(), "DELETE FROM messages WHERE session_id = ?", key); err != nil {
			return err
		}
		for i, row := range rows {
			if _, err := conn.ExecContext(context.Background(),
				"INSERT INTO messages (id, session_id, role, content, timestamp) VALUES (?, ?, ?, ?, ?)",
				row[0], key, row[1], row[2], 1000+i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Active rows of the session as id, role and content triples
func activeRows(t *testing.T, db *state.DB, key string) [][3]any {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(context.Background(), key, state.ConversationOptions{IncludeRowIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	out := [][3]any{}
	for _, row := range rows {
		id, _ := messageRowID(row)
		out = append(out, [3]any{id, row["role"], row["content"]})
	}
	return out
}

// Three user and assistant turns stamped 100 to 105
func threeTurnHistory() ([]any, [][3]any) {
	var history []any
	var rows [][3]any
	for turn := 0; turn < 3; turn++ {
		history = append(history,
			map[string]any{"_row_id": int64(100 + 2*turn), "role": "user", "content": fmt.Sprintf("u%d", turn)},
			map[string]any{"_row_id": int64(101 + 2*turn), "role": "assistant", "content": fmt.Sprintf("a%d", turn)})
		rows = append(rows, [3]any{int64(100 + 2*turn), "user", fmt.Sprintf("u%d", turn)},
			[3]any{int64(101 + 2*turn), "assistant", fmt.Sprintf("a%d", turn)})
	}
	return history, rows
}

func submitRPC(sid string, params map[string]any) map[string]any {
	merged := map[string]any{"session_id": sid}
	for key, value := range params {
		merged[key] = value
	}
	return handleRequest(context.Background(), map[string]any{"id": "1", "method": "prompt.submit", "params": merged})
}

func fenceSubmit(rid string) map[string]any {
	return lookupMethod("prompt.submit")(context.Background(), rid, map[string]any{"session_id": "session-1", "text": "continue"})
}

// Whether the submit passed the fence into normal admission and was accepted
func reachedAdmission(result map[string]any) bool {
	accepted, _ := result["result"].(map[string]any)
	return accepted["status"] == "streaming"
}

// Hosted room of the given members whose authority is this gateway
func createFenceRoom(t *testing.T, roomID string, members []any) {
	t.Helper()
	ctx := context.Background()
	authority, err := gateway.LocalAuthorityGatewayID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.CreateRoom(ctx, gateway.HostedRoomsDefaultDBPath(ctx), gateway.CreateRoomInput{RoomID: roomID, Name: roomID,
		Members: members, AuthorityGatewayID: authority}); err != nil {
		t.Fatal(err)
	}
}
