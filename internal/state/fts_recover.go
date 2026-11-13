package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Deferral counts and spans that trigger the orphan reap and the futile verdict, and the retry backoff bounds
const (
	ftsHolderEscalateAttempts = 3
	ftsHolderEscalateSpan     = 60 * time.Second
	ftsHolderFutileAttempts   = 10
	ftsHolderFutileSpan       = 1800 * time.Second
	ftsStaleRetry             = 60 * time.Second
	ftsStaleRetryMax          = 3600 * time.Second
)

// Merge pages per command and commands per index of one incremental pass
const (
	ftsMergeMaxPages       = 500
	ftsMergeCommandsPerRun = 4
)

// Seconds since the epoch as a float, the stamp every diagnostic records
func epochNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Holder pids a scan proved, sorted and unique
func holderPIDs(holders []Holder) []int {
	var pids []int
	for _, h := range holders {
		if h.PID > 0 && !slices.Contains(pids, h.PID) {
			pids = append(pids, h.PID)
		}
	}
	slices.Sort(pids)
	return pids
}

// Command line of a holder for an escalation, cut to 120 characters
func holderCmdline(pid int) string {
	argv := holderArgv(pid)
	if len(argv) == 0 {
		return "<cmdline unavailable>"
	}
	return headRunes(strings.Join(argv, " "), 120)
}

// Records a deferral diagnostic for the foreign holders, true when they still block the rebuild
func (s *Store) deferStaleFTSForHolders(ctx context.Context, c Conn, holders []Holder) (bool, error) {
	now := epochNow()
	raw, _, err := metaValue(ctx, c, ftsRebuildDeferralKey)
	if err != nil && familyOf(err) == notSQLite {
		return false, err
	}
	var record map[string]any
	if err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &record)
	}
	firstSeen, attempts, holdersSince, holdersAttempts := now, 1, now, 1
	if record != nil {
		fs, ok1 := numberOr(record, "first_seen", now)
		at, ok2 := numberOr(record, "attempts", 0)
		hs, ok3 := numberOr(record, "holders_since", now)
		ha, ok4 := numberOr(record, "holders_attempts", 0)
		if ok1 && ok2 && ok3 && ok4 {
			firstSeen, attempts, holdersSince, holdersAttempts = fs, int(at)+1, hs, int(ha)+1
		}
	}
	if firstSeen > now || firstSeen < 0 {
		firstSeen = now
	}
	pids := holderPIDs(holders)
	if !samePIDs(pids, record["holder_pids"]) {
		holdersSince, holdersAttempts = now, 1
	}
	escalate := attempts >= ftsHolderEscalateAttempts && now-firstSeen >= ftsHolderEscalateSpan.Seconds()
	if escalate {
		reaped, err := s.reapInactiveOrphanDesktopHolders(ctx, holders, ftsHolderEscalateSpan)
		if err != nil {
			return false, err
		}
		if len(reaped) > 0 {
			slog.Error(fmt.Sprintf("Reaped inactive orphan Desktop backend(s) %v after %d state.db FTS rebuild deferrals; "+
				"checking holders again.", reaped, attempts))
			holders = ForeignHolders(s.path)
			pids = holderPIDs(holders)
		}
	}
	futile := len(pids) > 0 && holdersAttempts >= ftsHolderFutileAttempts && now-holdersSince >= ftsHolderFutileSpan.Seconds()
	diagnostic := map[string]any{
		"first_seen": firstSeen, "last_seen": now, "attempts": attempts, "holder_pids": pids,
		"holders_since": holdersSince, "holders_attempts": holdersAttempts, "futile": futile,
	}
	if pids == nil {
		diagnostic["holder_pids"] = []int{}
	}
	payload, err := fileutil.EncodeJSON(diagnostic, fileutil.JSONStyle{Sorted: true, ASCII: true})
	if err != nil {
		return false, err
	}
	if _, err := c.ExecContext(ctx, stateMetaUpsertSQL, ftsRebuildDeferralKey, string(payload)); err != nil {
		return false, err
	}
	if len(holders) == 0 {
		s.fts().set(func(f *ftsFlags) { f.deferredPIDs = nil })
		return false, nil
	}
	s.fts().set(func(f *ftsFlags) { f.deferredPIDs = append([]int{}, pids...) })
	switch {
	case futile:
		lines := make([]string, len(pids))
		for i, pid := range pids {
			lines[i] = fmt.Sprintf("pid %d: %s", pid, holderCmdline(pid))
		}
		slog.Error(fmt.Sprintf("state.db FTS repair has been blocked by the same holder(s) for %d deferrals over %.0f min (%s); "+
			"waiting is futile. Stop ONLY the other holder(s) — this process keeps running and its own retry admits the "+
			"rebuild within %.0fs of the holder leaving. `icebath doctor` shows this.", holdersAttempts, (now-holdersSince)/60,
			strings.Join(lines, ", "), ftsStaleRetry.Seconds()))
	case escalate:
		slog.Error(fmt.Sprintf("state.db FTS repair remains blocked after %d deferrals by holder(s) %v. Stop the listed "+
			"processes (this process's own retry then rebuilds), or run `icebath sessions optimize-storage` with every holder "+
			"stopped. `icebath doctor` reports this degraded state.", attempts, holders))
	}
	slog.Warn(fmt.Sprintf("Deferred stale state.db FTS rebuild while foreign processes hold the database or WAL sidecars (%v); "+
		"canonical writes and LIKE search remain available (deferral %d).", holders, attempts))
	return true, nil
}

