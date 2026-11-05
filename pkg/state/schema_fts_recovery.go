package state

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Holder deferral escalation thresholds and the bounded doubling backoff of the in process stale FTS retry
const (
	ftsHolderEscalateAttempts = 3
	ftsHolderEscalateSeconds  = 60.0
	ftsHolderFutileAttempts   = 10
	ftsHolderFutileSeconds    = 1800.0
	ftsStaleRetrySeconds      = 60.0
	ftsStaleRetryMaxSeconds   = 3600.0
)

// Wall clock seconds the deferral diagnostic records, swapped by tests
var schemaWallTime = func() float64 { return metadataWallTime() }

// Lease hook of the armed startup watchdog, nil while no watchdog is armed
var startupProgressLease atomic.Pointer[func(expectedS float64, phase string)]

// Installs the armed startup watchdog lease hook, nil on disarm, called by the watchdog when it arms
func SetStartupProgressLease(fn func(expectedS float64, phase string)) {
	if fn == nil {
		startupProgressLease.Store(nil)
		return
	}
	startupProgressLease.Store(&fn)
}

// Declares a phase owned progress lease on the armed startup watchdog, doing nothing while none is armed
var reportStartupProgress = func(expectedS float64, phase string) {
	if fn := startupProgressLease.Load(); fn != nil {
		(*fn)(expectedS, phase)
	}
}

// First 120 characters of a holder command line, or a marker when the argv cannot be read
func holderCmdline(pid int) string {
	argv, ok := readProcArgv(pid)
	if !ok || len(argv) == 0 {
		return "<cmdline unavailable>"
	}
	runes := pystr.Runes(strings.Join(argv, " "))
	if len(runes) > 120 {
		runes = runes[:120]
	}
	return pystr.FromRunes(runes)
}

// Python repr of a holder list, a list of pid and path tuples
func holdersRepr(holders []Holder) string {
	items := make([]any, len(holders))
	for i, h := range holders {
		items[i] = pyrepr.Tuple{int64(h.PID), h.Target}
	}
	return pyrepr.Repr(items)
}

// Sorted distinct positive holder pids
func positiveHolderPIDs(holders []Holder) []int {
	pids := []int{}
	for _, h := range holders {
		if h.PID > 0 && !slices.Contains(pids, h.PID) {
			pids = append(pids, h.PID)
		}
	}
	slices.Sort(pids)
	return pids
}

// Python list of ints for equality and JSON rendering
func pidList(pids []int) []any {
	out := make([]any, len(pids))
	for i, pid := range pids {
		out[i] = int64(pid)
	}
	return out
}

// Deferral bookkeeping read from the stored record, reset as Python does on a TypeError or ValueError
func deferralCounters(record *pyjson.Object, now float64) (firstSeen float64, attempts *big.Int, holdersSince float64, holdersAttempts *big.Int, err error) {
	get := func(key string, def any) any {
		if v, ok := record.Get(key); ok {
			return v
		}
		return def
	}
	one := big.NewInt(1)
	firstSeen, err = pyint.Float(get("first_seen", now))
	if err == nil {
		attempts, err = pyint.Of(get("attempts", int64(0)))
	}
	if err == nil {
		attempts = new(big.Int).Add(attempts, one)
		holdersSince, err = pyint.Float(get("holders_since", now))
	}
	if err == nil {
		holdersAttempts, err = pyint.Of(get("holders_attempts", int64(0)))
	}
	if err == nil {
		holdersAttempts = new(big.Int).Add(holdersAttempts, one)
		return firstSeen, attempts, holdersSince, holdersAttempts, nil
	}
	if pe, ok := err.(*pyint.Error); ok && (pe.Kind == "TypeError" || pe.Kind == "ValueError") {
		return now, big.NewInt(1), now, big.NewInt(1), nil
	}
	return 0, nil, 0, nil, err
}

