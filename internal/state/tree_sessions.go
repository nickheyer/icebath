package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type TreeSession struct {
	ChatID           string
	HasChat          bool
	MessageCount     int
	Ended            bool
	PendingAdmission bool
	ActiveSchedule   bool
}

const treeSessionFlagsSQL = `SELECT s.chat_id, s.message_count, s.ended_at IS NOT NULL,
    EXISTS (SELECT 1 FROM session_admissions a WHERE a.target_session_id=s.id AND a.status!='terminal'),
    EXISTS (SELECT 1 FROM state_meta m WHERE m.key IN ('loop:'||s.id, 'heartbeat:'||s.id, 'goal:'||s.id)
        AND (json_valid(m.value)=0 OR COALESCE(json_extract(m.value,'$.status'),'active')='active'))
FROM sessions s WHERE s.id IN (`

func (e *Engine) SessionsInside(ctx context.Context, path string, inside func(cwd string) bool) ([]TreeSession, error) {
	if inside == nil {
		return nil, &MissingHookError{Name: "worktree path matcher"}
	}
	db, err := e.OpenDB(ctx, path, DBOptions{Label: storeLabel, ReadOnly: true, BusyTimeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	var out []TreeSession
	err = db.Transact(ctx, false, func(c Conn) error {
		ids, err := treeSessionIDs(ctx, c, inside)
		if err != nil {
			return err
		}
		for _, batch := range chunks(ids, sqlInChunk) {
			args := make([]any, len(batch))
			for i, id := range batch {
				args[i] = id
			}
			rows, err := c.QueryContext(ctx, treeSessionFlagsSQL+bindMarks(len(batch))+")", args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var chat sql.Null[string]
				var count sql.Null[int64]
				var t TreeSession
				if err := rows.Scan(&chat, &count, &t.Ended, &t.PendingAdmission, &t.ActiveSchedule); err != nil {
					return errors.Join(err, rows.Close())
				}
				t.ChatID, t.HasChat = chat.V, chat.Valid
				t.MessageCount = int(count.V)
				out = append(out, t)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func treeSessionIDs(ctx context.Context, c Conn, inside func(cwd string) bool) ([]string, error) {
	rows, err := c.QueryContext(ctx, "SELECT id, cwd FROM sessions")
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		var cwd sql.Null[string]
		if err := rows.Scan(&id, &cwd); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if inside(cwd.V) {
			ids = append(ids, id)
		}
	}
	return ids, errors.Join(rows.Err(), rows.Close())
}
