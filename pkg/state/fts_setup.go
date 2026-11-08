package state

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// FTS5 shadow tables the virtual table engine owns, orphaned by sqlite3 .recover
var ftsShadowSuffixes = []string{"content", "data", "docsize", "idx", "config"}

// List of n question mark binds joined by commas
func sqlBindMarks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// String values as query arguments
func stringArgs(values []string) []any {
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return args
}

// First column of every row as text
func queryStrings(ctx context.Context, conn SQLiteConn, query string, args ...any) ([]string, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s.String)
	}
	return out, rows.Err()
}

// Reports whether a query yields a row
func rowExists(ctx context.Context, conn SQLiteConn, query string, args ...any) (bool, error) {
	row, err := fetchOne(ctx, conn, query, args...)
	return row != nil, err
}

// Runs a multi statement script as sqlite3 executescript, committing an open transaction first
func executeScript(ctx context.Context, conn SQLiteConn, script string) error {
	auto, err := sqliteAutocommit(conn)
	if err != nil {
		return err
	}
	if !auto {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, script)
	return err
}

// Drops per family the shadow tables whose virtual table row is gone, returning the repaired families
func dropOrphanFTSShadowTables(ctx context.Context, conn SQLiteConn, families []string) ([]string, error) {
	repaired := []string{}
	for _, family := range families {
		live, err := rowExists(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ? "+
			"AND sql LIKE 'CREATE VIRTUAL TABLE%'", family)
		if err != nil {
			return repaired, err
		}
		if live {
			continue
		}
		shadows := make([]string, len(ftsShadowSuffixes))
		for i, suffix := range ftsShadowSuffixes {
			shadows[i] = family + "_" + suffix
		}
		orphans, err := queryStrings(ctx, conn, fmt.Sprintf(
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name IN (%s)", sqlBindMarks(len(shadows))),
			stringArgs(shadows)...)
		if err != nil {
			return repaired, err
		}
		if len(orphans) == 0 {
			continue
		}
		for _, name := range orphans {
			if _, err := conn.ExecContext(ctx, fmt.Sprintf(`DROP TABLE "%s"`, name)); err != nil {
				return repaired, err
			}
		}
		slog.Warn(fmt.Sprintf("Dropped orphan FTS5 shadow tables of %s (%s); the index is recreated from messages",
			family, strings.Join(orphans, ", ")))
		repaired = append(repaired, family)
	}
	return repaired, nil
}

// Rows per chunk and pacing of the chunked FTS rebuild engine
const (
	ftsRebuildChunkRows     = 500
	ftsRebuildChunkTargetS  = 0.25
	ftsRebuildPacedTargetS  = 0.05
	ftsRebuildDutyFactor    = 4.0
	ftsRebuildMinPause      = 0.2
	ftsTrashPrefix          = "fts_v22_trash_"
	ftsTableStatusDegraded  = ftsTableStatus(0)
	ftsTableStatusAbsent    = ftsTableStatus(1)
	ftsTableStatusQueryable = ftsTableStatus(2)
)

// Chunk size bounds of the rebuild engine, class attributes hermes tests patch
var (
	ftsRebuildChunkMinRows = 50
	ftsRebuildChunkMaxRows = 100_000
)

// FTS5 tables merged on optimize, each probed before use
var ftsTables = []string{"messages_fts", "messages_fts_trigram", "messages_fts_cjk"}

// Outcome of probing an FTS table, degraded when the module, tokenizer or content is unusable
type ftsTableStatus int

// SessionDB members the FTS lifecycle calls, provided by the hermes_state.py SessionDB
type ftsSetupHost interface {
	dbPath() string
	raiseIfDBCorrupt(storage bool) error
	raiseIfDBReplaced() error
	writerLock() sync.Locker
	writerConn() SQLiteConn
	reopenAfterCloseLocked(context string) error
	sleepBeforeWriteRetry(deadline, patienceS float64) bool
	writePatienceS() float64
}

// FTS table and trigger lifecycle state, embedded in SessionDB so its methods are SessionDB methods
type sessionFTSSetup struct {
	host                     ftsSetupHost
	ftsEnabled               atomic.Bool
	ftsStale                 atomic.Bool
	trigramAvailable         atomic.Bool
	ftsCJKLoaded             atomic.Bool
	ftsCJKAvailable          atomic.Bool
	ftsUnavailableWarned     atomic.Bool
	trigramUnavailableWarned atomic.Bool
	ftsChunkRows             int
}

// Lifecycle state for host with every flag false and the first rebuild chunk size
func newSessionFTSSetup(host ftsSetupHost) *sessionFTSSetup {
	return &sessionFTSSetup{host: host, ftsChunkRows: ftsRebuildChunkRows}
}

// Reports a missing FTS5 module or a missing optional tokenizer
func isFTS5UnavailableError(err error) bool {
	text := pystr.Lower(errText(err))
	return (strings.Contains(text, "no such module") && strings.Contains(text, "fts5")) || isTrigramUnavailableError(err)
}

// Reports that only the trigram or cjk_unicode61 tokenizer is missing
func isTrigramUnavailableError(err error) bool {
	text := pystr.Lower(errText(err))
	return strings.Contains(text, "no such tokenizer: trigram") || strings.Contains(text, "no such tokenizer: cjk_unicode61")
}

// Reports a messages_fts in any pre v23 shape, every one lacking the tool_name column
func dbHasLegacyInlineFTS(ctx context.Context, conn SQLiteConn) (bool, error) {
	row, err := fetchOne(ctx, conn, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'messages_fts'")
	if err != nil || row == nil {
		return false, err
	}
	text, _ := row[0].(string)
	return !strings.Contains(text, "tool_name"), nil
}

// Reports a trigram vtable that still projects the tool_calls payload, FTS storage version 1
func dbHasTrigramToolCallsProjection(ctx context.Context, conn SQLiteConn) (bool, error) {
	row, err := fetchOne(ctx, conn, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'messages_fts_trigram'")
	if err != nil || row == nil {
		return false, err
	}
	text, _ := row[0].(string)
	return strings.Contains(pystr.Lower(text), "tool_calls"), nil
}

// Reports layout work for optimize storage, a stored FTS vtable differing from the shipping DDL
func dbNeedsFTSStorageUpgrade(ctx context.Context, conn SQLiteConn) (bool, error) {
	drift, err := FTSLayoutDrift(ctx, conn)
	return len(drift) > 0, err
}

// Logs once that the trigram tokenizer is missing while base FTS5 stays enabled
func (f *sessionFTSSetup) warnTrigramUnavailable(err error) {
	if f.trigramUnavailableWarned.Swap(true) {
		return
	}
	slog.Info(fmt.Sprintf("SQLite trigram tokenizer unavailable for %s "+
		"(requires SQLite >= 3.34, this build is %s); "+
		"CJK/substring search will fall back to LIKE: %s", f.host.dbPath(), LinkedSQLiteVersion(), errText(err)))
}

// Disables full text search and logs once that FTS5 is missing
func (f *sessionFTSSetup) warnFTS5Unavailable(err error) {
	f.ftsEnabled.Store(false)
	if f.ftsUnavailableWarned.Swap(true) {
		return
	}
	slog.Warn(fmt.Sprintf("SQLite FTS5 unavailable for %s; full-text session search "+
		"disabled. Run `icebath update` to rebuild the venv with a "+
		"current Python (managed uv guarantees FTS5). (underlying error: %s)", f.host.dbPath(), errText(err)))
}

// Creates, repairs or detaches the CJK bigram index and sets its availability, raising only non operational errors
func (f *sessionFTSSetup) ensureFTSCJKSchema(ctx context.Context, conn SQLiteConn) error {
	present, err := rowExists(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'messages_fts_cjk'")
	if err == nil && !f.ftsCJKLoaded.Load() {
		if present {
			err = f.dropUnservableCJKTriggers(ctx, conn)
		}
		if err == nil {
			f.ftsCJKAvailable.Store(false)
			return nil
		}
	}
	if err != nil {
		if !isOperationalError(err) {
			return err
		}
		slog.Warn("messages_fts_cjk presence check failed; CJK search stays on trigram/LIKE", "error", err)
		f.ftsCJKAvailable.Store(false)
		return nil
	}
	available, err := ensureLoadedCJKIndex(ctx, conn, present)
	if err != nil {
		if !isOperationalError(err) {
			return err
		}
		slog.Warn("messages_fts_cjk ensure failed; CJK search stays on trigram/LIKE", "error", err)
		available = false
	}
	f.ftsCJKAvailable.Store(available)
	return nil
}

// Records the stale breadcrumb and drops live cjk triggers a process without the tokenizer cannot fire
func (f *sessionFTSSetup) dropUnservableCJKTriggers(ctx context.Context, conn SQLiteConn) error {
	live, err := queryStrings(ctx, conn, fmt.Sprintf("SELECT name FROM sqlite_master WHERE type = 'trigger' "+
		"AND name IN (%s)", sqlBindMarks(len(ftsCJKTriggers))), stringArgs(ftsCJKTriggers)...)
	if err != nil || len(live) == 0 {
		return err
	}
	slog.Warn(fmt.Sprintf("messages_fts_cjk triggers present but the "+
		"cjk_unicode61 tokenizer is unavailable (%s) — "+
		"dropping the cjk triggers so message writes keep "+
		"working. CJK search falls back to trigram/LIKE; "+
		"run `icebath sessions optimize-storage` on a host "+
		"with the extension to rebuild.", FTS5CJKSoPath(ctx)))
	if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta (key, value) VALUES (?, '1') "+
		"ON CONFLICT(key) DO UPDATE SET value = '1'", FTSCJKStaleKey); err != nil {
		return err
	}
	for _, trig := range live {
		if _, err := conn.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trig); err != nil {
			return err
		}
	}
	return nil
}

// Ensures the cjk table and, unless stale, its triggers, reporting whether the index is complete and served
func ensureLoadedCJKIndex(ctx context.Context, conn SQLiteConn, present bool) (bool, error) {
	if err := executeScript(ctx, conn, FTSCJKTableSQL); err != nil {
		return false, err
	}
	if !present {
		if _, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key = ?", FTSCJKStaleKey); err != nil {
			return false, err
		}
		row, err := fetchOne(ctx, conn, "SELECT COUNT(*) FROM messages WHERE role <> 'tool'")
		if err != nil {
			return false, err
		}
		if n, _ := row[0].(int64); n > 0 {
			hw, err := fetchOne(ctx, conn, "SELECT COALESCE(MAX(id), 0) FROM messages")
			if err != nil {
				return false, err
			}
			for _, kv := range [][2]string{{"fts_cjk_rebuild_high_water", fmt.Sprint(hw[0])}, {"fts_cjk_rebuild_progress", "0"}} {
				if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta (key, value) VALUES (?, ?) "+
					"ON CONFLICT(key) DO UPDATE SET value = excluded.value", kv[0], kv[1]); err != nil {
					return false, err
				}
			}
		}
	}
	stale, err := rowExists(ctx, conn, "SELECT 1 FROM state_meta WHERE key = ?", FTSCJKStaleKey)
	if err != nil || stale {
		return false, err
	}
	if err := executeScript(ctx, conn, FTSCJKTriggerSQL); err != nil {
		return false, err
	}
	pending, err := rowExists(ctx, conn, "SELECT 1 FROM state_meta WHERE key = 'fts_cjk_rebuild_high_water' LIMIT 1")
	return !pending, err
}

