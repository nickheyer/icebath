package state

import (
	"context"
	"math"
)

// Compression continuation edge between parent alias p and child alias c
var continuationEdgeSQL = "p.end_reason = 'compression'\n" + nonContinuationChildSQL("c.", "p.id")

// Unpinned continuations a pinned segment covers, walked down the compression edges
var pinnedTailSQL = "WITH RECURSIVE tail(id) AS (" +
	" SELECT c.id FROM sessions c JOIN sessions p ON p.id = c.parent_session_id" +
	" WHERE COALESCE(p.pinned, 0) = 1 AND " + continuationEdgeSQL +
	" UNION" +
	" SELECT c.id FROM tail t JOIN sessions p ON p.id = t.id JOIN sessions c ON c.parent_session_id = p.id" +
	" WHERE " + continuationEdgeSQL +
	") SELECT id FROM tail"

// Predicate sparing pinned rows and the unpinned continuations a pinned segment covers
func notPinnedSQL(alias string) string {
	return "COALESCE(" + alias + ".pinned, 0) = 0 AND " + alias + ".id NOT IN (" + pinnedTailSQL + ")"
}

// Orphan sweep scope, nil heartbeat windows meaning the defaults derived from the idle window
type SweepOrphanedOptions struct {
	MaxIdleSeconds                 float64
	Sources                        []string
	ExcludeIDs                     []string
	ExcludePinned                  bool
	HeartbeatStalenessSeconds      *float64
	HeartbeatOwnershipGraceSeconds *float64
	RespectGatewayHeartbeats       bool
}

// Sweep over tui, desktop and subagent rows honouring gateway heartbeats
func NewSweepOrphanedOptions(maxIdleSeconds float64) SweepOrphanedOptions {
	return SweepOrphanedOptions{MaxIdleSeconds: maxIdleSeconds, Sources: []string{"tui", "desktop", "subagent"}, RespectGatewayHeartbeats: true}
}

// Ends idle open rows of the sources no live backend could own, sparing excluded and guarded rows
func (d *SessionDB) SweepOrphanedSessions(ctx context.Context, o SweepOrphanedOptions) ([]string, error) {
	var srcs []string
	for _, s := range o.Sources {
		if s != "" {
			srcs = append(srcs, s)
		}
	}
	if o.MaxIdleSeconds <= 0 || len(srcs) == 0 {
		return []string{}, nil
	}
	staleness := o.MaxIdleSeconds * 2
	if o.HeartbeatStalenessSeconds != nil && *o.HeartbeatStalenessSeconds > 0 && !math.IsNaN(*o.HeartbeatStalenessSeconds) {
		staleness = *o.HeartbeatStalenessSeconds
	}
	grace := staleness
	if o.HeartbeatOwnershipGraceSeconds != nil && *o.HeartbeatOwnershipGraceSeconds >= 0 {
		grace = *o.HeartbeatOwnershipGraceSeconds
	}
	t := now()
	cutoff := t - o.MaxIdleSeconds
	pinScope := ""
	if o.ExcludePinned {
		pinScope = " AND " + notPinnedSQL("sessions")
	}
	predicate := "started_at < ? AND " + sqlSessionLastActive("sessions") + " < ?"
	var heartbeat []any
	if o.RespectGatewayHeartbeats {
		predicate += " AND NOT EXISTS (SELECT 1 FROM gateway_heartbeats h WHERE" +
			" h.last_heartbeat >= ? AND h.started_at <= sessions.started_at + ?)"
		heartbeat = []any{t - staleness, grace}
	}
	scopeSQL := " AND source IN (" + placeholders(len(srcs)) + ")" + pinScope + " AND " + predicate
	scopeParams := append(append(stringsToAny(srcs), cutoff, cutoff), heartbeat...)
	excluded := map[string]bool{}
	for _, x := range o.ExcludeIDs {
		if x != "" {
			excluded[x] = true
		}
	}
	var victims []string
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		victims = nil
		rows, err := connRows(ctx, conn, "SELECT id FROM sessions WHERE ended_at IS NULL"+scopeSQL, scopeParams...)
		if err != nil {
			return err
		}
		for _, row := range rows {
			sid := pyStr(row["id"])
			if excluded[sid] {
				continue
			}
			rejected, err := d.writeGuardsReject(ctx, conn, sid, transcriptGuards{})
			if err != nil {
				return err
			}
			if !rejected {
				victims = append(victims, sid)
			}
		}
		if len(victims) == 0 {
			return nil
		}
		params := append(append([]any{now()}, stringsToAny(victims)...), scopeParams...)
		_, err = conn.ExecContext(ctx, "UPDATE sessions SET ended_at = ?, end_reason = 'startup_orphan_reap'"+
			" WHERE id IN ("+placeholders(len(victims))+") AND ended_at IS NULL"+scopeSQL, params...)
		return err
	}, nil)
	if err != nil {
		return nil, err
	}
	if victims == nil {
		victims = []string{}
	}
	return victims, nil
}

// Archives every lineage tip idle for the given days through the sweep provenance, the hidden Bot Chat exempt
func (d *SessionDB) ArchiveStaleSessions(ctx context.Context, idleDays float64, excludePinned bool) (int, error) {
	if idleDays < 0 || math.IsNaN(idleDays) {
		return 0, nil
	}
	cutoff := now() - idleDays*86400.0
	pinClause := ""
	if excludePinned {
		pinClause = "AND " + notPinnedSQL("s")
	}
	rows, err := d.readAll(ctx, `
            SELECT s.id FROM sessions s
            WHERE s.archived = 0
              AND COALESCE(s.end_reason, '') <> 'compression'
              `+pinClause+`
              AND NOT (COALESCE(s.hidden, 0) <> 0 AND COALESCE(s.title, '') = ?)
              AND `+sqlSessionLastActive("s")+` < ?
            ORDER BY s.started_at ASC
            `, CanonicalBotChatTitle, cutoff)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if _, err := d.autoArchiveLineage(ctx, pyStr(rowValue(row, "id"))); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}