// Records the holder deferral diagnostic and reports whether foreign holders still block the rebuild
func (s *sessionSchema) deferStaleFTSForHolders(ctx context.Context, conn SQLiteConn, foreignHolders []Holder) (bool, error) {
	now := schemaWallTime()
	row, err := fetchOne(ctx, conn, "SELECT value FROM state_meta WHERE key = ? LIMIT 1", FTSRebuildDeferralKey)
	if err != nil {
		if _, ok := sqliteClassOf(err); !ok {
			return false, err
		}
		row = nil
	}
	record := pyjson.NewObject()
	if row != nil {
		if parsed, ok := pyjson.SafeLoads(row[0], nil).(*pyjson.Object); ok {
			record = parsed
		}
	}
	firstSeen, attempts, holdersSince, holdersAttempts, err := deferralCounters(record, now)
	if err != nil {
		return false, err
	}
	if firstSeen > now || firstSeen < 0 {
		firstSeen = now
	}
	holderPIDs := positiveHolderPIDs(foreignHolders)
	if stored, _ := record.Get("holder_pids"); !pyrepr.Equal(pidList(holderPIDs), stored) {
		holdersSince, holdersAttempts = now, big.NewInt(1)
	}
	escalate := attempts.Cmp(big.NewInt(ftsHolderEscalateAttempts)) >= 0 && now-firstSeen >= ftsHolderEscalateSeconds
	if escalate {
		reaped := s.host.reapInactiveOrphanDesktopHolders(foreignHolders, ftsHolderEscalateSeconds)
		if len(reaped) > 0 {
			slog.Error(fmt.Sprintf("Reaped inactive orphan Desktop backend(s) %s after %s "+
				"state.db FTS rebuild deferrals; checking holders again.", pyrepr.Repr(pidList(reaped)), attempts.String()))
			if foreignHolders, err = s.host.foreignStateDBHolders(); err != nil {
				return false, err
			}
			holderPIDs = positiveHolderPIDs(foreignHolders)
		}
	}
	futile := len(holderPIDs) > 0 && holdersAttempts.Cmp(big.NewInt(ftsHolderFutileAttempts)) >= 0 &&
		now-holdersSince >= ftsHolderFutileSeconds
	diagnostic := pyjson.NewObject()
	diagnostic.Set("first_seen", firstSeen)
	diagnostic.Set("last_seen", now)
	diagnostic.Set("attempts", pyint.Value(attempts))
	diagnostic.Set("holder_pids", pidList(holderPIDs))
	diagnostic.Set("holders_since", holdersSince)
	diagnostic.Set("holders_attempts", pyint.Value(holdersAttempts))
	diagnostic.Set("futile", futile)
	text, err := pyjson.Dumps(diagnostic, pyjson.SortKeys(true))
	if err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, stateMetaUpsertSQL, FTSRebuildDeferralKey, text); err != nil {
		return false, err
	}
	if len(foreignHolders) == 0 {
		s.setDeferredHolderPIDs(nil)
		return false, nil
	}
	s.setDeferredHolderPIDs(holderPIDs)
	if futile {
		named := make([]string, len(holderPIDs))
		for i, pid := range holderPIDs {
			named[i] = fmt.Sprintf("pid %d: %s", pid, holderCmdline(pid))
		}
		slog.Error(fmt.Sprintf("state.db FTS repair has been blocked by the same holder(s) for %s deferrals over %s min "+
			"(%s); waiting is futile. Stop ONLY the other holder(s) — this process keeps running and its "+
			"own retry admits the rebuild within %ss of the holder leaving. `icebath doctor` shows this.",
			holdersAttempts.String(), pyFixed0((now-holdersSince)/60.0), strings.Join(named, ", "), pyFixed0(ftsStaleRetrySeconds)))
	} else if escalate {
		slog.Error(fmt.Sprintf("state.db FTS repair remains blocked after %s deferrals by holder(s) %s. Stop the listed "+
			"processes (this process's own retry then rebuilds), or run `icebath sessions optimize-storage` "+
			"with every holder stopped. `icebath doctor` reports this degraded state.", attempts.String(), holdersRepr(foreignHolders)))
	}
	slog.Warn(fmt.Sprintf("Deferred stale state.db FTS rebuild while foreign processes "+
		"hold the database or WAL sidecars (%s); canonical writes and LIKE search remain available (deferral %s).",
		holdersRepr(foreignHolders), attempts.String()))
	return true, nil
}

// Records the holder set that earned the current retry backoff, nil when no holder blocked
func (s *sessionSchema) setDeferredHolderPIDs(pids []int) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	s.ftsDeferredHolderPIDs = pids
}

