package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type BackendHeartbeat struct {
	BackendID     string
	PID           int
	StartedAt     float64
	LastHeartbeat *float64
	Profile       string
	Host          string
}

// Upserts the liveness row of one backend, the heartbeat defaulting to now and an empty id writing nothing
func (d *SessionDB) RegisterBackendHeartbeat(ctx context.Context, h BackendHeartbeat) error {
	if h.BackendID == "" {
		return nil
	}
	ts := now()
	if h.LastHeartbeat != nil {
		ts = *h.LastHeartbeat
	}
	return d.writeSQL(ctx, "INSERT INTO gateway_heartbeats (backend_id, pid, started_at, last_heartbeat, profile, host)"+
		" VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(backend_id) DO UPDATE SET pid = excluded.pid,"+
		" started_at = excluded.started_at, last_heartbeat = excluded.last_heartbeat,"+
		" profile = excluded.profile, host = excluded.host",
		[]any{h.BackendID, h.PID, h.StartedAt, ts, h.Profile, h.Host}, false, nil)
}

// Removes the heartbeat row of one backend and reports whether a row went away
func (d *SessionDB) ClearBackendHeartbeat(ctx context.Context, backendID string) (bool, error) {
	if backendID == "" {
		return false, nil
	}
	n, err := d.writeRowcount(ctx, "DELETE FROM gateway_heartbeats WHERE backend_id = ?", []any{backendID}, nil)
	return n > 0, err
}

// Drops heartbeat rows older than the staleness window and returns the removed backend ids
func (d *SessionDB) PruneStaleHeartbeats(ctx context.Context, maxAgeSeconds float64) ([]string, error) {
	if maxAgeSeconds <= 0 {
		return []string{}, nil
	}
	cutoff := now() - maxAgeSeconds
	removed := []string{}
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		removed = []string{}
		rows, err := connRows(ctx, conn, "DELETE FROM gateway_heartbeats WHERE last_heartbeat < ? RETURNING backend_id", cutoff)
		for _, row := range rows {
			removed = append(removed, pyStr(row["backend_id"]))
		}
		return err
	}, nil)
	return removed, err
}

// Snapshot of every backend heartbeat row, freshest first
func (d *SessionDB) ListBackendHeartbeats(ctx context.Context) ([]*pyjson.Object, error) {
	return d.readAll(ctx, "SELECT backend_id, pid, started_at, last_heartbeat, profile, host FROM gateway_heartbeats"+
		" ORDER BY last_heartbeat DESC")
}