// Drops the base and trigram FTS triggers, ignoring operational errors
func dropFTSTriggers(ctx context.Context, conn SQLiteConn) error {
	for _, trigger := range ftsTriggers {
		if _, err := conn.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger); err != nil && !isOperationalError(err) {
			return err
		}
	}
	return nil
}

// Drops every FTS trigger including the cjk ones, ignoring operational errors
func (f *sessionFTSSetup) dropAllFTSTriggers(ctx context.Context, conn SQLiteConn) error {
	if err := dropFTSTriggers(ctx, conn); err != nil {
		return err
	}
	for _, trigger := range ftsCJKTriggers {
		if _, err := conn.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger); err != nil && !isOperationalError(err) {
			return err
		}
	}
	return nil
}

// Statement probing an FTS table for a usable row shape, a var the stale recovery tests patch
var ftsProbeExec = func(ctx context.Context, conn SQLiteConn, table string) error {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s LIMIT 0", table))
	if err == nil {
		err = rows.Close()
	}
	return err
}

// Probes an FTS table as queryable, absent, or degraded when its module, tokenizer or content is unusable
func (f *sessionFTSSetup) ftsTableProbe(ctx context.Context, conn SQLiteConn, table string) (ftsTableStatus, error) {
	err := ftsProbeExec(ctx, conn, table)
	if err == nil {
		return ftsTableStatusQueryable, nil
	}
	if !isOperationalError(err) {
		return ftsTableStatusDegraded, err
	}
	if isFTS5UnavailableError(err) {
		if isTrigramUnavailableError(err) {
			f.warnTrigramUnavailable(err)
		} else {
			f.warnFTS5Unavailable(err)
		}
		return ftsTableStatusDegraded, nil
	}
	text := pystr.Lower(errText(err))
	if strings.Contains(text, "no such table") {
		return ftsTableStatusAbsent, nil
	}
	if !strings.Contains(text, "decode to utf-8") {
		return ftsTableStatusDegraded, err
	}
	slog.Warn(fmt.Sprintf("%s probe encountered invalid UTF-8 in FTS content; "+
		"search may return incomplete results until FTS is rebuilt: %s", table, errText(err)))
	return ftsTableStatusDegraded, nil
}

