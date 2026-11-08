package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	pruneTokensSQL = "(COALESCE(s.input_tokens, 0) + COALESCE(s.output_tokens, 0))"
	pruneCostSQL   = "COALESCE(s.actual_cost_usd, s.estimated_cost_usd, 0)"
)

// Bulk prune and archive selection filters, nil bounds and empty strings unset, Archived nil meaning both
type PruneFilters struct {
	Archived                                *bool
	IncludePinned, LineageTipsOnly          bool
	IncludeOpen                             bool
	LastActiveBefore, LastActiveAfter       *float64
	StartedBefore, StartedAfter             *float64
	Source, TitleLike, EndReason, CwdPrefix string
	MinMessages, MaxMessages                *float64
	ModelLike, Provider, UserID, ChatID     string
	ChatType, BranchLike                    string
	MinTokens, MaxTokens, MinCost, MaxCost  *float64
	MinToolCalls, MaxToolCalls              *float64
}

// Case insensitive substring LIKE pattern with wildcards escaped
func pruneLike(value string) string { return "%" + EscapeLike(pystr.Lower(value)) + "%" }

// Clauses and params of the cwd prefix filter, the shared prefix clause over alias s
func cwdPrefixFilter(value string) ([]string, []any) {
	clause, params := cwdPrefixClause(value)
	return []string{clause}, params
}

// Builder of a one clause filter binding the value after an optional conversion
func pruneOne(clause string, conv func(string) string) func(string) ([]string, []any) {
	return func(v string) ([]string, []any) {
		if conv != nil {
			v = conv(v)
		}
		return []string{clause}, []any{v}
	}
}

// Age of a state_meta timestamp, false when unset or corrupt
func secondsSince(now float64, raw string, ok bool) (float64, bool) {
	if !ok || raw == "" {
		return 0, false
	}
	v, err := pyint.ParseFloat(raw)
	if err != nil {
		return 0, false
	}
	return now - v, true
}

// Shared prune WHERE over alias s, only ended rows unless IncludeOpen and pinned rows spared unless opted in
func pruneFilterWhere(f PruneFilters) (string, []any) {
	clauses := []string{}
	if !f.IncludeOpen {
		clauses = append(clauses, "s.ended_at IS NOT NULL")
	}
	if f.LineageTipsOnly {
		clauses = append(clauses, "COALESCE(s.end_reason, '') <> 'compression'")
	}
	params := []any{}
	num := func(v *float64, clause string) {
		if v != nil {
			clauses = append(clauses, clause)
			params = append(params, bindNumber(*v))
		}
	}
	str := func(v, clause string, conv func(string) string) {
		if v != "" {
			c, p := pruneOne(clause, conv)(v)
			clauses = append(clauses, c...)
			params = append(params, p...)
		}
	}
	if f.LastActiveBefore != nil {
		clauses = append(clauses, sqlSessionLastActive("s")+" < ?", "(COALESCE(s.end_reason, '') != 'startup_orphan_reap' OR s.ended_at < ?)")
		params = append(params, bindNumber(*f.LastActiveBefore), bindNumber(*f.LastActiveBefore))
	}
	num(f.LastActiveAfter, sqlSessionLastActive("s")+" >= ?")
	num(f.StartedBefore, "s.started_at < ?")
	num(f.StartedAfter, "s.started_at >= ?")
	str(f.Source, "s.source = ?", nil)
	str(f.TitleLike, `LOWER(COALESCE(s.title, '')) LIKE ? ESCAPE '\'`, pruneLike)
	str(f.EndReason, "s.end_reason = ?", nil)
	if f.CwdPrefix != "" {
		c, p := cwdPrefixFilter(f.CwdPrefix)
		clauses = append(clauses, c...)
		params = append(params, p...)
	}
	num(f.MinMessages, "s.message_count >= ?")
	num(f.MaxMessages, "s.message_count <= ?")
	str(f.ModelLike, `LOWER(COALESCE(s.model, '')) LIKE ? ESCAPE '\'`, pruneLike)
	str(f.Provider, "LOWER(COALESCE(s.billing_provider, '')) = ?", pystr.Lower)
	str(f.UserID, "s.user_id = ?", nil)
	str(f.ChatID, "s.chat_id = ?", nil)
	str(f.ChatType, "s.chat_type = ?", nil)
	str(f.BranchLike, `LOWER(COALESCE(s.git_branch, '')) LIKE ? ESCAPE '\'`, pruneLike)
	num(f.MinTokens, pruneTokensSQL+" >= ?")
	num(f.MaxTokens, pruneTokensSQL+" <= ?")
	num(f.MinCost, pruneCostSQL+" >= ?")
	num(f.MaxCost, pruneCostSQL+" <= ?")
	num(f.MinToolCalls, "COALESCE(s.tool_call_count, 0) >= ?")
	num(f.MaxToolCalls, "COALESCE(s.tool_call_count, 0) <= ?")
	if f.Archived != nil {
		flag := 0
		if *f.Archived {
			flag = 1
		}
		clauses = append(clauses, fmt.Sprintf("s.archived = %d", flag))
	}
	if !f.IncludePinned {
		clauses = append(clauses, notPinnedSQL("s"))
	}
	return strings.Join(clauses, " AND "), params
}