// Rebuilds stale FTS indexes under rebuild admission, leaving the breadcrumb set when holders or a rival block it
func (s *sessionSchema) recoverStaleFTS(ctx context.Context, conn SQLiteConn, legacy bool, timeoutSeconds *float64) (bool, error) {
	holders, err := s.host.foreignStateDBHolders()
	if err != nil {
		return false, err
	}
	if len(holders) > 0 {
		deferred, err := s.deferStaleFTSForHolders(ctx, conn, holders)
		if err != nil || deferred {
			return false, err
		}
	}
	recovered := false
	err = startupRebuildAdmission(s.host.dbPath(), timeoutSeconds, func(admitted bool) error {
		if !admitted {
			slog.Warn("Deferred stale state.db FTS rebuild: another process holds the rebuild authority; " +
				"canonical writes and LIKE search remain available.")
			return nil
		}
		var err error
		recovered, err = s.recoverStaleFTSLocked(ctx, conn, legacy)
		return err
	})
	return recovered, err
}

// Retries a deferred stale FTS rebuild from a periodic tick with doubling backoff and non blocking admission
func (s *sessionSchema) RetryDeferredFTSRecovery(ctx context.Context) (bool, error) {
	if !s.fts.ftsStale.Load() {
		return false, nil
	}
	if _, quarantined := s.host.quarantineReason(); quarantined {
		s.retryMu.Lock()
		s.ftsStaleRetryAfter, s.ftsStaleRetryInterval = 0, 0
		s.retryMu.Unlock()
		return false, nil
	}
	if s.host.readOnly() || s.host.writerConn() == nil {
		return false, nil
	}
	now := monotonicSeconds()
	s.retryMu.Lock()
	deferredPIDs, retryAfter := s.ftsDeferredHolderPIDs, s.ftsStaleRetryAfter
	s.retryMu.Unlock()
	if now < retryAfter {
		if deferredPIDs == nil {
			return false, nil
		}
		holders, err := s.host.foreignStateDBHolders()
		if err != nil {
			return false, err
		}
		if slices.Equal(positiveHolderPIDs(holders), deferredPIDs) {
			return false, nil
		}
		s.retryMu.Lock()
		s.ftsStaleRetryInterval = 0
		s.retryMu.Unlock()
	}
	s.retryMu.Lock()
	interval := s.ftsStaleRetryInterval
	if interval <= 0 {
		interval = ftsStaleRetrySeconds
	}
	s.ftsStaleRetryAfter = now + interval
	s.ftsStaleRetryInterval = math.Min(math.Max(math.Max(interval, ftsStaleRetrySeconds), 1.0)*2.0, ftsStaleRetryMaxSeconds)
	s.retryMu.Unlock()
	recovered, err := s.retryDeferredFTSRecoveryLocked(ctx)
	if err != nil {
		slog.Warn("In-process retry of the deferred stale state.db FTS rebuild failed; will retry later.", "error", err)
		return false, nil
	}
	return recovered, nil
}

// Runs one deferred recovery attempt on the writer under its lock, committing whatever it settled
func (s *sessionSchema) retryDeferredFTSRecoveryLocked(ctx context.Context) (bool, error) {
	lock := s.host.writerLock()
	lock.Lock()
	defer lock.Unlock()
	conn := s.host.writerConn()
	if conn == nil || !s.fts.ftsStale.Load() {
		return false, nil
	}
	legacy, err := dbHasLegacyInlineFTS(ctx, conn)
	if err != nil {
		return false, err
	}
	zero := 0.0
	recovered, err := s.recoverStaleFTS(ctx, conn, legacy, &zero)
	if err != nil {
		return false, err
	}
	if recovered {
		if err := s.fts.ensureFTSCJKSchema(ctx, conn); err != nil {
			return false, err
		}
		s.retryMu.Lock()
		s.ftsStaleRetryInterval = 0
		s.retryMu.Unlock()
	}
	if err := endTransaction(ctx, conn, "COMMIT"); err != nil {
		if _, ok := sqliteClassOf(err); !ok {
			return false, err
		}
	}
	return recovered, nil
}

