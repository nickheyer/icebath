package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	sqliteconfig "github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Share of pages a database must have free before automatic cleanup compacts it
const vacuumFreelist = 0.25

// Sources whose sessions left open are ended by automatic cleanup once older than the keep time
var staleOpenSources = []string{"cli", "tui", "cron", "kanban", "acp", "api_server", "subagent", "tool", "recovered"}

// Sources whose sessions a process that died left open are closed when the server starts
var orphanSources = []string{"tui", "subagent", "unknown"}

// Sessions open in the pinned head of a compression chain, which its pin covers too, over aliases c and p
const pinnedTail = `WITH RECURSIVE tail(id) AS (
	SELECT c.id FROM sessions c JOIN sessions p ON p.id = c.parent_session_id WHERE p.pinned = 1 AND ` + continuation + `
	UNION SELECT c.id FROM tail JOIN sessions p ON p.id = tail.id JOIN sessions c ON c.parent_session_id = p.id WHERE ` + continuation + `
) SELECT id FROM tail`

// Selection of a prune or archive over alias s: ended sessions unless open ones are asked for, archived ones only when asked, pinned ones
// never unless asked, the conditions of the filter
func (s *store) selection(f *sessionsv1.Filter, archive bool) ([]string, []any) {
	where, args := s.where(f, true)
	if !f.GetIncludeOpen() {
		where = append(where, "s.ended_at IS NOT NULL")
	}
	if !f.GetIncludeArchived() {
		where = append(where, "s.archived = 0")
	}
	if !f.GetIncludePinned() {
		where = append(where, "s.pinned = 0 AND s.id NOT IN ("+pinnedTail+")")
	}
	if archive {
		where = append(where, "COALESCE(s.end_reason, '') <> 'compression'")
	}
	return where, args
}

// Sessions of a selection, least recently active first
func (s *store) candidates(ctx context.Context, q querier, where []string, args []any) ([]*sessionsv1.Session, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+columns+` FROM sessions s`+whereSQL(where)+` ORDER BY COALESCE(s.last_activity_at, s.started_at), s.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*sessionsv1.Session
	for rows.Next() {
		session, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// How many sessions the filter matches that a rule spares: those still open and those pinned
func (s *store) spared(ctx context.Context, q querier, f *sessionsv1.Filter, archive bool) (int32, int32, error) {
	count := func(change func(f *sessionsv1.Filter)) (int32, error) {
		copied := cloneFilter(f)
		change(copied)
		where, args := s.selection(copied, archive)
		var n int32
		err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions s`+whereSQL(where), args...).Scan(&n)
		return n, err
	}
	total, err := count(func(*sessionsv1.Filter) {})
	if err != nil {
		return 0, 0, err
	}
	var open, pinned int32
	if !f.GetIncludeOpen() {
		all, err := count(func(f *sessionsv1.Filter) { f.IncludeOpen = true })
		if err != nil {
			return 0, 0, err
		}
		open = all - total
	}
	if !f.GetIncludePinned() {
		all, err := count(func(f *sessionsv1.Filter) { f.IncludePinned = true })
		if err != nil {
			return 0, 0, err
		}
		pinned = all - total
	}
	return open, pinned, nil
}

func cloneFilter(f *sessionsv1.Filter) *sessionsv1.Filter {
	if f == nil {
		return &sessionsv1.Filter{}
	}
	copied := *f
	return &copied
}