// Number a decoded JSON field holds, the fallback when absent, false when present but not numeric
func numberOr(record map[string]any, key string, fallback float64) (float64, bool) {
	v, ok := record[key]
	if !ok {
		return fallback, true
	}
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case float64:
		return t, true
	case string:
		var f float64
		if _, err := fmt.Sscan(strings.TrimSpace(t), &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// Reports whether a recorded holder list equals pids
func samePIDs(pids []int, recorded any) bool {
	list, ok := recorded.([]any)
	if !ok || len(list) != len(pids) {
		return false
	}
	for i, v := range list {
		f, ok := v.(float64)
		if !ok || int(f) != pids[i] || float64(int(f)) != f {
			return false
		}
	}
	return true
}

// Rebuilds stale base and trigram indexes under admission, deferring while foreign processes hold the file
func (s *Store) recoverStaleFTS(ctx context.Context, c Conn, legacy bool, timeout time.Duration) (bool, error) {
	if holders := ForeignHolders(s.path); len(holders) > 0 {
		deferred, err := s.deferStaleFTSForHolders(ctx, c, holders)
		if err != nil || deferred {
			return false, err
		}
	}
	release, admitted := s.eng.ftsRebuildAdmission(ctx, s.path, timeout)
	if !admitted {
		slog.Warn("Deferred stale state.db FTS rebuild: another process holds the rebuild authority; canonical writes and " +
			"LIKE search remain available.")
		return false, nil
	}
	defer release()
	return s.recoverStaleFTSLocked(ctx, c, legacy)
}

// Legacy inline reinsert of table, the delete first when asked
func legacyInlineReinsertSQL(table string, indent int, deleteFirst bool) string {
	pad := strings.Repeat(" ", indent)
	body := ""
	if deleteFirst {
		body = pad + "DELETE FROM " + table + ";\n"
	}
	return "\n" + body + pad + "INSERT INTO " + table + "(rowid, content)\n" + pad + "SELECT id,\n" +
		pad + "       COALESCE(content, '') || ' ' ||\n" + pad + "       COALESCE(tool_name, '') || ' ' ||\n" +
		pad + "       COALESCE(tool_calls, '')\n" + pad + "FROM messages;\n" + pad[:len(pad)-4]
}

// One write transaction that drops and recreates the stale indexes, rebuilds them and restores the sync triggers
func (s *Store) recoverStaleFTSLocked(ctx context.Context, c Conn, legacy bool) (bool, error) {
	var includeTrigram bool
	probe, err := ftsTableProbe(ctx, c, s, "messages_fts_trigram")
	if err != nil {
		if familyOf(err) == notSQLite {
			return false, err
		}
		includeTrigram = true
	} else {
		includeTrigram = probe == probeQueryable
		if !includeTrigram && !legacy {
			if includeTrigram, err = s.trigramTokenizerAvailable(ctx, c); err != nil {
				return false, err
			}
		}
	}
	var b strings.Builder
	b.WriteString("BEGIN IMMEDIATE;")
	for _, t := range ftsTriggers {
		b.WriteString("DROP TRIGGER IF EXISTS " + t + ";")
	}
	if includeTrigram {
		b.WriteString("DROP TABLE IF EXISTS messages_fts_trigram;")
	}
	b.WriteString("DROP VIEW IF EXISTS messages_fts_trigram_src;DROP TABLE IF EXISTS messages_fts;")
	if legacy {
		b.WriteString(legacyFTSSQL)
		if includeTrigram {
			b.WriteString(legacyFTSTrigramSQL)
		}
		b.WriteString(legacyInlineReinsertSQL("messages_fts", 16, false))
		if includeTrigram {
			b.WriteString(legacyInlineReinsertSQL("messages_fts_trigram", 20, true))
		}
	} else {
		b.WriteString(ftsSQL)
		if includeTrigram {
			b.WriteString(ftsTrigramSQL)
		}
		b.WriteString("INSERT INTO messages_fts(messages_fts) VALUES('rebuild');")
		if includeTrigram {
			b.WriteString("INSERT INTO messages_fts_trigram(messages_fts_trigram) VALUES('rebuild');")
		}
		b.WriteString(clearRebuildMarkersSQL + ";" + dropRetiredToolHighWaterSQL + ";")
	}
	b.WriteString("DELETE FROM state_meta WHERE key IN ('" + ftsStaleKey + "', '" + ftsRebuildDeferralKey + "');COMMIT;")
	if _, err := c.ExecContext(ctx, b.String()); err != nil {
		if familyOf(err) == notSQLite {
			return false, err
		}
		_, _ = c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		if derr := dropAllFTSTriggers(ctx, c); derr != nil {
			return false, derr
		}
		slog.Error(fmt.Sprintf("Automatic rebuild of stale FTS indexes failed (%s); canonical writes remain enabled with FTS detached.", errText(err)))
		return false, nil
	}
	s.fts().set(func(f *ftsFlags) { f.stale, f.enabled, f.trigram = false, true, includeTrigram })
	slog.Warn("Rebuilt stale state.db FTS indexes from canonical messages and restored sync triggers.")
	return true, nil
}

// Runs fn on the writer under its lock outside a transaction, refusing a replaced file and reopening after close
func (s *Store) onWriter(ctx context.Context, fn func(c Conn) error) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if err := s.refuseIfReplaced(ctx); err != nil {
		return err
	}
	if s.conn == nil {
		if err := s.reopenAfterClose(ctx, "write"); err != nil {
			return err
		}
	}
	return fn(s.conn.q())
}

// Retries a deferred stale index rebuild from a housekeeping tick with doubling backoff, true once the index is back
func (s *Store) RetryDeferredFTSRecovery(ctx context.Context) bool {
	f := s.fts()
	if !ftsGet(f, func(f *ftsFlags) bool { return f.stale }) {
		return false
	}
	if s.quarantineReason() != "" {
		f.set(func(f *ftsFlags) { f.retryAfter, f.retryInterval = time.Time{}, 0 })
		return false
	}
	if s.readOnly || !s.hasWriter() {
		return false
	}
	now := time.Now()
	f.mu.Lock()
	after, deferred := f.retryAfter, f.deferredPIDs
	f.mu.Unlock()
	if now.Before(after) {
		if deferred == nil || slices.Equal(holderPIDs(ForeignHolders(s.path)), deferred) {
			return false
		}
		f.set(func(f *ftsFlags) { f.retryInterval = 0 })
	}
	f.mu.Lock()
	interval := f.retryInterval
	if interval <= 0 {
		interval = ftsStaleRetry
	}
	f.retryAfter = now.Add(interval)
	f.retryInterval = min(max(interval, ftsStaleRetry, time.Second)*2, ftsStaleRetryMax)
	f.mu.Unlock()
	recovered := false
	err := func() error {
		if err := s.acquire(ctx); err != nil {
			return err
		}
		defer s.release()
		if s.conn == nil || !ftsGet(f, func(f *ftsFlags) bool { return f.stale }) {
			return nil
		}
		c := s.conn.q()
		legacy, err := dbHasLegacyInlineFTS(ctx, c)
		if err != nil {
			return err
		}
		if recovered, err = s.recoverStaleFTS(ctx, c, legacy, 0); err != nil || !recovered {
			return err
		}
		if err := s.ensureFTSCJKSchema(ctx, c); err != nil {
			return err
		}
		f.set(func(f *ftsFlags) { f.retryInterval = 0 })
		return nil
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("In-process retry of the deferred stale state.db FTS rebuild failed; will retry later: %s", errText(err)))
		return false
	}
	return recovered
}