// Whole numbers bind as integers and the rest as floats, as Python ints and floats bind
func bindNumber(v float64) any {
	if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
		return int64(v)
	}
	return v
}

// Compression ancestors of every row the candidate WHERE over alias s does not select
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

// Legacy age window translated into the activity bound, whole lineages keeping ancestors of unmatched continuations
func pruneWhere(olderThanDays *float64, source string, f PruneFilters, wholeLineages bool) (string, []any, error) {
	if olderThanDays != nil && f.LastActiveBefore == nil && f.StartedBefore == nil {
		if *olderThanDays < 0 {
			repr, _ := pyReprFloat(*olderThanDays)
			return "", nil, pyValueError("older_than_days must be >= 0, got " + repr + ": a negative " +
				"retention builds a future cutoff that matches every ended session.")
		}
		cutoff := now() - *olderThanDays*86400
		f.LastActiveBefore = &cutoff
	}
	f.Source = source
	where, params := pruneFilterWhere(f)
	if !wholeLineages {
		return where, params, nil
	}
	return where + " AND s.id NOT IN (" + continuedAncestorsSQL(where) + ")", append(slices.Clone(params), params...), nil
}

// Repr of a retention bound as Python prints a float or a whole number
func pyReprFloat(v float64) (string, error) {
	if n, ok := bindNumber(v).(int64); ok {
		return strconv.FormatInt(n, 10), nil
	}
	return pyFloatRepr(v), nil
}

// Dry run of the sessions a matching prune or archive would touch, least recently active first
func (d *SessionDB) ListPruneCandidates(ctx context.Context, olderThanDays *float64, source string, wholeLineages bool, f PruneFilters) ([]*pyjson.Object, error) {
	where, params, err := pruneWhere(olderThanDays, source, f, wholeLineages)
	if err != nil {
		return nil, err
	}
	return d.readAll(ctx, `SELECT s.id, s.source, s.title, s.model, s.started_at,
                           `+sqlSessionLastActive("s")+` AS last_active,
                           s.ended_at, s.message_count, s.archived
                    FROM sessions s WHERE `+where+`
                    ORDER BY last_active ASC, s.started_at ASC`, params...)
}

// Count of prune matches, pinnedOnly counting the rows that carry the pin itself
func (d *SessionDB) CountPruneMatches(ctx context.Context, olderThanDays *float64, source string, pinnedOnly bool, f PruneFilters) (int, error) {
	if pinnedOnly {
		f.IncludePinned = true
	}
	where, params, err := pruneWhere(olderThanDays, source, f, false)
	if err != nil {
		return 0, err
	}
	if pinnedOnly {
		where += " AND COALESCE(s.pinned, 0) = 1"
	}
	return d.countOne(ctx, "SELECT COUNT(*) FROM sessions s WHERE "+where, params)
}

