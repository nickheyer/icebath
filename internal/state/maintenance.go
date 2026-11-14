package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

const DeleteReasonPrune = "prune"

var autoPruneStaleOpenSources = []string{"cli", "cron", "kanban", "acp", "api_server", "subagent", "tool", "recovered"}

var (
	ErrNegativeRetention = errors.New("OlderThanDays must be >= 0")
	ErrPruneOpenSessions = errors.New("prune never deletes open sessions: IncludeOpen is archive-only")
)

const (
	pruneTokensSQL = "(COALESCE(s.input_tokens, 0) + COALESCE(s.output_tokens, 0))"
	pruneCostSQL   = "COALESCE(s.actual_cost_usd, s.estimated_cost_usd, 0)"
	endedGuardSQL  = "s.ended_at IS NOT NULL"
)

var (
	pruneLastActiveSQL  = SessionLastActiveSQL("s")
	continuationEdgeSQL = "p.end_reason = 'compression'\n" + nonContinuationChildSQL("c.", "p.id")
	pinnedTailSQL       = "WITH RECURSIVE tail(id) AS (" +
		" SELECT c.id FROM sessions c JOIN sessions p ON p.id = c.parent_session_id" +
		" WHERE COALESCE(p.pinned, 0) = 1 AND " + continuationEdgeSQL +
		" UNION" +
		" SELECT c.id FROM tail t JOIN sessions p ON p.id = t.id JOIN sessions c ON c.parent_session_id = p.id" +
		" WHERE " + continuationEdgeSQL +
		") SELECT id FROM tail"
)

func continuedAncestorsSQL(candidatesWhere string) string {
	return "WITH RECURSIVE kept(id) AS (" +
		" SELECT p.id FROM sessions c JOIN sessions p ON p.id = c.parent_session_id" +
		" WHERE " + continuationEdgeSQL +
		" AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = c.id AND " + candidatesWhere + ")" +
		" UNION" +
		" SELECT p.id FROM kept k JOIN sessions c ON c.id = k.id JOIN sessions p ON p.id = c.parent_session_id" +
		" WHERE " + continuationEdgeSQL +
		") SELECT id FROM kept"
}

func notPinnedSQL(alias string) string {
	return "COALESCE(" + alias + ".pinned, 0) = 0 AND " + alias + ".id NOT IN (" + pinnedTailSQL + ")"
}

type PruneFilter struct {
	OlderThanDays    *float64
	Source           string
	LastActiveBefore *float64
	LastActiveAfter  *float64
	StartedBefore    *float64
	StartedAfter     *float64
	TitleLike        string
	EndReason        string
	CwdPrefix        string
	MinMessages      *int64
	MaxMessages      *int64
	ModelLike        string
	Provider         string
	UserID           string
	ChatID           string
	ChatType         string
	BranchLike       string
	MinTokens        *int64
	MaxTokens        *int64
	MinCost          *float64
	MaxCost          *float64
	MinToolCalls     *int64
	MaxToolCalls     *int64
	Archived         *bool
	IncludePinned    bool
	LineageTipsOnly  bool
	IncludeOpen      bool
	WholeLineages    bool
}

type PruneCandidate struct {
	ID           string
	Source       string
	Title        sql.Null[string]
	Model        sql.Null[string]
	StartedAt    sql.Null[float64]
	LastActive   sql.Null[float64]
	EndedAt      sql.Null[float64]
	MessageCount sql.Null[int64]
	Archived     int64
}

func likeContains(v string) string { return "%" + EscapeLike(strings.ToLower(v)) + "%" }