// Detaches corrupt derived indexes behind an atomic breadcrumb so canonical writes continue, busy locks waited out
func enterFTSFailOpen(ctx context.Context, s *Store, cause error, deadline time.Time, patience time.Duration) (bool, error) {
	if !ftsGet(s.fts(), func(f *ftsFlags) bool { return f.enabled }) || !IsFTSScopedCorruption(cause) {
		return false, nil
	}
	for {
		if err := s.refuseIfCorrupt(true); err != nil {
			return false, err
		}
		_, err := s.writeOnce(ctx, deadline, func(c Conn) error {
			if _, err := c.ExecContext(ctx, staleKeyUpsertSQL, ftsStaleKey); err != nil {
				return err
			}
			var one int
			err := c.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'trigger' AND name IN ("+
				bindMarks(len(ftsCJKTriggers))+") LIMIT 1", stringArgs(ftsCJKTriggers)...).Scan(&one)
			switch {
			case err == nil:
				if _, err := c.ExecContext(ctx, staleKeyUpsertSQL, ftsCJKStaleKey); err != nil {
					return err
				}
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			return dropAllFTSTriggers(ctx, c)
		})
		if err == nil {
			break
		}
		var corrupt *CorruptError
		if errors.Is(err, ErrReplaced) || errors.As(err, &corrupt) || ctx.Err() != nil || familyOf(err) == notSQLite {
			return false, err
		}
		if familyOf(err) == callFamily && isLockError(err) {
			retry, serr := sleepBeforeRetry(ctx, deadline, patience)
			if serr != nil {
				return false, serr
			}
			if retry {
				continue
			}
		}
		slog.Error(fmt.Sprintf("Could not detach corrupt FTS indexes; canonical write still cannot proceed: %s", errText(err)))
		return false, nil
	}
	s.fts().set(func(f *ftsFlags) { f.stale, f.enabled, f.trigram, f.cjkAvailable = true, false, false, false })
	slog.Error(fmt.Sprintf("state.db FTS indexes remain corrupt (%s); disabled FTS sync and retrying the canonical write. Search "+
		"temporarily uses LIKE until a later state store open rebuilds the indexes.", errText(cause)))
	return true, nil
}

// Full-text tables that answer a zero row query on c
func presentFTSTables(ctx context.Context, c Conn) []string {
	var out []string
	for _, t := range ftsTables {
		if _, err := c.ExecContext(ctx, "SELECT 1 FROM "+t+" LIMIT 0"); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// One bounded merge pass after a committed write, a failure logged and never handed back to the writer
func tryIncrementalMergeFTS(ctx context.Context, s *Store) {
	if !ftsGet(s.fts(), func(f *ftsFlags) bool { return f.enabled }) {
		return
	}
	if _, err := s.mergeFTSIncrementally(ctx, ftsMergeMaxPages, ftsMergeCommandsPerRun); err != nil {
		slog.Warn(fmt.Sprintf("FTS incremental merge failed after commit: %s", errText(err)))
	}
}

// Bounded FTS5 merge commands per present index, the usermerge floor lowered to two once, returning commands run
func (s *Store) mergeFTSIncrementally(ctx context.Context, maxPages, maxCommands int) (int, error) {
	if maxPages <= 0 || maxCommands <= 0 {
		return 0, fmt.Errorf("merge pages and commands must be greater than zero")
	}
	if err := s.acquire(ctx); err != nil {
		return 0, err
	}
	defer s.release()
	if s.conn == nil {
		return 0, fmt.Errorf("%w: %s", ErrClosed, s.path)
	}
	c := s.conn.q()
	applied := ftsGet(s.fts(), func(f *ftsFlags) bool { return f.usermergeApplied })
	executed := 0
	for _, t := range presentFTSTables(ctx, c) {
		if !applied {
			if _, err := c.ExecContext(ctx, "INSERT INTO "+t+"("+t+", rank) VALUES('usermerge', 2)"); err != nil {
				return executed, err
			}
		}
		for range maxCommands {
			before, err := totalChanges(ctx, c)
			if err != nil {
				return executed, err
			}
			if _, err := c.ExecContext(ctx, "INSERT INTO "+t+"("+t+", rank) VALUES('merge', ?)", maxPages); err != nil {
				return executed, err
			}
			executed++
			after, err := totalChanges(ctx, c)
			if err != nil {
				return executed, err
			}
			if after-before < 2 {
				break
			}
		}
	}
	s.fts().set(func(f *ftsFlags) { f.usermergeApplied = true })
	return executed, nil
}

// Rows the connection changed since it opened
func totalChanges(ctx context.Context, c Conn) (int64, error) {
	var n int64
	err := c.QueryRowContext(ctx, "SELECT total_changes()").Scan(&n)
	return n, err
}

// Merges every index into one segment with the FTS5 optimize command, returning how many it optimized
func (s *Store) OptimizeFTS(ctx context.Context) (int, error) {
	if err := s.refuseIfCorrupt(false); err != nil {
		return 0, err
	}
	optimized := 0
	err := s.onWriter(ctx, func(c Conn) error {
		for _, t := range presentFTSTables(ctx, c) {
			if _, err := c.ExecContext(ctx, "INSERT INTO "+t+"("+t+") VALUES('optimize')"); err != nil {
				if familyOf(err) == notSQLite {
					return err
				}
				slog.Warn(fmt.Sprintf("FTS optimize failed for %s: %s", t, errText(err)))
				continue
			}
			optimized++
		}
		return nil
	})
	return optimized, presentable(err)
}

// Rebuilds every index from messages under the cross-process admission, zero when another process holds it
func (s *Store) RebuildFTS(ctx context.Context) (int, error) {
	if err := s.refuseIfCorrupt(false); err != nil {
		return 0, err
	}
	release, admitted := s.eng.ftsRebuildAdmission(ctx, s.path, ftsRebuildLockTimeout)
	if !admitted {
		slog.Warn("Deferred in-place FTS rebuild: another process holds the rebuild authority for this state.db.")
		return 0, nil
	}
	defer release()
	rebuilt := 0
	err := s.onWriter(ctx, func(c Conn) error {
		for _, t := range presentFTSTables(ctx, c) {
			if _, err := c.ExecContext(ctx, "INSERT INTO "+t+"("+t+") VALUES('rebuild')"); err != nil {
				if familyOf(err) == notSQLite {
					return err
				}
				if IsMalformed(err) {
					slog.Error(fmt.Sprintf("FTS rebuild failed with a corruption-class error for %s: %s; run 'icebath sessions "+
						"repair' (or 'icebath doctor --fix') to rebuild the index offline", t, errText(err)))
				} else {
					slog.Warn(fmt.Sprintf("FTS rebuild failed for %s: %s", t, errText(err)))
				}
				continue
			}
			rebuilt++
		}
		return nil
	})
	return rebuilt, presentable(err)
}
