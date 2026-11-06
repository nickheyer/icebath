package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

const (
	PromptRefreshProfileKey       = "prompt_refresh.epoch"
	PromptRefreshSessionKeyPrefix = "prompt_refresh.epoch:"
)

// Integer of an aggregate column, zero for NULL
func aggregateInt(v any) (int, error) {
	if !pyTruthy(v) {
		return 0, nil
	}
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// Records a refresh request for every session or one session, returning the new epoch of the shared counter
func (d *SessionDB) RequestPromptRefresh(ctx context.Context, sessionID *string) (int, error) {
	key := PromptRefreshProfileKey
	if sessionID != nil {
		key = PromptRefreshSessionKeyPrefix + *sessionID
	}
	epoch := 0
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		row, err := connRow(ctx, conn, "SELECT MAX(CAST(value AS INTEGER)) AS m FROM state_meta WHERE key = ? OR substr(key, 1, ?) = ?",
			PromptRefreshProfileKey, len(PromptRefreshSessionKeyPrefix), PromptRefreshSessionKeyPrefix)
		if err != nil {
			return err
		}
		current, err := aggregateInt(row["m"])
		if err != nil {
			return err
		}
		epoch = current + 1
		_, err = conn.ExecContext(ctx, "INSERT INTO state_meta (key, value) VALUES (?, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, pyStr(epoch))
		return err
	}, nil)
	return epoch, err
}

// Newest refresh epoch that applies to the session, zero when never requested
func (d *SessionDB) PromptRefreshEpoch(ctx context.Context, sessionID string) (int, error) {
	row, err := d.readOne(ctx, "SELECT MAX(CAST(value AS INTEGER)) AS m FROM state_meta WHERE key IN (?, ?)",
		PromptRefreshProfileKey, PromptRefreshSessionKeyPrefix+sessionID)
	if err != nil || row == nil {
		return 0, err
	}
	return aggregateInt(rowValue(row, "m"))
}

// Sessions whose next turn would replay a stored prompt, the ones a refresh rebuilds
func (d *SessionDB) CountSessionsWithStoredPrompt(ctx context.Context) (int, error) {
	row, err := d.readOne(ctx, "SELECT COUNT(*) AS n FROM sessions WHERE system_prompt_hash IS NOT NULL OR system_prompt IS NOT NULL")
	if err != nil || row == nil {
		return 0, err
	}
	return aggregateInt(rowValue(row, "n"))
}