func pruneFilterWhere(f PruneFilter) (string, []any) {
	var clauses []string
	if !f.IncludeOpen {
		clauses = append(clauses, endedGuardSQL)
	}
	if f.LineageTipsOnly {
		clauses = append(clauses, "COALESCE(s.end_reason, '') <> 'compression'")
	}
	var args []any
	add := func(clause string, v any) {
		clauses = append(clauses, clause)
		args = append(args, v)
	}
	if f.LastActiveBefore != nil {
		clauses = append(clauses, pruneLastActiveSQL+" < ?",
			"(COALESCE(s.end_reason, '') != 'startup_orphan_reap' OR s.ended_at < ?)")
		args = append(args, *f.LastActiveBefore, *f.LastActiveBefore)
	}
	if f.LastActiveAfter != nil {
		add(pruneLastActiveSQL+" >= ?", *f.LastActiveAfter)
	}
	if f.StartedBefore != nil {
		add("s.started_at < ?", *f.StartedBefore)
	}
	if f.StartedAfter != nil {
		add("s.started_at >= ?", *f.StartedAfter)
	}
	if f.Source != "" {
		add("s.source = ?", f.Source)
	}
	if f.TitleLike != "" {
		add(`LOWER(COALESCE(s.title, '')) LIKE ? ESCAPE '\'`, likeContains(f.TitleLike))
	}
	if f.EndReason != "" {
		add("s.end_reason = ?", f.EndReason)
	}
	if f.CwdPrefix != "" {
		clause, bound := cwdPrefixClause(f.CwdPrefix)
		clauses = append(clauses, clause)
		args = append(args, bound...)
	}
	if f.MinMessages != nil {
		add("s.message_count >= ?", *f.MinMessages)
	}
	if f.MaxMessages != nil {
		add("s.message_count <= ?", *f.MaxMessages)
	}
	if f.ModelLike != "" {
		add(`LOWER(COALESCE(s.model, '')) LIKE ? ESCAPE '\'`, likeContains(f.ModelLike))
	}
	if f.Provider != "" {
		add("LOWER(COALESCE(s.billing_provider, '')) = ?", strings.ToLower(f.Provider))
	}
	if f.UserID != "" {
		add("s.user_id = ?", f.UserID)
	}
	if f.ChatID != "" {
		add("s.chat_id = ?", f.ChatID)
	}
	if f.ChatType != "" {
		add("s.chat_type = ?", f.ChatType)
	}
	if f.BranchLike != "" {
		add(`LOWER(COALESCE(s.git_branch, '')) LIKE ? ESCAPE '\'`, likeContains(f.BranchLike))
	}
	if f.MinTokens != nil {
		add(pruneTokensSQL+" >= ?", *f.MinTokens)
	}
	if f.MaxTokens != nil {
		add(pruneTokensSQL+" <= ?", *f.MaxTokens)
	}
	if f.MinCost != nil {
		add(pruneCostSQL+" >= ?", *f.MinCost)
	}
	if f.MaxCost != nil {
		add(pruneCostSQL+" <= ?", *f.MaxCost)
	}
	if f.MinToolCalls != nil {
		add("COALESCE(s.tool_call_count, 0) >= ?", *f.MinToolCalls)
	}
	if f.MaxToolCalls != nil {
		add("COALESCE(s.tool_call_count, 0) <= ?", *f.MaxToolCalls)
	}
	if f.Archived != nil {
		clauses = append(clauses, "s.archived = "+strconv.Itoa(boolInt(*f.Archived)))
	}
	if !f.IncludePinned {
		clauses = append(clauses, notPinnedSQL("s"))
	}
	return strings.Join(clauses, " AND "), args
}

func pruneWhere(f PruneFilter, wholeLineages bool) (string, []any, error) {
	if f.OlderThanDays != nil && f.LastActiveBefore == nil && f.StartedBefore == nil {
		days := *f.OlderThanDays
		if days < 0 {
			return "", nil, fmt.Errorf("%w, got %v: a negative retention builds a future cutoff that matches every ended session",
				ErrNegativeRetention, days)
		}
		cutoff := nowEpoch() - days*86400
		f.LastActiveBefore = &cutoff
	}
	where, args := pruneFilterWhere(f)
	if !wholeLineages {
		return where, args, nil
	}
	return where + " AND s.id NOT IN (" + continuedAncestorsSQL(where) + ")", append(append([]any{}, args...), args...), nil
}

