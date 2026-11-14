package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Messages a resume or export holds by default before the guard refuses it
const DefaultTranscriptLimit = 20000

// Effective transcript limit of a configured sessions setting, the default when unset or negative and zero disabling the guard
func TranscriptLimit(configured *int64) int {
	if configured == nil || *configured < 0 {
		return DefaultTranscriptLimit
	}
	return int(*configured)
}

// Resume past the configured message limit
type ResumeTooLargeError struct {
	MessageCount int
	Limit        int
	// What was counted, the whole lineage or the tip segment alone
	Scope string
}

// User facing refusal naming the uncapped export paths and the setting that raises the limit
func (e *ResumeTooLargeError) Error() string {
	return fmt.Sprintf("This session is too long to reload safely (%d messages; limit %d). Start a fresh chat and keep a copy "+
		"with the dashboard Sessions page's Export action or `icebath sessions export --format md --session-id <id>` "+
		"(neither is capped), or raise the limit with `icebath config set sessions.max_resume_messages 0`.", e.MessageCount, e.Limit)
}

// In memory export past the configured message limit
type ExportTooLargeError struct {
	SessionID    string
	MessageCount int
	Limit        int
}

// User facing refusal every in-memory JSON export shares
func (e *ExportTooLargeError) Error() string {
	return fmt.Sprintf("Session '%s' has more than %s exportable messages; the JSON/JSONL backup is built in memory and capped "+
		"per session. Use the dashboard Sessions page's streaming Export action, or set sessions.max_export_messages: 0 in "+
		"config.yaml to disable the guard.", e.SessionID, groupThousands(e.Limit))
}

// Decimal text of n with comma thousands separators
func groupThousands(n int) string {
	text := fmt.Sprint(n)
	neg := strings.HasPrefix(text, "-")
	digits := strings.TrimPrefix(text, "-")
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Error of a negative transcript limit handed to a guard
var ErrNegativeLimit = errors.New("max_messages must be non-negative")

// Rows an export of the session materializes, refusing past limit, zero disabling the guard
func (s *Store) AssertExportSafe(ctx context.Context, sessionID string, limit int) (int, error) {
	if limit < 0 {
		return 0, ErrNegativeLimit
	}
	if limit == 0 {
		return 0, nil
	}
	var n int
	err := s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT 1 FROM messages WHERE session_id = ? LIMIT ?)", sessionID, limit+1).Scan(&n)
	})
	if err != nil {
		return 0, err
	}
	if n > limit {
		return n, &ExportTooLargeError{SessionID: sessionID, MessageCount: n, Limit: limit}
	}
	return n, nil
}

// Export guard over every id with one limit, zero running no queries
func (s *Store) AssertExportsSafe(ctx context.Context, sessionIDs []string, limit int) error {
	if limit == 0 {
		return nil
	}
	for _, id := range sessionIDs {
		if _, err := s.AssertExportSafe(ctx, id, limit); err != nil {
			return err
		}
	}
	return nil
}

// Retags legacy kanban worker rows from cli by cwd under the board's workspaces root once per root, returning rows retagged
func (s *Store) RetagKanbanWorkerSessions(ctx context.Context, workspacesRoot string) (int64, error) {
	prefix := strings.TrimRight(workspacesRoot, `/\`)
	if prefix == "" {
		return 0, nil
	}
	gate := "kanban_worker_source_retagged:" + prefix
	if v, ok, err := s.GetMeta(ctx, gate); err != nil || (ok && v == "1") {
		return 0, err
	}
	var retagged int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		esc := EscapeLike(prefix)
		res, err := c.ExecContext(ctx, "UPDATE sessions SET source = 'kanban' "+
			`WHERE source = 'cli' AND (cwd = ? OR cwd LIKE ? ESCAPE '\' `+
			`OR cwd LIKE ? ESCAPE '\')`, prefix, esc+"/%", esc+`\\%`)
		if err != nil {
			return err
		}
		if retagged, err = res.RowsAffected(); err != nil {
			return err
		}
		return SetMetaIn(ctx, c, gate, "1")
	})
	return retagged, err
}

// Reports whether the session or any segment its resume materializes belongs to the kanban dispatcher
func (s *Store) IsKanbanOwnedSession(ctx context.Context, sessionID string) (bool, error) {
	lineage, err := s.ResumeLineageIDs(ctx, sessionID)
	if err != nil || len(lineage) == 0 {
		return false, err
	}
	var hits []string
	err = s.Read(ctx, func(c Conn) error {
		var err error
		hits, err = queryStrings(ctx, c, "SELECT 1 FROM sessions WHERE id IN ("+bindMarks(len(lineage))+") "+
			"AND (source = 'kanban' OR created_source = 'kanban') LIMIT 1", stringArgs(lineage)...)
		return err
	})
	return len(hits) > 0, err
}