// Runs an FTS DDL script when its table is usable, false when FTS5 or its tokenizer is missing
func (f *sessionFTSSetup) ensureFTSSchema(ctx context.Context, conn SQLiteConn, table, ddl string) (bool, error) {
	status, err := f.ftsTableProbe(ctx, conn, table)
	if err != nil || status == ftsTableStatusDegraded {
		return false, err
	}
	if err := executeScript(ctx, conn, ddl); err != nil {
		if !isOperationalError(err) || !isFTS5UnavailableError(err) {
			return false, err
		}
		if isTrigramUnavailableError(err) {
			f.warnTrigramUnavailable(err)
		} else {
			f.warnFTS5Unavailable(err)
		}
		return false, nil
	}
	return true, nil
}

// Reports corruption SQLite scopes to the FTS indexes rather than the canonical tables, a var tests patch
var isFTSWriteCorruptionError = func(err error) bool {
	return IsFTSScopedCorruptionError(err)
}

// Detaches corrupt FTS indexes within the write budget so canonical writes continue, true once detached
func (f *sessionFTSSetup) enterFTSFailOpen(ctx context.Context, exc error, deadline, patienceS *float64) (bool, error) {
	if !f.ftsEnabled.Load() || !isFTSWriteCorruptionError(exc) {
		return false, nil
	}
	patience := f.host.writePatienceS()
	if patienceS != nil {
		patience = *patienceS
	}
	limit := monotonicSeconds() + patience
	if deadline != nil {
		limit = *deadline
	}
	for {
		if err := f.host.raiseIfDBCorrupt(true); err != nil {
			return false, err
		}
		detachErr := f.detachCorruptFTS(ctx)
		if detachErr == nil {
			break
		}
		if _, isSQLite := sqliteClassOf(detachErr); !isSQLite {
			return false, detachErr
		}
		if isOperationalError(detachErr) && IsSQLiteLockError(detachErr) && f.host.sleepBeforeWriteRetry(limit, patience) {
			continue
		}
		slog.Error(fmt.Sprintf("Could not detach corrupt FTS indexes; canonical write still cannot proceed: %s", errText(detachErr)))
		return false, nil
	}
	f.ftsStale.Store(true)
	f.ftsEnabled.Store(false)
	f.trigramAvailable.Store(false)
	f.ftsCJKAvailable.Store(false)
	slog.Error(fmt.Sprintf("state.db FTS indexes remain corrupt (%s); disabled FTS sync and "+
		"retrying the canonical write. Search temporarily uses LIKE until "+
		"a later SessionDB open rebuilds the indexes.", errText(exc)))
	return true, nil
}