func (s *store) Prune(ctx context.Context, request *sessionsv1.PruneRequest) (*sessionsv1.PruneResponse, error) {
	f, dryRun := request.GetFilter(), request.GetDryRun()
	if f.GetIncludeOpen() {
		return nil, errors.New("prune never deletes open sessions: include open is for archive only")
	}
	if f.GetNeverActive() {
		return s.pruneNeverActive(ctx, f, dryRun)
	}
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	out := &sessionsv1.PruneResponse{}
	if out.Open, out.Pinned, err = s.spared(ctx, db, f, false); err != nil {
		return nil, err
	}
	where, args := s.selection(f, false)
	// A chain is deleted whole or not at all: an ancestor a kept continuation still holds is kept
	where = append(where, "s.id NOT IN (WITH RECURSIVE kept(id) AS (SELECT p.id FROM sessions c JOIN sessions p ON p.id = c.parent_session_id WHERE "+
		continuation+" AND c.id NOT IN (SELECT s.id FROM sessions s"+whereSQL(where)+") UNION SELECT p.id FROM kept JOIN sessions c ON c.id = kept.id "+
		"JOIN sessions p ON p.id = c.parent_session_id WHERE "+continuation+") SELECT id FROM kept)")
	args = append(args, args...)
	if out.Sessions, err = s.candidates(ctx, db, where, args); err != nil {
		return nil, err
	}
	if dryRun || len(out.GetSessions()) == 0 {
		return out, nil
	}
	if refused := refusal("prune", s.path, foreignHolders(s.path), request.GetForce()); refused != "" {
		return nil, errors.New(refused)
	}
	var removed []string
	err = s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT s.id FROM sessions s`+whereSQL(where), args...)
		if err != nil {
			return err
		}
		ids, err := collect(rows)
		if err != nil {
			return err
		}
		held, err := guarded(ctx, tx, ids)
		if err != nil {
			return err
		}
		removed = slices.DeleteFunc(ids, func(id string) bool { return slices.Contains(held, id) })
		out.Busy = int32(len(held))
		return deleteRows(ctx, tx, removed)
	})
	if err != nil {
		return nil, err
	}
	out.Deleted = int32(len(removed))
	s.changed(removed...)
	s.deleted(ctx, out.GetSessions(), removed, "prune", "")
	return out, nil
}

// Deletes keyed gateway sessions opened and never used, older than the filter's activity bound, with the routing entries pointing at them
func (s *store) pruneNeverActive(ctx context.Context, f *sessionsv1.Filter, dryRun bool) (*sessionsv1.PruneResponse, error) {
	age := 30 * 24 * time.Hour
	if f.GetActiveBefore() != nil {
		age = time.Since(f.GetActiveBefore().AsTime())
	}
	if age < 0 {
		return nil, errors.New("the age of never-active sessions must be at least 0: a negative one matches every never-active keyed row.")
	}
	idle, err := s.NeverActive(ctx, age)
	if err != nil {
		return nil, err
	}
	out := &sessionsv1.PruneResponse{Sessions: idle}
	if dryRun || len(idle) == 0 {
		return out, nil
	}
	var removed []string
	err = s.write(ctx, func(tx *sql.Tx) error {
		ids := make([]string, len(idle))
		for i, session := range idle {
			ids[i] = session.GetId()
		}
		held, err := guarded(ctx, tx, ids)
		if err != nil {
			return err
		}
		removed = slices.DeleteFunc(ids, func(id string) bool { return slices.Contains(held, id) })
		out.Busy = int32(len(held))
		if err := deleteRows(ctx, tx, removed); err != nil {
			return err
		}
		for _, chunk := range chunks(removed) {
			result, err := tx.ExecContext(ctx, `DELETE FROM gateway_routing WHERE json_valid(entry_json) AND json_extract(entry_json, '$.session_id') IN (`+
				marks(len(chunk))+`)`, strings2any(chunk)...)
			if err != nil {
				return err
			}
			n, _ := result.RowsAffected()
			out.Routes += int32(n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Deleted = int32(len(removed))
	s.changed(removed...)
	s.deleted(ctx, idle, removed, "empty_sweep", "")
	return out, nil
}

func (s *store) Archive(ctx context.Context, f *sessionsv1.Filter, dryRun bool) (*sessionsv1.ArchiveResponse, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	f = cloneFilter(f)
	f.IncludeArchived, f.IncludePinned = false, false
	out := &sessionsv1.ArchiveResponse{}
	if out.Open, out.Pinned, err = s.spared(ctx, db, f, true); err != nil {
		return nil, err
	}
	where, args := s.selection(f, true)
	if out.Sessions, err = s.candidates(ctx, db, where, args); err != nil {
		return nil, err
	}
	if dryRun {
		return out, nil
	}
	var ids []string
	for _, session := range out.GetSessions() {
		if err := s.archive(ctx, session.GetId(), false); err != nil {
			s.deleted(ctx, out.GetSessions(), ids, "archive", "")
			return nil, err
		}
		ids = append(ids, session.GetId())
	}
	out.Archived = int32(len(ids))
	s.deleted(ctx, out.GetSessions(), ids, "archive", "")
	return out, nil
}

// Archives the session with its compression chain, marked as done by the idle sweep when automatic
func (s *store) archive(ctx context.Context, id string, automatic bool) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		chain, err := lineage(ctx, tx, id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET archived = 1, auto_archived = ? WHERE id IN (`+marks(len(chain))+`)`,
			append([]any{boolInt(automatic)}, strings2any(chain)...)...)
		return err
	})
	if err == nil {
		s.changed(id)
	}
	return err
}