func (s *Store) ListPruneCandidates(ctx context.Context, f PruneFilter) ([]PruneCandidate, error) {
	where, args, err := pruneWhere(f, f.WholeLineages)
	if err != nil {
		return nil, err
	}
	var out []PruneCandidate
	err = s.Read(ctx, func(c Conn) error {
		out = nil
		rows, err := c.QueryContext(ctx, `SELECT s.id, s.source, s.title, s.model, s.started_at,
                           `+pruneLastActiveSQL+` AS last_active,
                           s.ended_at, s.message_count, s.archived
                    FROM sessions s WHERE `+where+`
                    ORDER BY last_active ASC, s.started_at ASC`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p PruneCandidate
			if err := rows.Scan(lenient{&p.ID}, lenient{&p.Source}, lenient{&p.Title}, lenient{&p.Model}, lenient{&p.StartedAt},
				lenient{&p.LastActive}, lenient{&p.EndedAt}, lenient{&p.MessageCount}, lenient{&p.Archived}); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Store) countWhere(ctx context.Context, where string, args []any) (int64, error) {
	var n int64
	err := s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions s WHERE "+where, args...).Scan(&n)
	})
	return n, err
}

func (s *Store) CountPruneMatches(ctx context.Context, f PruneFilter, pinnedOnly bool) (int64, error) {
	if pinnedOnly {
		f.IncludePinned = true
	}
	where, args, err := pruneWhere(f, false)
	if err != nil {
		return 0, err
	}
	if pinnedOnly {
		where += " AND COALESCE(s.pinned, 0) = 1"
	}
	return s.countWhere(ctx, where, args)
}

func (s *Store) CountOpenPruneMatches(ctx context.Context, f PruneFilter) (int64, error) {
	if f.IncludeOpen {
		return 0, nil
	}
	where, args, err := pruneWhere(f, false)
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(where, endedGuardSQL) {
		return 0, errors.New("prune filter lost its ended-session safety guard")
	}
	return s.countWhere(ctx, "s.ended_at IS NULL"+where[len(endedGuardSQL):], args)
}

func (s *Store) PruneEmptyGhostSessions(ctx context.Context, sessionsDir string) (int, error) {
	cutoff := nowEpoch() - 86400
	var removed []string
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		removed = nil
		ids, err := queryStrings(ctx, c, `
                SELECT id FROM sessions
                WHERE source = 'tui'
                  AND title IS NULL
                  AND ended_at IS NOT NULL
                  AND started_at < ?
                  AND NOT EXISTS (
                      SELECT 1 FROM messages WHERE messages.session_id = sessions.id
                  )
            `, cutoff)
		if err != nil {
			return err
		}
		if ids, err = retirePrunable(ctx, c, ids); err != nil {
			return err
		}
		for _, chunk := range chunks(ids, sqlInChunk) {
			if _, err := c.ExecContext(ctx, "DELETE FROM sessions WHERE id IN ("+bindMarks(len(chunk))+")", stringArgs(chunk)...); err != nil {
				return err
			}
		}
		if len(ids) > 0 {
			if err := deleteUnreferencedSystemPrompts(ctx, c); err != nil {
				return err
			}
		}
		removed = ids
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.emitSessionDeleted(ctx, removed, DeleteReasonGhostPrune, "", sessionsDir)
	if err := s.collectRetiredMedia(ctx); err != nil {
		return len(removed), err
	}
	for _, sid := range removed {
		RemoveSessionFiles(sessionsDir, sid)
	}
	return len(removed), nil
}

func (s *Store) guardedIDs(ctx context.Context, c Conn, ids []string) (map[string]bool, error) {
	guarded := map[string]bool{}
	for _, sid := range ids {
		hit, err := s.writeGuardsReject(ctx, c, sid, true)
		if err != nil {
			return nil, err
		}
		if hit {
			guarded[sid] = true
		}
	}
	return guarded, nil
}

func (s *Store) writeGuardsReject(ctx context.Context, c Conn, sessionID string, allowClosedCompressionParent bool) (bool, error) {
	err := s.checkTranscriptWriteGuards(ctx, c, sessionID, writeGuards{
		rejectActiveTurnLease: true, rejectActiveCompressionLock: true, allowClosedCompressionParent: allowClosedCompressionParent,
	})
	var lost *TurnLeaseLostError
	if errors.Is(err, ErrCompressionInProgress) || errors.As(err, &lost) {
		return true, nil
	}
	return false, err
}

type SweepOptions struct {
	MaxIdle                 time.Duration
	Sources                 []string
	ExcludeIDs              []string
	ExcludePinned           bool
	IgnoreGatewayHeartbeats bool
}

func (s *Store) SweepOrphanedSessions(ctx context.Context, o SweepOptions) ([]string, error) {
	var srcs []string
	for _, v := range o.Sources {
		if v != "" {
			srcs = append(srcs, v)
		}
	}
	maxIdle := o.MaxIdle.Seconds()
	if maxIdle <= 0 || len(srcs) == 0 {
		return nil, nil
	}
	staleness := maxIdle * 2
	grace := staleness
	now := nowEpoch()
	cutoff := now - maxIdle
	pinScope := ""
	if o.ExcludePinned {
		pinScope = " AND " + notPinnedSQL("sessions")
	}
	orphan := "started_at < ? AND " + SessionLastActiveSQL("sessions") + " < ?"
	var heartbeatArgs []any
	if !o.IgnoreGatewayHeartbeats {
		orphan += " AND NOT EXISTS (SELECT 1 FROM gateway_heartbeats h WHERE" +
			" h.last_heartbeat >= ? AND h.started_at <= sessions.started_at + ?)"
		heartbeatArgs = []any{now - staleness, grace}
	}
	scopeSQL := " AND source IN (" + bindMarks(len(srcs)) + ")" + pinScope + " AND " + orphan
	scopeArgs := append(append(stringArgs(srcs), cutoff, cutoff), heartbeatArgs...)
	excluded := map[string]bool{}
	for _, x := range o.ExcludeIDs {
		if x != "" {
			excluded[x] = true
		}
	}
	var victims []string
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		victims = nil
		ids, err := queryStrings(ctx, c, "SELECT id FROM sessions WHERE ended_at IS NULL"+scopeSQL, scopeArgs...)
		if err != nil {
			return err
		}
		var found []string
		for _, sid := range ids {
			if excluded[sid] {
				continue
			}
			guarded, err := s.writeGuardsReject(ctx, c, sid, false)
			if err != nil {
				return err
			}
			if !guarded {
				found = append(found, sid)
			}
		}
		if len(found) == 0 {
			return nil
		}
		args := append(append([]any{nowEpoch()}, stringArgs(found)...), scopeArgs...)
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET ended_at = ?, end_reason = 'startup_orphan_reap'"+
			" WHERE id IN ("+bindMarks(len(found))+") AND ended_at IS NULL"+scopeSQL, args...); err != nil {
			return err
		}
		victims = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return victims, nil
}