// Commits the stale breadcrumbs and the trigger drop atomically on the writer under its lock
func (f *sessionFTSSetup) detachCorruptFTS(ctx context.Context) (err error) {
	lock := f.host.writerLock()
	lock.Lock()
	defer lock.Unlock()
	if err := f.host.raiseIfDBReplaced(); err != nil {
		return err
	}
	if f.host.writerConn() == nil {
		if err := f.host.reopenAfterCloseLocked("write"); err != nil {
			return err
		}
	}
	conn := f.host.writerConn()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			endTransaction(ctx, conn, "ROLLBACK")
		}
	}()
	err = f.detachCorruptFTSBody(ctx, conn)
	if err == nil {
		err = endTransaction(ctx, conn, "COMMIT")
	}
	if err != nil {
		done = true
		if rerr := endTransaction(ctx, conn, "ROLLBACK"); rerr != nil {
			return rerr
		}
		return err
	}
	done = true
	return nil
}

// Writes fts_stale, fts_cjk_stale when cjk triggers exist, then drops every FTS trigger
func (f *sessionFTSSetup) detachCorruptFTSBody(ctx context.Context, conn SQLiteConn) error {
	upsert := "INSERT INTO state_meta (key, value) VALUES (?, '1') ON CONFLICT(key) DO UPDATE SET value = excluded.value"
	if _, err := conn.ExecContext(ctx, upsert, FTSStaleKey); err != nil {
		return err
	}
	cjkTriggersPresent, err := rowExists(ctx, conn, fmt.Sprintf("SELECT 1 FROM sqlite_master WHERE type = 'trigger' "+
		"AND name IN (%s) LIMIT 1", sqlBindMarks(len(ftsCJKTriggers))), stringArgs(ftsCJKTriggers)...)
	if err != nil {
		return err
	}
	if cjkTriggersPresent {
		if _, err := conn.ExecContext(ctx, upsert, FTSCJKStaleKey); err != nil {
			return err
		}
	}
	return f.dropAllFTSTriggers(ctx, conn)
}

// Rows for the next chunk so it costs about targetS, moving at most double or half per step
func (f *sessionFTSSetup) nextChunkRows(rows int, costS, targetS float64) int {
	scale := targetS / max(costS, 1e-4)
	rows = int(float64(rows) * min(2.0, max(0.5, scale)))
	return min(ftsRebuildChunkMaxRows, max(ftsRebuildChunkMinRows, rows))
}

// Reports demoted v22 shadow tables still awaiting teardown
func (f *sessionFTSSetup) hasFTSTrash(ctx context.Context, conn SQLiteConn) (bool, error) {
	return rowExists(ctx, conn, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name LIKE ? ESCAPE '\' LIMIT 1`,
		strings.ReplaceAll(ftsTrashPrefix, "_", `\_`)+"%")
}