// Count of open sessions a matching prune or archive skips, zero when the selection includes open rows
func (d *SessionDB) CountOpenPruneMatches(ctx context.Context, olderThanDays *float64, source string, f PruneFilters) (int, error) {
	if f.IncludeOpen {
		return 0, nil
	}
	where, params, err := pruneWhere(olderThanDays, source, f, false)
	if err != nil {
		return 0, err
	}
	const endedGuard = "s.ended_at IS NOT NULL"
	if !strings.HasPrefix(where, endedGuard) {
		return 0, &pyint.Error{Kind: "RuntimeError", Msg: "prune filter lost its ended-session safety guard"}
	}
	return d.countOne(ctx, "SELECT COUNT(*) FROM sessions s WHERE s.ended_at IS NULL"+where[len(endedGuard):], params)
}

// First column of a single count row as an int
func (d *SessionDB) countOne(ctx context.Context, query string, params []any) (int, error) {
	row, err := d.readOne(ctx, query, params...)
	if err != nil || row == nil || len(row.Keys) == 0 {
		return 0, err
	}
	v, _ := row.Get(row.Keys[0])
	n, err := pyint.Float(v)
	return int(n), err
}

// Removes empty ended TUI ghost sessions older than a day with their transcript files
func (d *SessionDB) PruneEmptyGhostSessions(ctx context.Context, sessionsDir string) (int, error) {
	cutoff := now() - 86400
	var removed []string
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		removed = nil
		rows, err := queryObjects(ctx, conn, `
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
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, pyStr(rowValue(r, "id")))
		}
		if ids, err = retirePrunable(ctx, conn, ids); err != nil {
			return err
		}
		for _, chunk := range idChunks(ids, sqlInChunk) {
			if _, err := conn.ExecContext(ctx, "DELETE FROM sessions WHERE id IN ("+placeholders(len(chunk))+")", stringsToAny(chunk)...); err != nil {
				return err
			}
		}
		if len(ids) > 0 {
			if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
				return err
			}
		}
		removed = ids
		return nil
	}, nil)
	if err != nil {
		return 0, err
	}
	d.EmitSessionDeleted(ctx, removed, "ghost_prune", "", sessionsDir)
	if err := d.CollectRetiredMedia(ctx); err != nil {
		return 0, err
	}
	for _, sid := range removed {
		removeSessionFiles(sessionsDir, sid)
	}
	return len(removed), nil
}

// Outcome of one auto archive pass
type AutoArchiveResult struct {
	Skipped  bool
	Archived int
	Error    string
}

// Idempotent idle archive gated by the last_auto_archive meta stamp, never failing
func (d *SessionDB) MaybeAutoArchive(ctx context.Context, idleDays, minIntervalHours float64, excludePinned bool) AutoArchiveResult {
	result := AutoArchiveResult{}
	err := func() error {
		at := now()
		raw, ok, err := d.GetMeta(ctx, "last_auto_archive")
		if err != nil {
			return err
		}
		last := 0.0
		if ok && raw != "" {
			if v, perr := pyint.ParseFloat(raw); perr == nil {
				last = v
			}
		}
		if last != 0 && at-last < minIntervalHours*3600 {
			result.Skipped = true
			return nil
		}
		ReportStartupProgress(900.0, "state_db_auto_archive")
		if result.Archived, err = d.ArchiveStaleSessions(ctx, idleDays, excludePinned); err != nil {
			return err
		}
		if err := d.SetMeta(ctx, "last_auto_archive", pyFloatRepr(at)); err != nil {
			return err
		}
		if result.Archived > 0 {
			slog.Info(fmt.Sprintf("state.db auto-archive: archived %d session(s) idle >= %s days", result.Archived, pyFloatRepr(idleDays)))
		}
		return nil
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("state.db auto-archive failed: %s", err))
	}
	return result
}

// Options of a prune pass, a nil OlderThanDays meaning no implicit age bound
type PruneSessionsOptions struct {
	OlderThanDays            *float64
	Source                   string
	SessionsDir              string
	ExcludeActiveWriteGuards bool
	DeletedIDs               *[]string
	Filters                  PruneFilters
}

// Prune options with the 90 day retention default
func NewPruneSessionsOptions() PruneSessionsOptions {
	days := 90.0
	return PruneSessionsOptions{OlderThanDays: &days}
}

// Deletes ended sessions matching the filters with whole lineages, orphaning children and reclaiming expired tombstones
func (d *SessionDB) PruneSessions(ctx context.Context, o PruneSessionsOptions) (int, error) {
	if o.Filters.IncludeOpen {
		return 0, pyValueError("prune_sessions() never deletes open sessions: include_open is archive-only")
	}
	where, whereParams, err := pruneWhere(o.OlderThanDays, o.Source, o.Filters, true)
	if err != nil {
		return 0, err
	}
	var removed []string
	count := 0
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		removed, count = nil, 0
		rows, err := queryObjects(ctx, conn, "SELECT s.id FROM sessions s WHERE "+where, whereParams...)
		if err != nil {
			return err
		}
		set := map[string]bool{}
		for _, r := range rows {
			set[pyStr(rowValue(r, "id"))] = true
		}
		if o.ExcludeActiveWriteGuards {
			ids := make([]string, 0, len(set))
			for id := range set {
				ids = append(ids, id)
			}
			guarded, err := d.guardedIDs(ctx, conn, ids)
			if err != nil {
				return err
			}
			for id := range guarded {
				delete(set, id)
			}
		}
		sorted := make([]string, 0, len(set))
		for id := range set {
			sorted = append(sorted, id)
		}
		slices.Sort(sorted)
		ids, err := retirePrunable(ctx, conn, sorted)
		if err != nil {
			return err
		}
		if err := pruneRetiredTombstones(ctx, conn, retryHorizonS); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, chunk := range idChunks(ids, sqlInChunk) {
			ph, args := placeholders(len(chunk)), stringsToAny(chunk)
			for _, q := range []string{"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IN (" + ph + ")",
				"DELETE FROM messages WHERE session_id IN (" + ph + ")", "DELETE FROM sessions WHERE id IN (" + ph + ")"} {
				if _, err := conn.ExecContext(ctx, q, args...); err != nil {
					return err
				}
			}
			removed = append(removed, chunk...)
		}
		count = len(ids)
		return deleteUnreferencedSystemPrompts(ctx, conn)
	}, nil)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		d.EmitSessionDeleted(ctx, removed, "prune", "", o.SessionsDir)
	}
	if err := d.CollectRetiredMedia(ctx); err != nil {
		return 0, err
	}
	for _, sid := range removed {
		removeSessionFiles(o.SessionsDir, sid)
	}
	if o.DeletedIDs != nil {
		*o.DeletedIDs = append(*o.DeletedIDs, removed...)
	}
	return count, nil
}

// Integer pragmas over the existing connection, nil with a debug log on failure
func (d *SessionDB) pagePragmas(ctx context.Context, names []string, failMsg string) []int64 {
	var out []int64
	err := d.readCtx(ctx, true, func(conn SQLiteConn) error {
		if !d.hasConn.Load() {
			return errNoPragmaConn
		}
		for _, name := range names {
			var v int64
			if err := conn.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	if errors.Is(err, errNoPragmaConn) {
		return nil
	}
	if err != nil {
		slog.Debug(strings.Replace(failMsg, "%s", err.Error(), 1))
		return nil
	}
	return out
}

var errNoPragmaConn = errors.New("closed writer connection")

// Page count times page size, the main file size once the WAL is checkpointed in
func (d *SessionDB) LogicalSizeBytes(ctx context.Context) (int64, bool) {
	v := d.pagePragmas(ctx, []string{"page_count", "page_size"}, "Could not read logical DB size: %s")
	if v == nil {
		return 0, false
	}
	return v[0] * v[1], true
}

// Reclaimable page fraction gating VACUUM, false when the pragmas cannot be read
func (d *SessionDB) FreelistRatio(ctx context.Context) (float64, bool) {
	v := d.pagePragmas(ctx, []string{"page_count", "freelist_count"}, "Could not read freelist ratio: %s")
	if v == nil {
		return 0, false
	}
	if v[0] > 0 {
		return float64(v[1]) / float64(v[0]), true
	}
	return 0.0, true
}

// WAL checkpoint in the given mode on the writer, logging a failure at debug, caller holds mu
func (d *SessionDB) tryCheckpointLocked(ctx context.Context, mode, failMsg string) {
	if _, err := d.conn.ExecContext(ctx, "PRAGMA wal_checkpoint("+mode+")"); err != nil {
		slog.Debug(strings.Replace(failMsg, "%s", err.Error(), 1))
	}
}

// Merges FTS segments then rewrites the file and truncates the WAL, refusing a quarantined handle first
func (d *SessionDB) Vacuum(ctx context.Context) (int, error) {
	if err := d.raiseIfDBCorrupt(true); err != nil {
		return 0, err
	}
	optimized, err := d.OptimizeFTS(ctx)
	if err != nil {
		slog.Warn(fmt.Sprintf("FTS optimize before VACUUM failed: %s", err))
		optimized = 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.raiseIfDBReplaced(); err != nil {
		return 0, err
	}
	if d.conn == nil {
		if err := d.reopenAfterCloseLocked("write"); err != nil {
			return 0, err
		}
	}
	d.tryCheckpointLocked(ctx, "PASSIVE", "WAL checkpoint (PASSIVE) before VACUUM failed: %s")
	if _, err := d.conn.ExecContext(ctx, "VACUUM"); err != nil {
		return 0, err
	}
	d.tryCheckpointLocked(ctx, "TRUNCATE", "WAL checkpoint (TRUNCATE) after VACUUM failed: %s")
	if err := d.recordDBFileIdentity(ctx); err != nil {
		return 0, err
	}
	return optimized, nil
}

// Startup auto maintenance knobs, built with the hermes defaults by NewAutoPruneOptions
type AutoPruneOptions struct {
	RetentionDays          *float64
	MinIntervalHours       float64
	Vacuum                 bool
	SessionsDir            string
	MinVacuumIntervalDays  float64
	MinVacuumFreelistRatio float64
}

// Ninety day retention, a daily run, vacuum on a thirty day throttle above the default freelist ratio
func NewAutoPruneOptions() AutoPruneOptions {
	days := 90.0
	return AutoPruneOptions{RetentionDays: &days, MinIntervalHours: 24, Vacuum: true, MinVacuumIntervalDays: 30,
		MinVacuumFreelistRatio: AutoVacuumMinFreelistRatio}
}

// Outcome of one auto maintenance pass, the optional fields set only when hermes reports them
type AutoPruneResult struct {
	Skipped              bool
	Pruned               int
	Closed               int
	Vacuumed             bool
	FreelistConsidered   bool
	FreelistRatio        *float64
	VacuumSkippedHolders *int
	Error                *string
}

// Prunes inactive sessions, closes stale open state owned rows and vacuums when due, never failing startup
func (d *SessionDB) MaybeAutoPruneAndVacuum(ctx context.Context, o AutoPruneOptions) AutoPruneResult {
	result := AutoPruneResult{}
	if o.RetentionDays == nil || *o.RetentionDays < 0 {
		shown := "None"
		if o.RetentionDays != nil {
			shown, _ = pyReprFloat(*o.RetentionDays)
		}
		slog.Warn(fmt.Sprintf("state.db auto-maintenance skipped: sessions.retention_days=%s is outside the allowed "+
			"range (a whole number of days >= 0); set sessions.auto_prune: false to disable pruning", shown))
		result.Skipped = true
		return result
	}
	retention := *o.RetentionDays
	lock := tryAcquireAutoMaintenanceLock(d.path)
	if lock == nil {
		result.Skipped = true
		return result
	}
	defer releaseAutoMaintenanceLock(lock)
	err := func() error {
		at := now()
		raw, ok, err := d.GetMeta(ctx, "last_auto_prune")
		if err != nil {
			return err
		}
		if since, has := secondsSince(at, raw, ok); has && since < o.MinIntervalHours*3600 {
			result.Skipped = true
			return nil
		}
		ReportStartupProgress(900.0, "state_db_auto_prune")
		po := PruneSessionsOptions{OlderThanDays: &retention, SessionsDir: o.SessionsDir, ExcludeActiveWriteGuards: true}
		if result.Pruned, err = d.PruneSessions(ctx, po); err != nil {
			return err
		}
		ReportStartupProgress(900.0, "state_db_auto_sweep")
		so := NewSweepOrphanedOptions(retention * 86400.0)
		so.Sources, so.ExcludePinned, so.RespectGatewayHeartbeats = autoPruneStaleOpenSources, true, false
		closed, err := d.SweepOrphanedSessions(ctx, so)
		if err != nil {
			return err
		}
		result.Closed = len(closed)
		rawVacuum, okVacuum, err := d.GetMeta(ctx, "last_vacuum")
		if err != nil {
			return err
		}
		sinceVacuum, hasVacuum := secondsSince(at, rawVacuum, okVacuum)
		vacuumDue := !hasVacuum || sinceVacuum >= o.MinVacuumIntervalDays*86400
		if o.Vacuum && result.Pruned > 0 && vacuumDue {
			result.FreelistConsidered = true
			ratio, hasRatio := d.FreelistRatio(ctx)
			if hasRatio {
				result.FreelistRatio = &ratio
			}
			holders, err := ForeignStateDBHolders(d.path)
			if err != nil {
				return err
			}
			holders = append(holders, InProcessStateDBHolders(d.path, d)...)
			switch {
			case len(holders) > 0:
				n := len(holders)
				result.VacuumSkippedHolders = &n
				shown := make([]string, 0, 3)
				for _, h := range holders[:min(3, n)] {
					shown = append(shown, fmt.Sprintf("%d:%s", h.PID, h.Target))
				}
				slog.Debug(fmt.Sprintf("state.db auto-maintenance: skipping VACUUM, %d other process(es) hold the "+
					"store or a WAL sidecar (%s)", n, strings.Join(shown, ", ")))
			case !hasRatio || ratio > o.MinVacuumFreelistRatio:
				ReportStartupProgress(900.0, "state_db_auto_vacuum")
				if _, verr := d.Vacuum(ctx); verr != nil {
					slog.Warn(fmt.Sprintf("state.db VACUUM failed: %s", verr))
				} else {
					result.Vacuumed = true
					if err := d.SetMeta(ctx, "last_vacuum", pyFloatRepr(at)); err != nil {
						slog.Warn(fmt.Sprintf("state.db VACUUM failed: %s", err))
					}
				}
			default:
				slog.Debug(fmt.Sprintf("state.db auto-maintenance: skipping VACUUM, only "+
					"%.1f%% of pages reclaimable (threshold %.0f%%)", ratio*100.0, o.MinVacuumFreelistRatio*100.0))
			}
		}
		if err := d.SetMeta(ctx, "last_auto_prune", pyFloatRepr(at)); err != nil {
			return err
		}
		if len(closed) > 0 || result.Pruned > 0 {
			suffix := ""
			if result.Vacuumed {
				suffix = " + VACUUM"
			}
			slog.Info(fmt.Sprintf("state.db auto-maintenance: closed %d stale open session(s), "+
				"pruned %d session(s) inactive for %d days%s", len(closed), result.Pruned, int64(retention), suffix))
		}
		return nil
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("state.db auto-maintenance failed: %s", err))
		msg := err.Error()
		result.Error = &msg
	}
	return result
}