func (s *Store) archiveStaleSessions(ctx context.Context, idleDays float64) (int, error) {
	if idleDays < 0 {
		return 0, nil
	}
	cutoff := nowEpoch() - idleDays*86400.0
	var ids []string
	err := s.Read(ctx, func(c Conn) error {
		var err error
		ids, err = queryStrings(ctx, c, `
            SELECT s.id FROM sessions s
            WHERE s.archived = 0
              AND COALESCE(s.end_reason, '') <> 'compression'
              AND `+notPinnedSQL("s")+`
              AND NOT (COALESCE(s.hidden, 0) <> 0 AND COALESCE(s.title, '') = ?)
              AND `+pruneLastActiveSQL+` < ?
            ORDER BY s.started_at ASC
            `, CanonicalBotChatTitle, cutoff)
		return err
	})
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := s.autoArchiveLineage(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

type AutoArchiveResult struct {
	Skipped  bool
	Archived int
	Err      error
}

func metaEpoch(raw string) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return v, err == nil
}

func epochText(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func (s *Store) MaybeAutoArchive(ctx context.Context, idleDays float64, minInterval time.Duration) AutoArchiveResult {
	var result AutoArchiveResult
	now := nowEpoch()
	raw, _, err := s.GetMeta(ctx, "last_auto_archive")
	if err == nil {
		last, _ := metaEpoch(raw)
		if last != 0 && now-last < minInterval.Seconds() {
			result.Skipped = true
			return result
		}
		s.tables.state.options.progress(900*time.Second, "state_db_auto_archive")
		result.Archived, err = s.archiveStaleSessions(ctx, idleDays)
	}
	if err == nil {
		err = s.SetMeta(ctx, "last_auto_archive", epochText(now))
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("state.db auto-archive failed: %s", errText(err)))
		result.Err = err
		return result
	}
	if result.Archived > 0 {
		slog.Info(fmt.Sprintf("state.db auto-archive: archived %d session(s) idle >= %v days", result.Archived, idleDays))
	}
	return result
}

type PruneSessionsOptions struct {
	SessionsDir              string
	ExcludeActiveWriteGuards bool
}

func (s *Store) PruneSessions(ctx context.Context, f PruneFilter, o PruneSessionsOptions) (int, []string, error) {
	if f.IncludeOpen {
		return 0, nil, ErrPruneOpenSessions
	}
	where, whereArgs, err := pruneWhere(f, true)
	if err != nil {
		return 0, nil, err
	}
	var removed []string
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		removed = nil
		ids, err := queryStrings(ctx, c, "SELECT s.id FROM sessions s WHERE "+where, whereArgs...)
		if err != nil {
			return err
		}
		set := map[string]bool{}
		for _, id := range ids {
			set[id] = true
		}
		if o.ExcludeActiveWriteGuards {
			guarded, err := s.guardedIDs(ctx, c, sortedKeys(set))
			if err != nil {
				return err
			}
			for id := range guarded {
				delete(set, id)
			}
		}
		sessionIDs, err := retirePrunable(ctx, c, sortedKeys(set))
		if err != nil {
			return err
		}
		if err := pruneRetiredTombstones(ctx, c); err != nil {
			return err
		}
		if len(sessionIDs) == 0 {
			return nil
		}
		var gone []string
		for _, chunk := range chunks(sessionIDs, sqlInChunk) {
			ph, args := bindMarks(len(chunk)), stringArgs(chunk)
			for _, q := range []string{
				"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IN (" + ph + ")",
				"DELETE FROM messages WHERE session_id IN (" + ph + ")",
				"DELETE FROM sessions WHERE id IN (" + ph + ")",
			} {
				if _, err := c.ExecContext(ctx, q, args...); err != nil {
					return err
				}
			}
			gone = append(gone, chunk...)
		}
		if err := deleteUnreferencedSystemPrompts(ctx, c); err != nil {
			return err
		}
		removed = gone
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	if len(removed) > 0 {
		s.emitSessionDeleted(ctx, removed, DeleteReasonPrune, "", o.SessionsDir)
	}
	if err := s.collectRetiredMedia(ctx); err != nil {
		return len(removed), removed, err
	}
	for _, sid := range removed {
		RemoveSessionFiles(o.SessionsDir, sid)
	}
	return len(removed), removed, nil
}

func (s *Store) pagePragmas(ctx context.Context, names []string, what string) ([]int64, bool) {
	values := make([]int64, len(names))
	err := s.ReadIfOpen(ctx, func(c Conn) error {
		for i, name := range names {
			if err := c.QueryRowContext(ctx, "PRAGMA "+name).Scan(&values[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, ErrClosed) {
			slog.Debug(fmt.Sprintf("Could not read %s: %s", what, errText(err)))
		}
		return nil, false
	}
	return values, true
}

func (s *Store) LogicalSizeBytes(ctx context.Context) (int64, bool) {
	v, ok := s.pagePragmas(ctx, []string{"page_count", "page_size"}, "logical DB size")
	if !ok {
		return 0, false
	}
	return v[0] * v[1], true
}

func (s *Store) freelistRatio(ctx context.Context) (float64, bool) {
	v, ok := s.pagePragmas(ctx, []string{"page_count", "freelist_count"}, "freelist ratio")
	if !ok {
		return 0, false
	}
	if v[0] <= 0 {
		return 0, true
	}
	return float64(v[1]) / float64(v[0]), true
}

func (s *Store) checkpointLogged(ctx context.Context, mode, when string) {
	if _, err := s.conn.q().ExecContext(ctx, "PRAGMA wal_checkpoint("+mode+")"); err != nil {
		slog.Debug(fmt.Sprintf("WAL checkpoint (%s) %s VACUUM failed: %s", mode, when, errText(err)))
	}
}

func (s *Store) recordIdentityLocked(ctx context.Context) {
	s.identity, s.identityKnown = statIdentity(s.path)
	s.sidecars = sidecarIdentities(s.path)
	if id, ok := s.eng.readApplicationID(s.path); ok && id != 0 {
		s.appID = id
		return
	}
	if s.conn != nil && s.appID == 0 {
		var id sql.NullInt64
		if err := s.conn.q().QueryRowContext(ctx, "PRAGMA application_id").Scan(&id); err == nil && id.Int64 > 0 {
			s.appID = uint32(id.Int64)
		}
	}
}

func (s *Store) Vacuum(ctx context.Context) (int, error) {
	if err := s.refuseIfCorrupt(false); err != nil {
		return 0, err
	}
	optimized, err := s.OptimizeFTS(ctx)
	if err != nil {
		slog.Warn(fmt.Sprintf("FTS optimize before VACUUM failed: %s", errText(err)))
		optimized = 0
	}
	if err := s.acquire(ctx); err != nil {
		return 0, err
	}
	defer s.release()
	if err := s.refuseIfReplaced(ctx); err != nil {
		return 0, err
	}
	if s.conn == nil {
		if err := s.reopenAfterClose(ctx, "write"); err != nil {
			return 0, presentable(err)
		}
	}
	s.checkpointLogged(ctx, "PASSIVE", "before")
	if _, err := s.conn.q().ExecContext(ctx, "VACUUM"); err != nil {
		return 0, presentable(err)
	}
	s.checkpointLogged(ctx, "TRUNCATE", "after")
	s.recordIdentityLocked(ctx)
	return optimized, nil
}

type AutoMaintenance struct {
	RetentionDays     int
	MinInterval       time.Duration
	Vacuum            bool
	SessionsDir       string
	MinVacuumInterval time.Duration
}

type AutoMaintenanceResult struct {
	Skipped              bool
	Pruned               int
	Closed               int
	Vacuumed             bool
	FreelistRatio        sql.Null[float64]
	VacuumSkippedHolders int
	Err                  error
}

func tryAutoMaintenanceLock(dbPath string) *os.File {
	lockPath := dbPath + ".auto-maintenance.lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o777); err != nil {
		slog.Warn(fmt.Sprintf("Could not open state.db auto-maintenance lock %s (%v) — skipping automatic maintenance.", lockPath, err))
		return nil
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not open state.db auto-maintenance lock %s (%v) — skipping automatic maintenance.", lockPath, err))
		return nil
	}
	if err := fileutil.TryLockFile(f, fileutil.Range(0, 1)); err != nil {
		_ = f.Close()
		return nil
	}
	return f
}

func releaseAutoMaintenanceLock(f *os.File) {
	_ = fileutil.UnlockFile(f, fileutil.Range(0, 1))
	_ = f.Close()
}

func secondsSince(now float64, raw string) (float64, bool) {
	v, ok := metaEpoch(raw)
	if !ok {
		return 0, false
	}
	return now - v, true
}

func (s *Store) MaybeAutoPruneAndVacuum(ctx context.Context, o AutoMaintenance) AutoMaintenanceResult {
	var result AutoMaintenanceResult
	if o.RetentionDays < 0 {
		slog.Warn(fmt.Sprintf("state.db auto-maintenance skipped: sessions.retention_days=%d is outside the allowed "+
			"range (a whole number of days >= 0); set sessions.auto_prune: false to disable pruning", o.RetentionDays))
		result.Skipped = true
		return result
	}
	lock := tryAutoMaintenanceLock(s.path)
	if lock == nil {
		result.Skipped = true
		return result
	}
	defer releaseAutoMaintenanceLock(lock)
	if err := s.autoPruneAndVacuum(ctx, o, &result); err != nil {
		slog.Warn(fmt.Sprintf("state.db auto-maintenance failed: %s", errText(err)))
		result.Err = err
	}
	return result
}

func (s *Store) autoPruneAndVacuum(ctx context.Context, o AutoMaintenance, result *AutoMaintenanceResult) error {
	now := nowEpoch()
	raw, _, err := s.GetMeta(ctx, "last_auto_prune")
	if err != nil {
		return err
	}
	if since, ok := secondsSince(now, raw); ok && since < o.MinInterval.Seconds() {
		result.Skipped = true
		return nil
	}
	progress := s.tables.state.options.progress
	progress(900*time.Second, "state_db_auto_prune")
	days := float64(o.RetentionDays)
	pruned, _, err := s.PruneSessions(ctx, PruneFilter{OlderThanDays: &days},
		PruneSessionsOptions{SessionsDir: o.SessionsDir, ExcludeActiveWriteGuards: true})
	if err != nil {
		return err
	}
	result.Pruned = pruned
	progress(900*time.Second, "state_db_auto_sweep")
	closed, err := s.SweepOrphanedSessions(ctx, SweepOptions{
		MaxIdle: time.Duration(days * 86400 * float64(time.Second)), Sources: autoPruneStaleOpenSources,
		ExcludePinned: true, IgnoreGatewayHeartbeats: true,
	})
	if err != nil {
		return err
	}
	result.Closed = len(closed)
	rawVacuum, _, err := s.GetMeta(ctx, "last_vacuum")
	if err != nil {
		return err
	}
	sinceVacuum, known := secondsSince(now, rawVacuum)
	vacuumDue := !known || sinceVacuum >= o.MinVacuumInterval.Seconds()
	if o.Vacuum && pruned > 0 && vacuumDue {
		ratio, ratioKnown := s.freelistRatio(ctx)
		result.FreelistRatio = sql.Null[float64]{V: ratio, Valid: ratioKnown}
		holders := append(ForeignHolders(s.path), s.eng.InProcessHolders(s.path, s)...)
		switch {
		case len(holders) > 0:
			result.VacuumSkippedHolders = len(holders)
			var named []string
			for _, h := range holders[:min(3, len(holders))] {
				named = append(named, fmt.Sprintf("%d:%s", h.PID, h.Target))
			}
			slog.Debug(fmt.Sprintf("state.db auto-maintenance: skipping VACUUM, %d other process(es) hold the "+
				"store or a WAL sidecar (%s)", len(holders), strings.Join(named, ", ")))
		case !ratioKnown || ratio > AutoVacuumMinFreelistRatio:
			progress(900*time.Second, "state_db_auto_vacuum")
			if _, err := s.Vacuum(ctx); err != nil {
				slog.Warn(fmt.Sprintf("state.db VACUUM failed: %s", errText(err)))
			} else {
				result.Vacuumed = true
				if err := s.SetMeta(ctx, "last_vacuum", epochText(now)); err != nil {
					slog.Warn(fmt.Sprintf("state.db VACUUM failed: %s", errText(err)))
				}
			}
		default:
			slog.Debug(fmt.Sprintf("state.db auto-maintenance: skipping VACUUM, only "+
				"%.1f%% of pages reclaimable (threshold %.0f%%)", ratio*100.0, AutoVacuumMinFreelistRatio*100.0))
		}
	}
	if err := s.SetMeta(ctx, "last_auto_prune", epochText(now)); err != nil {
		return err
	}
	if len(closed) > 0 || pruned > 0 {
		suffix := ""
		if result.Vacuumed {
			suffix = " + VACUUM"
		}
		slog.Info(fmt.Sprintf("state.db auto-maintenance: closed %d stale open session(s), "+
			"pruned %d session(s) inactive for %d days%s", len(closed), pruned, o.RetentionDays, suffix))
	}
	return nil
}