// Drops and rebuilds the stale base and trigram indexes in one write transaction, detaching FTS when it fails
func (s *sessionSchema) recoverStaleFTSLocked(ctx context.Context, conn SQLiteConn, legacy bool) (bool, error) {
	var includeTrigram bool
	status, err := s.fts.ftsTableProbe(ctx, conn, "messages_fts_trigram")
	switch {
	case err != nil && isDatabaseError(err):
		includeTrigram = true
	case err != nil:
		return false, err
	case status == ftsTableStatusQueryable:
		includeTrigram = true
	case !legacy:
		if includeTrigram, err = s.trigramTokenizerAvailable(ctx, conn); err != nil {
			return false, err
		}
	}
	var drop strings.Builder
	for _, trigger := range ftsTriggers {
		drop.WriteString("DROP TRIGGER IF EXISTS " + trigger + ";")
	}
	if includeTrigram {
		drop.WriteString("DROP TABLE IF EXISTS messages_fts_trigram;")
	}
	drop.WriteString("DROP VIEW IF EXISTS messages_fts_trigram_src;DROP TABLE IF EXISTS messages_fts;")
	var rebuild string
	if legacy {
		rebuild = LegacyFTSSQL
		if includeTrigram {
			rebuild += LegacyFTSTrigramSQL
		}
		rebuild += legacyInlineReinsertSQL("messages_fts", 16, false)
		if includeTrigram {
			rebuild += legacyInlineReinsertSQL("messages_fts_trigram", 20, true)
		}
	} else {
		rebuild = FTSSQL
		if includeTrigram {
			rebuild += FTSTrigramSQL
		}
		rebuild += "INSERT INTO messages_fts(messages_fts) VALUES('rebuild');"
		if includeTrigram {
			rebuild += "INSERT INTO messages_fts_trigram(messages_fts_trigram) VALUES('rebuild');"
		}
		rebuild += clearRebuildMarkersSQL + ";" + dropRetiredToolHighWaterSQL + ";"
	}
	recovery := "BEGIN IMMEDIATE;" + drop.String() + rebuild +
		fmt.Sprintf("DELETE FROM state_meta WHERE key IN ('%s', '%s');COMMIT;", FTSStaleKey, FTSRebuildDeferralKey)
	if err := executeScript(ctx, conn, recovery); err != nil {
		if !isDatabaseError(err) {
			return false, err
		}
		if rerr := endTransaction(ctx, conn, "ROLLBACK"); rerr != nil {
			if _, ok := sqliteClassOf(rerr); !ok {
				return false, rerr
			}
		}
		if derr := s.fts.dropAllFTSTriggers(ctx, conn); derr != nil {
			return false, derr
		}
		if cerr := endTransaction(ctx, conn, "COMMIT"); cerr != nil {
			return false, cerr
		}
		slog.Error(fmt.Sprintf("Automatic rebuild of stale FTS indexes failed (%s); "+
			"canonical writes remain enabled with FTS detached.", errText(err)))
		return false, nil
	}
	s.fts.ftsStale.Store(false)
	s.fts.ftsEnabled.Store(true)
	s.fts.trigramAvailable.Store(includeTrigram)
	slog.Warn("Rebuilt stale state.db FTS indexes from canonical messages and restored sync triggers.")
	return true, nil
}

// Probes the trigram tokenizer through a temp table that never outlives the probe
func (s *sessionSchema) trigramTokenizerAvailable(ctx context.Context, conn SQLiteConn) (bool, error) {
	const probe = "temp.hermes_fts5_trigram_probe"
	if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+probe); err != nil {
		return false, err
	}
	_, err := conn.ExecContext(ctx, "CREATE VIRTUAL TABLE "+probe+" USING fts5(content, tokenize='trigram')")
	unavailable := err != nil && isOperationalError(err) && isTrigramUnavailableError(err)
	if unavailable {
		s.fts.warnTrigramUnavailable(err)
	}
	if _, derr := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+probe); derr != nil {
		return false, derr
	}
	if err != nil && !unavailable {
		return false, err
	}
	return err == nil, nil
}

// Legacy inline FTS repopulation script fragment with its pinned indentation
func legacyInlineReinsertSQL(table string, indent int, deleteFirst bool) string {
	pad := strings.Repeat(" ", indent)
	body := ""
	if deleteFirst {
		body = pad + "DELETE FROM " + table + ";\n"
	}
	return "\n" + body + pad + "INSERT INTO " + table + "(rowid, content)\n" +
		pad + "SELECT id,\n" +
		pad + "       COALESCE(content, '') || ' ' ||\n" +
		pad + "       COALESCE(tool_name, '') || ' ' ||\n" +
		pad + "       COALESCE(tool_calls, '')\n" +
		pad + "FROM messages;\n" + pad[:max(len(pad)-4, 0)]
}