func (s *store) Stats(ctx context.Context) (*sessionsv1.StatsResponse, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	out := &sessionsv1.StatsResponse{Path: s.path, JournalMode: s.mode}
	if err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM sessions), (SELECT COUNT(*) FROM messages)`).Scan(&out.Sessions, &out.Messages); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT source, COUNT(*) FROM sessions GROUP BY source ORDER BY COUNT(*) DESC, source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		count := &sessionsv1.SourceCount{}
		if err := rows.Scan(&count.Source, &count.Sessions); err != nil {
			return nil, err
		}
		out.Sources = append(out.Sources, count)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, file := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if info, err := os.Stat(file); err == nil {
			out.Bytes += info.Size()
		}
	}
	if out.Indexing, err = s.Indexing(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// Cleanup a long-lived server runs as it starts: archiving idle sessions, deleting old ones and compacting the file, each when its setting
// asks and its spacing allows, then closing sessions a process that died left open
func (s *store) Start(ctx context.Context) {
	s.mu.Lock()
	s.background = ctx
	s.mu.Unlock()
	go s.mend(ctx)
	go func() {
		if err := s.housekeeping(ctx); err != nil {
			slog.Warn("Session database housekeeping failed", "error", err)
		}
		s.notice(ctx)
	}()
	grace := time.Duration(s.config.GetInt(sessionsconfig.OrphanReapGrace)) * time.Second
	if !s.config.GetBool(sessionsconfig.StartupOrphanSweep) || grace <= 0 {
		return
	}
	ttl := 6 * time.Hour
	if text := os.Getenv("ICEBATH_TUI_SESSION_TTL_S"); text != "" {
		if value, err := strconv.ParseFloat(text, 64); err == nil {
			ttl = time.Duration(value * float64(time.Second))
		}
	}
	if ttl <= 0 {
		return
	}
	time.AfterFunc(grace, func() {
		swept, err := s.SweepOrphans(ctx, orphanSources, ttl)
		switch {
		case err != nil:
			slog.Warn("Orphaned session row sweep failed", "error", err)
		case len(swept) > 0:
			slog.Info(fmt.Sprintf("Closed %d orphaned session row(s) from a previous process (startup_orphan_reap): %s", len(swept), strings.Join(swept, ", ")))
		}
	})
}

func (s *store) housekeeping(ctx context.Context) error {
	interval := time.Duration(s.config.GetInt(sessionsconfig.MinIntervalHours)) * time.Hour
	if s.config.GetBool(sessionsconfig.AutoArchive) {
		if err := s.autoArchive(ctx, float64(s.config.GetInt(sessionsconfig.AutoArchiveDays)), interval); err != nil {
			slog.Warn("Session auto-archive failed", "error", err)
		}
	}
	if !s.config.GetBool(sessionsconfig.AutoPrune) {
		return nil
	}
	days := s.config.GetInt(sessionsconfig.RetentionDays)
	if days < 0 {
		slog.Warn(fmt.Sprintf("Session auto-maintenance skipped: sessions.retention_days=%d is outside the allowed range (a whole number of days >= 0); "+
			"turn Delete old sessions off to disable pruning", days))
		return nil
	}
	return withFlock(s.config.GetString(sqliteconfig.Path)+".maintenance.lock", 0, func(locked bool) error {
		if !locked {
			return nil
		}
		return s.autoPrune(ctx, days, interval)
	})
}

// Archives sessions idle for the days, once per interval
func (s *store) autoArchive(ctx context.Context, days float64, interval time.Duration) error {
	db, err := s.read(ctx)
	if err != nil {
		return err
	}
	at := now()
	if last, ok, err := meta(ctx, db, "last_auto_archive"); err != nil {
		return err
	} else if since, valid := elapsed(at, last, ok); valid && since < interval.Seconds() {
		return nil
	}
	if days < 0 {
		return nil
	}
	idle, err := s.candidates(ctx, db, []string{`s.archived = 0 AND COALESCE(s.end_reason, '') <> 'compression' AND s.pinned = 0 AND s.id NOT IN (` +
		pinnedTail + `) AND NOT (s.hidden <> 0 AND COALESCE(s.title, '') = ?) AND COALESCE(s.last_activity_at, s.started_at) < ?`},
		[]any{sessions.BotChat, at - days*86400})
	if err != nil {
		return err
	}
	var ids []string
	for _, session := range idle {
		if err := s.archive(ctx, session.GetId(), true); err != nil {
			s.deleted(ctx, idle, ids, "archive", "")
			return err
		}
		ids = append(ids, session.GetId())
	}
	s.deleted(ctx, idle, ids, "archive", "")
	if err := s.write(ctx, func(tx *sql.Tx) error {
		return setMeta(ctx, tx, "last_auto_archive", strconv.FormatFloat(at, 'f', -1, 64))
	}); err != nil {
		return err
	}
	if len(ids) > 0 {
		slog.Info(fmt.Sprintf("Session auto-archive: archived %d session(s) idle >= %g days", len(ids), days))
	}
	return nil
}

// Deletes sessions inactive past the keep time, ends stale open ones and compacts the file when enough of it is free, once per interval
func (s *store) autoPrune(ctx context.Context, days int, interval time.Duration) error {
	db, err := s.read(ctx)
	if err != nil {
		return err
	}
	at := now()
	if last, ok, err := meta(ctx, db, "last_auto_prune"); err != nil {
		return err
	} else if since, valid := elapsed(at, last, ok); valid && since < interval.Seconds() {
		return nil
	}
	cutoff := fromSeconds(at - float64(days)*86400)
	result, err := s.Prune(ctx, &sessionsv1.PruneRequest{Filter: &sessionsv1.Filter{ActiveBefore: timestampOf(cutoff)}, Force: true})
	if err != nil {
		return err
	}
	closed, err := s.SweepOrphans(ctx, staleOpenSources, time.Duration(days)*24*time.Hour)
	if err != nil {
		return err
	}
	vacuumed := false
	last, ok, err := meta(ctx, db, "last_vacuum")
	if err != nil {
		return err
	}
	since, valid := elapsed(at, last, ok)
	due := !valid || since >= float64(s.config.GetInt(sessionsconfig.MinVacuumIntervalDays))*86400
	if s.config.GetBool(sessionsconfig.VacuumAfterPrune) && result.GetDeleted() > 0 && due {
		ratio, known := s.freelist(ctx, db)
		switch holders := foreignHolders(s.path); {
		case len(holders) > 0:
			slog.Debug(fmt.Sprintf("Session auto-maintenance: skipping VACUUM, %d other process(es) hold the store or a WAL sidecar", len(holders)))
		case !known || ratio > vacuumFreelist:
			if err := s.vacuum(ctx); err != nil {
				slog.Warn("Session database VACUUM failed", "error", err)
			} else {
				vacuumed = true
				if err := s.write(ctx, func(tx *sql.Tx) error { return setMeta(ctx, tx, "last_vacuum", strconv.FormatFloat(at, 'f', -1, 64)) }); err != nil {
					slog.Warn("Session database VACUUM failed", "error", err)
				}
			}
		default:
			slog.Debug(fmt.Sprintf("Session auto-maintenance: skipping VACUUM, only %.1f%% of pages reclaimable (threshold %.0f%%)", ratio*100, vacuumFreelist*100))
		}
	}
	if err := s.write(ctx, func(tx *sql.Tx) error {
		return setMeta(ctx, tx, "last_auto_prune", strconv.FormatFloat(at, 'f', -1, 64))
	}); err != nil {
		return err
	}
	if len(closed) > 0 || result.GetDeleted() > 0 {
		suffix := ""
		if vacuumed {
			suffix = " + VACUUM"
		}
		slog.Info(fmt.Sprintf("Session auto-maintenance: closed %d stale open session(s), pruned %d session(s) inactive for %d days%s", len(closed),
			result.GetDeleted(), days, suffix))
	}
	return nil
}

// Seconds since a stored time, false when unset or unreadable
func elapsed(at float64, raw string, ok bool) (float64, bool) {
	if !ok || raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, false
	}
	return at - value, true
}

// Share of the file's pages that are free, false when it can't be read
func (s *store) freelist(ctx context.Context, db *sql.DB) (float64, bool) {
	var pages, free int64
	if db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages) != nil || db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free) != nil {
		return 0, false
	}
	if pages == 0 {
		return 0, true
	}
	return float64(free) / float64(pages), true
}

// Merges the search indexes and rewrites the file, the write-ahead log checkpointed before and truncated after
func (s *store) vacuum(ctx context.Context) error {
	if err := s.health(); err != nil {
		return err
	}
	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	s.optimizeIndexes(ctx, db)
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		slog.Debug("WAL checkpoint (PASSIVE) before VACUUM failed", "error", err)
	}
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		slog.Debug("WAL checkpoint (TRUNCATE) after VACUUM failed", "error", err)
	}
	s.mu.Lock()
	if info, err := os.Stat(s.path); err == nil {
		s.dev, s.ino = identity(info)
	}
	s.mu.Unlock()
	return nil
}

// Merges each search index into one segment, returning how many were merged
func (s *store) optimizeIndexes(ctx context.Context, db *sql.DB) int {
	if s.isStale(ctx, db) {
		return 0
	}
	merged := 0
	for _, table := range s.indexes() {
		err := s.once(ctx, db, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO "+table+"("+table+") VALUES ('optimize')")
			return err
		})
		if err != nil {
			slog.Warn("Search index optimize failed", "index", table, "error", err)
			continue
		}
		merged++
	}
	return merged
}

// Tells the person a search index rebuild of a large database was left unfinished, when the notice is on
func (s *store) notice(ctx context.Context) {
	if s.config.GetString(sqliteconfig.FTSOptimizeNotice) == sqliteconfig.Off {
		return
	}
	db, err := s.read(ctx)
	if err != nil {
		return
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Size() < 512<<20 {
		return
	}
	if _, _, running, err := progressOf(ctx, db, wordBackfill); err == nil && running {
		slog.Warn("Session database optimization incomplete: a previous `icebath sessions optimize-storage` run was interrupted. Search still works; " +
			"re-run the command to resume and finish reclaiming disk.")
	}
}
