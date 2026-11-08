package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Lifecycle statuses a session transcript tail classifies to
const (
	SessionStatusComplete    = "complete"
	SessionStatusInterrupted = "interrupted"
	SessionStatusError       = "error"
	SessionStatusEmpty       = "empty"
)

// finish_reason values meaning the turn ended in a provider or agent error
var errorFinishReasons = map[string]bool{"error": true, "agent_error": true, "content_filter": true}

// Error for an error finish, interrupted for pending tool calls or a trailing user or tool row, else complete
func ClassifySessionStatus(role any, hasToolCalls bool, finishReason any) string {
	finish, _ := finishReason.(string)
	if errorFinishReasons[pystr.Lower(pystr.Strip(finish))] {
		return SessionStatusError
	}
	roleText, _ := role.(string)
	r := pystr.Lower(pystr.Strip(roleText))
	if r == "user" || r == "tool" || (r == "assistant" && hasToolCalls) {
		return SessionStatusInterrupted
	}
	return SessionStatusComplete
}

// Lifecycle status of each session from its last message row in one query, empty when it has none
func (d *SessionDB) SessionLifecycleStatuses(ctx context.Context, sessionIDs []string) (map[string]string, error) {
	ids := make([]string, 0, len(sessionIDs))
	for _, sid := range sessionIDs {
		if sid != "" {
			ids = append(ids, sid)
		}
	}
	statuses := map[string]string{}
	if len(ids) == 0 {
		return statuses, nil
	}
	for _, sid := range ids {
		statuses[sid] = SessionStatusEmpty
	}
	rows, err := d.readAll(ctx, `
            SELECT m.session_id, m.role,
                   m.tool_calls IS NOT NULL AS has_tool_calls,
                   m.finish_reason
            FROM messages m
            JOIN (
                SELECT session_id, MAX(id) AS max_id
                FROM messages
                WHERE session_id IN (`+placeholders(len(ids))+`)
                GROUP BY session_id
            ) latest ON m.id = latest.max_id
        `, stringArgs(ids)...)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		statuses[pyStr(rowValue(row, "session_id"))] = ClassifySessionStatus(rowValue(row, "role"),
			pyTruthy(rowValue(row, "has_tool_calls")), rowValue(row, "finish_reason"))
	}
	return statuses, nil
}
