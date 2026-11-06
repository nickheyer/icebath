package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// SessionDB members the search mixin calls beyond the FTS lifecycle ones, provided by the hermes_state.py SessionDB
type searchHost interface {
	ftsSetupHost
	readOnly() bool
	executeWrite(ctx context.Context, fn func(conn SQLiteConn) error, patienceS *float64) error
	readCtx(ctx context.Context, reopen bool, fn func(conn SQLiteConn) error) error
	readAll(ctx context.Context, query string, args ...any) ([]*pyjson.Object, error)
	readOne(ctx context.Context, query string, args ...any) (*pyjson.Object, error)
	decodeContent(content any) any
	getMeta(ctx context.Context, key string) (string, bool, error)
	setMeta(ctx context.Context, key, value string, cursor SQLiteConn) error
	foreignStateDBHolders() ([]Holder, error)
	ftsMergeMaxPagesPerIndex() int
	ftsMergeCommandsPerPass() int
	GetMessagesAround(ctx context.Context, sessionID string, aroundMessageID int64, window int) (*pyjson.Object, error)
	rowToMessageDict(row *pyjson.Object, warnContext string, summaryFlag bool) (*pyjson.Object, error)
}

const (
	optimizeChunkAttempts = 3
	optimizeVacuumTickS   = 1.0
	holderScanIntervalS   = 2.0
)

// Search, deferred rebuild and optimize storage state, embedded in SessionDB so its methods are SessionDB methods
type sessionSearch struct {
	host                     searchHost
	fts                      *sessionFTSSetup
	ftsUsermergeFloorApplied bool
	optimizeChunkAttempts    int
	vacuumTickS              float64
	holderScanIntervalS      float64
}

// Search mixin over host sharing the FTS lifecycle state, timing knobs at their class defaults
func newSessionSearch(host searchHost, fts *sessionFTSSetup) *sessionSearch {
	return &sessionSearch{host: host, fts: fts, optimizeChunkAttempts: optimizeChunkAttempts,
		vacuumTickS: optimizeVacuumTickS, holderScanIntervalS: holderScanIntervalS}
}

// Sleeps the given seconds between optimize chunks, swapped by tests
var searchSleep = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }

var errWriterClosed = errors.New("'NoneType' object has no attribute 'execute'")

// Writer connection, the AttributeError Python raises once close left it None
func (s *sessionSearch) writer() (SQLiteConn, error) {
	if conn := s.host.writerConn(); conn != nil {
		return conn, nil
	}
	return nil, errWriterClosed
}

// Binds a Python int as SQLite does, OverflowError past the signed 64 bit range
func sqliteInt(n *big.Int) (int64, error) {
	if !n.IsInt64() {
		return 0, &pyint.Error{Kind: "OverflowError", Msg: "Python int too large to convert to SQLite INTEGER"}
	}
	return n.Int64(), nil
}

var searchMessageResultFields = []string{"id", "session_id", "role", "snippet", "timestamp", "tool_name", "source",
	"model", "session_started", "context"}

// Requested result fields in canonical order, nil for every field, ValueError naming unknown ones
func searchMessageFields(fields []string) ([]string, error) {
	if fields == nil {
		return nil, nil
	}
	requested := map[string]bool{}
	for _, f := range fields {
		requested[f] = true
	}
	var unknown []string
	for f := range requested {
		if !slices.Contains(searchMessageResultFields, f) {
			unknown = append(unknown, f)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, &pyint.Error{Kind: "ValueError", Msg: "unknown search result field(s): " + strings.Join(unknown, ", ")}
	}
	out := []string{}
	for _, f := range searchMessageResultFields {
		if requested[f] {
			out = append(out, f)
		}
	}
	return out, nil
}

// Runs one bounded merge pass after a committed write, logging every failure instead of returning it
func (s *sessionSearch) tryIncrementalMergeFTS(ctx context.Context) {
	if !s.fts.ftsEnabled.Load() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn(fmt.Sprintf("FTS incremental merge failed after commit: %s", errText(panicError(r))))
		}
	}()
	if _, err := s.mergeFTSIncrementally(ctx, s.host.ftsMergeMaxPagesPerIndex(), nil); err != nil {
		slog.Warn(fmt.Sprintf("FTS incremental merge failed after commit: %s", errText(err)))
	}
}

// Error carried by a recovered panic value
func panicError(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return fmt.Errorf("%v", r)
}

// Reports whether an FTS5 table is queryable, any database error counting as absent
func (s *sessionSearch) ftsTableExists(ctx context.Context, name string) (bool, error) {
	conn, err := s.writer()
	if err != nil {
		return false, err
	}
	rows, err := conn.QueryContext(ctx, "SELECT 1 FROM "+name+" LIMIT 0")
	if err == nil {
		err = rows.Close()
	}
	if err != nil {
		if isDatabaseError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Queryable FTS tables, caller holds the writer lock
func (s *sessionSearch) presentFTSTables(ctx context.Context) ([]string, error) {
	present := []string{}
	for _, tbl := range ftsTables {
		ok, err := s.ftsTableExists(ctx, tbl)
		if err != nil {
			return nil, err
		}
		if ok {
			present = append(present, tbl)
		}
	}
	return present, nil
}

// Runs bounded FTS5 merge commands on each present index until no progress, returning the commands run
func (s *sessionSearch) mergeFTSIncrementally(ctx context.Context, maxPages int, maxCommands *int) (int, error) {
	if err := positiveInt("max_pages", maxPages); err != nil {
		return 0, err
	}
	commands := s.host.ftsMergeCommandsPerPass()
	if maxCommands != nil {
		commands = *maxCommands
	}
	if err := positiveInt("max_commands", commands); err != nil {
		return 0, err
	}
	executed := 0
	lock := s.host.writerLock()
	lock.Lock()
	defer lock.Unlock()
	present, err := s.presentFTSTables(ctx)
	if err != nil {
		return executed, err
	}
	conn, err := s.writer()
	if err != nil {
		return executed, err
	}
	for _, tbl := range present {
		if !s.ftsUsermergeFloorApplied {
			if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s, rank) VALUES('usermerge', 2)", tbl, tbl)); err != nil {
				return executed, err
			}
		}
		for range commands {
			before, err := sqliteTotalChanges(conn)
			if err != nil {
				return executed, err
			}
			if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s, rank) VALUES('merge', ?)", tbl, tbl), maxPages); err != nil {
				return executed, err
			}
			executed++
			after, err := sqliteTotalChanges(conn)
			if err != nil {
				return executed, err
			}
			if after-before < 2 {
				break
			}
		}
	}
	s.ftsUsermergeFloorApplied = true
	return executed, nil
}

// Deferred rebuild markers of one backfill, high water and progress as Python ints
type ftsRebuildState struct {
	total   *big.Int
	indexed *big.Int
}

// Status payload with pending, total, indexed and the truncated percent capped at 100
func (st *ftsRebuildState) info() (*pyjson.Object, error) {
	ratio, _ := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(100), st.indexed), st.total).Float64()
	if math.IsInf(ratio, 0) {
		return nil, &pyint.Error{Kind: "OverflowError", Msg: "integer division result too large for a float"}
	}
	var percent any = 100
	if ratio < 100 {
		n, _ := big.NewFloat(ratio).Int(nil)
		percent = pyint.Native(n)
	}
	out := pyjson.NewObject()
	out.Set("pending", true)
	out.Set("total", pyint.Value(st.total))
	out.Set("indexed", pyint.Value(st.indexed))
	out.Set("percent", percent)
	return out, nil
}

// Deferred base rebuild progress or nil when none is pending, read through the pooled reader
func (s *sessionSearch) FTSRebuildStatus(ctx context.Context) (*pyjson.Object, error) {
	return s.rebuildStatus(ctx, "fts_rebuild")
}

// CJK index backfill progress or nil when none is pending
func (s *sessionSearch) FTSCJKRebuildStatus(ctx context.Context) (*pyjson.Object, error) {
	return s.rebuildStatus(ctx, "fts_cjk_rebuild")
}

// Progress payload of the backfill under prefix, nil when its high water is absent or not positive
func (s *sessionSearch) rebuildStatus(ctx context.Context, prefix string) (*pyjson.Object, error) {
	st, err := s.rebuildState(ctx, prefix)
	if err != nil || st == nil {
		return nil, err
	}
	return st.info()
}

// Parsed markers of the backfill under prefix, nil when its high water is absent or not positive
func (s *sessionSearch) rebuildState(ctx context.Context, prefix string) (*ftsRebuildState, error) {
	rows, err := s.host.readAll(ctx, "SELECT key, value FROM state_meta WHERE key IN (?, ?)",
		prefix+"_high_water", prefix+"_progress")
	if err != nil {
		return nil, err
	}
	meta := map[string]any{}
	for _, r := range rows {
		key, _ := r.Get("key")
		meta[errText(key)], _ = r.Get("value")
	}
	highWater, ok := meta[prefix+"_high_water"]
	if !ok || highWater == nil {
		return nil, nil
	}
	total, err := pyint.Of(highWater)
	if err != nil {
		return nil, err
	}
	if total.Sign() <= 0 {
		return nil, nil
	}
	progressRaw := meta[prefix+"_progress"]
	progress := big.NewInt(0)
	if pyTruthy(progressRaw) {
		if progress, err = pyint.Of(progressRaw); err != nil {
			return nil, err
		}
	}
	return &ftsRebuildState{total: total, indexed: progress}, nil
}

const boundarySweepSQL = "INSERT INTO {table}(rowid, content, tool_name, tool_calls) " +
	"SELECT m.id, m.content, m.tool_name, m.tool_calls FROM messages m WHERE m.id > ? AND m.id <= ? {extra}" +
	"AND NOT EXISTS (SELECT 1 FROM {table}_docsize d WHERE d.id = m.id)"

const baseBoundarySweepSQL = "INSERT INTO messages_fts(rowid, content, tool_name, tool_calls) " +
	"SELECT m.id, CASE WHEN m.role = 'tool' THEN substr(COALESCE(m.content, ''), 1, ?) " +
	"ELSE m.content END, m.tool_name, m.tool_calls FROM messages m WHERE m.id > ? AND m.id <= ? " +
	"AND NOT EXISTS (SELECT 1 FROM messages_fts_docsize d WHERE d.id = m.id)"

var trigramBoundarySweepSQL = "INSERT INTO messages_fts_trigram(rowid, content, tool_name) " +
	"SELECT m.id, m.content, m.tool_name FROM messages m JOIN sessions s ON s.id = m.session_id " +
	"WHERE m.id > ? AND m.id <= ? AND m.role <> 'tool' AND " + FTSTrigramSessionSQLFor("s") + " " +
	"AND NOT EXISTS (SELECT 1 FROM messages_fts_trigram_docsize d WHERE d.id = m.id)"

var chunkInsertSQL = "INSERT INTO {table}(rowid, content, tool_name, tool_calls) " +
	"SELECT id, CASE WHEN role = 'tool' " +
	fmt.Sprintf("THEN substr(COALESCE(content, ''), 1, %d) ", FTSToolContentPrefixChars) +
	"ELSE content END, tool_name, tool_calls FROM messages WHERE id > ? AND id <= ?{extra}"

var trigramChunkInsertSQL = "INSERT INTO messages_fts_trigram(rowid, content, tool_name) " +
	"SELECT m.id, m.content, m.tool_name FROM messages m JOIN sessions s ON s.id = m.session_id " +
	"WHERE m.id > ? AND m.id <= ? AND m.role <> 'tool' AND " + FTSTrigramSessionSQLFor("s")

// Fills the table and extra fields of a sweep or chunk template as str.format does
func formatFTSInsert(template, table, extra string) string {
	return strings.NewReplacer("{table}", table, "{extra}", extra).Replace(template)
}

// Boundary sweep statement, bounded ones taking the tool content prefix length first
type rebuildSweep struct {
	sql     string
	bounded bool
}

// Sweeps the base and servable trigram windows, then clears the base rebuild markers
func (s *sessionSearch) ftsRebuildFinish(ctx context.Context) error {
	sweeps := []rebuildSweep{{baseBoundarySweepSQL, true}}
	if s.fts.trigramAvailable.Load() {
		sweeps = append(sweeps, rebuildSweep{trigramBoundarySweepSQL, false})
	}
	if err := s.rebuildFinish(ctx, "fts_rebuild", sweeps); err != nil {
		return err
	}
	slog.Info("Deferred FTS rebuild complete — all messages indexed.")
	return nil
}

// Sweeps the CJK window, clears its markers and marks the CJK index servable
func (s *sessionSearch) ftsCJKRebuildFinish(ctx context.Context) error {
	sweep := formatFTSInsert(boundarySweepSQL, "messages_fts_cjk", "AND m.role <> 'tool' ")
	if err := s.rebuildFinish(ctx, "fts_cjk_rebuild", []rebuildSweep{{sweep, false}}); err != nil {
		return err
	}
	s.fts.ftsCJKAvailable.Store(true)
	slog.Info("CJK FTS index backfill complete — serving CJK search.")
	return nil
}

// Sweeps a thousand ids either side of the high water, then deletes the high water and progress markers
func (s *sessionSearch) rebuildFinish(ctx context.Context, prefix string, sweeps []rebuildSweep) error {
	return s.host.executeWrite(ctx, func(conn SQLiteConn) error {
		hwRow, err := metaRow(ctx, conn, prefix+"_high_water")
		if err != nil {
			return err
		}
		if hwRow != nil {
			hw, err := pyint.Of(hwRow[0])
			if err != nil {
				return err
			}
			lo, err := sqliteInt(new(big.Int).Sub(hw, big.NewInt(1000)))
			if err != nil {
				return err
			}
			hi, err := sqliteInt(new(big.Int).Add(hw, big.NewInt(1000)))
			if err != nil {
				return err
			}
			for _, sw := range sweeps {
				args := []any{}
				if sw.bounded {
					args = append(args, FTSToolContentPrefixChars)
				}
				if _, err := conn.ExecContext(ctx, sw.sql, append(args, lo, hi)...); err != nil {
					return err
				}
			}
		}
		return deleteMeta(ctx, conn, prefix+"_high_water", prefix+"_progress")
	}, nil)
}

// Backfills one chunk of the deferred base rebuild into every servable index, true while work remains
func (s *sessionSearch) FTSRebuildStep(ctx context.Context, progress *backfillProgress) (bool, error) {
	if !s.fts.ftsEnabled.Load() {
		return false, nil
	}
	inserts := []string{formatFTSInsert(chunkInsertSQL, "messages_fts", "")}
	if s.fts.trigramAvailable.Load() {
		inserts = append(inserts, trigramChunkInsertSQL)
	}
	return s.rebuildStep(ctx, "fts_rebuild", inserts, s.ftsRebuildFinish, true, progress)
}

// Backfills one chunk of the CJK index, true while work remains
func (s *sessionSearch) FTSCJKRebuildStep(ctx context.Context, progress *backfillProgress) (bool, error) {
	if !s.fts.ftsEnabled.Load() || !s.fts.ftsCJKLoaded.Load() {
		return false, nil
	}
	insert := formatFTSInsert(chunkInsertSQL, "messages_fts_cjk", " AND role <> 'tool'")
	return s.rebuildStep(ctx, "fts_cjk_rebuild", []string{insert}, s.ftsCJKRebuildFinish, false, progress)
}

// Claims and indexes the next chunk of messages inside one write transaction, finishing once progress meets high water
func (s *sessionSearch) rebuildStep(ctx context.Context, prefix string, insertSQLs []string,
	finish func(context.Context) error, finishWhenEmpty bool, progress *backfillProgress) (bool, error) {
	raw, ok, err := s.host.getMeta(ctx, prefix+"_high_water")
	if err != nil || !ok {
		return false, err
	}
	highWater, err := pyint.Of(raw)
	if err != nil {
		return false, err
	}
	chunk := s.fts.ftsChunkRows
	var more bool
	var covered int
	err = s.host.executeWrite(ctx, func(conn SQLiteConn) error {
		more, covered = false, 0
		row, err := metaRow(ctx, conn, prefix+"_progress")
		if err != nil || row == nil {
			return err
		}
		doneN, err := pyint.Of(row[0])
		if err != nil {
			return err
		}
		if doneN.Cmp(highWater) >= 0 {
			return nil
		}
		done, err := sqliteInt(doneN)
		if err != nil {
			return err
		}
		hw, err := sqliteInt(highWater)
		if err != nil {
			return err
		}
		nth, err := fetchOne(ctx, conn, "SELECT id FROM messages WHERE id > ? AND id <= ? ORDER BY id LIMIT 1 OFFSET ?",
			done, hw, chunk-1)
		if err != nil {
			return err
		}
		upper, chunkCovered := hw, 0
		if nth != nil {
			n, err := pyint.Of(nth[0])
			if err != nil {
				return err
			}
			if upper, err = sqliteInt(n); err != nil {
				return err
			}
			chunkCovered = chunk
		}
		if nth == nil && progress != nil {
			countRow, err := fetchOne(ctx, conn, "SELECT COUNT(*) FROM messages WHERE id > ? AND id <= ?", done, upper)
			if err != nil {
				return err
			}
			chunkCovered = int(countRow[0].(int64))
		}
		for _, q := range insertSQLs {
			if _, err := conn.ExecContext(ctx, q, done, upper); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "UPDATE state_meta SET value = ? WHERE key = ?",
			fmt.Sprint(upper), prefix+"_progress"); err != nil {
			return err
		}
		more, covered = upper < hw, chunkCovered
		return nil
	}, nil)
	if err != nil {
		return false, err
	}
	if progress != nil {
		progress.indexed += covered
	}
	if !more {
		st, err := s.rebuildState(ctx, prefix)
		if err != nil {
			return false, err
		}
		if (finishWhenEmpty && highWater.Sign() <= 0) || (st != nil && st.indexed.Cmp(st.total) >= 0) {
			if err := finish(ctx); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	return true, nil
}

// Drains one chunk of the first demoted shadow table, dropping it once empty, true while trash remains
func (s *sessionSearch) ftsTeardownTrashStep(ctx context.Context, progress *teardownProgress) (bool, error) {
	limit := s.fts.ftsChunkRows
	var trash []string
	started := false
	err := s.host.readCtx(ctx, true, func(conn SQLiteConn) error {
		var err error
		trash, err = queryStrings(ctx, conn, "SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE ? ESCAPE '\\'",
			strings.ReplaceAll(ftsTrashPrefix, "_", "\\_")+"%")
		if err != nil {
			return err
		}
		if len(trash) > 0 && progress != nil && progress.table != trash[0] {
			started = true
			return s.ftsTeardownStartTable(ctx, conn, trash, progress)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if started {
		return true, nil
	}
	if len(trash) == 0 {
		return false, nil
	}
	tbl := trash[0]
	drop := func(conn SQLiteConn, markerKey string) (bool, error) {
		if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl); err != nil {
			return false, err
		}
		if markerKey != "" {
			if err := deleteMeta(ctx, conn, markerKey); err != nil {
				return false, err
			}
		}
		slog.Info(fmt.Sprintf("Old FTS shadow table %s torn down.", tbl))
		return true, nil
	}
	var more bool
	var deleted int64
	err = s.host.executeWrite(ctx, func(conn SQLiteConn) error {
		more, deleted = false, 0
		pkNames, pkTypes, err := teardownPrimaryKey(ctx, conn, tbl)
		if err != nil {
			return err
		}
		key := "rowid"
		if len(pkNames) > 0 {
			key = strings.Join(pkNames, ", ")
		}
		if len(pkNames) == 1 && pkTypes[0] == "INTEGER" {
			markerKey := "fts_teardown_" + tbl + "_progress"
			row, err := metaRow(ctx, conn, markerKey)
			if err != nil {
				return err
			}
			var highWater int64
			if row != nil {
				n, err := pyint.Of(row[0])
				if err != nil {
					return err
				}
				if highWater, err = sqliteInt(n); err != nil {
					return err
				}
			}
			upperRow, err := fetchOne(ctx, conn, fmt.Sprintf("SELECT %s FROM %s WHERE %s > ? ORDER BY %s LIMIT 1 OFFSET ?",
				key, tbl, key, key), highWater, limit-1)
			if err != nil {
				return err
			}
			if upperRow == nil {
				if upperRow, err = fetchOne(ctx, conn, fmt.Sprintf("SELECT MAX(%s) FROM %s WHERE %s > ?", key, tbl, key),
					highWater); err != nil {
					return err
				}
			}
			if upperRow == nil || upperRow[0] == nil {
				more, err = drop(conn, markerKey)
				return err
			}
			upper := upperRow[0]
			res, err := conn.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s > ? AND %s <= ?", tbl, key, key),
				highWater, upper)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n > 0 {
				if err := s.host.setMeta(ctx, markerKey, pyrepr.Str(upper), conn); err != nil {
					return err
				}
			}
			more, deleted = true, n
			return nil
		}
		res, err := conn.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE (%s) IN (SELECT %s FROM %s LIMIT %d)",
			tbl, key, key, tbl, limit))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		more, deleted = true, n
		if n == 0 {
			more, err = drop(conn, "")
		}
		return err
	}, nil)
	if err != nil {
		return false, err
	}
	if progress != nil {
		progress.drained += int(deleted)
	}
	return more, nil
}

// Primary key columns of a table in column order with their declared types uppercased
func teardownPrimaryKey(ctx context.Context, conn SQLiteConn, tbl string) ([]string, []string, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", tbl))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var names, types []string
	for rows.Next() {
		var cid, notnull, pk int64
		var name string
		var typ, dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, nil, err
		}
		if pk > 0 {
			declared := ""
			if pyTruthy(typ) {
				declared = errText(typ)
			}
			names = append(names, name)
			types = append(types, pystr.Upper(declared))
		}
	}
	return names, types, rows.Err()
}

// Points progress at the first trash table with the rows left past its resume marker and restarts the chunk size
func (s *sessionSearch) ftsTeardownStartTable(ctx context.Context, conn SQLiteConn, trash []string, progress *teardownProgress) error {
	tbl := trash[0]
	marker, err := metaRow(ctx, conn, "fts_teardown_"+tbl+"_progress")
	if err != nil {
		return err
	}
	where, params := "", []any{}
	if marker != nil {
		n, err := pyint.Of(marker[0])
		if err != nil {
			return err
		}
		bound, err := sqliteInt(n)
		if err != nil {
			return err
		}
		where, params = "WHERE rowid > ?", []any{bound}
	}
	tableCount := progress.tableCount
	if tableCount == 0 {
		tableCount = len(trash)
	}
	total := 0
	if progress.counted {
		row, err := fetchOne(ctx, conn, fmt.Sprintf("SELECT COUNT(*) FROM %s %s", tbl, where), params...)
		if err != nil {
			return err
		}
		total = int(row[0].(int64))
	}
	progress.tableCount, progress.total, progress.drained = tableCount, total, 0
	progress.tableIndex = max(1, tableCount-len(trash)+1)
	s.fts.ftsChunkRows = ftsRebuildChunkRows
	progress.table = tbl
	return nil
}

// Drops a stale CJK index with its triggers and markers, then recreates it outside the write transaction
func (s *sessionSearch) ftsCJKResetIfStale(ctx context.Context) error {
	if !s.fts.ftsCJKLoaded.Load() {
		return nil
	}
	reset := false
	err := s.host.executeWrite(ctx, func(conn SQLiteConn) error {
		reset = false
		row, err := metaRow(ctx, conn, FTSCJKStaleKey)
		if err != nil || row == nil {
			return err
		}
		for _, trig := range ftsCJKTriggers {
			if _, err := conn.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trig); err != nil {
				return err
			}
		}
		for _, stmt := range []string{"DROP TABLE IF EXISTS messages_fts_cjk", "DROP VIEW IF EXISTS messages_fts_cjk_src"} {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		if err := deleteMeta(ctx, conn, FTSCJKStaleKey, "fts_cjk_rebuild_high_water", "fts_cjk_rebuild_progress"); err != nil {
			return err
		}
		reset = true
		return nil
	}, nil)
	if err != nil || !reset {
		return err
	}
	return s.ensureCJKSchemaCommitted(ctx)
}

// Ensures the CJK index on the writer under the lock and commits
func (s *sessionSearch) ensureCJKSchemaCommitted(ctx context.Context) error {
	lock := s.host.writerLock()
	lock.Lock()
	defer lock.Unlock()
	conn, err := s.writer()
	if err != nil {
		return err
	}
	if err := s.fts.ensureFTSCJKSchema(ctx, conn); err != nil {
		return err
	}
	return endTransaction(ctx, conn, "COMMIT")
}

// Reports a base index holding nothing while messages has rows, an OperationalError counting as false
func (s *sessionSearch) ftsExternalIndexEmptyWithMessages(ctx context.Context, conn SQLiteConn) (bool, error) {
	row, err := fetchOne(ctx, conn, "SELECT EXISTS(SELECT 1 FROM messages)")
	if err == nil && pyTruthy(row[0]) {
		row, err = fetchOne(ctx, conn, "SELECT EXISTS(SELECT 1 FROM messages_fts_docsize)")
		if err == nil {
			return !pyTruthy(row[0]), nil
		}
	}
	if err != nil {
		if isOperationalError(err) {
			return false, nil
		}
		return false, err
	}
	return false, nil
}

// Resets the index to known empty and seeds zero progress when a high water has no progress marker
func (s *sessionSearch) reseedMissingProgress(ctx context.Context, conn SQLiteConn) error {
	row, err := metaRow(ctx, conn, "fts_rebuild_progress")
	if err != nil || row != nil {
		return err
	}
	empty, err := ftsIndexKnownEmpty(ctx, conn)
	if err != nil {
		return err
	}
	if !empty {
		if err := resetFTSIndexToEmpty(ctx, conn); err != nil {
			return err
		}
	}
	return s.host.setMeta(ctx, "fts_rebuild_progress", "0", conn)
}

// Reports the base external content index holds no rows, a missing table counting as empty
func ftsIndexKnownEmpty(ctx context.Context, conn SQLiteConn) (bool, error) {
	row, err := fetchOne(ctx, conn, "SELECT COUNT(*) FROM messages_fts_docsize")
	if err != nil {
		if isOperationalError(err) {
			return true, nil
		}
		return false, err
	}
	n, err := pyint.Of(row[0])
	if err != nil {
		return false, err
	}
	return n.Sign() == 0, nil
}

// Truncates the base and trigram indexes with FTS5 delete all, skipping absent tables
func resetFTSIndexToEmpty(ctx context.Context, conn SQLiteConn) error {
	for _, tbl := range []string{"messages_fts", "messages_fts_trigram"} {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s) VALUES('delete-all')", tbl, tbl)); err != nil && !isOperationalError(err) {
			return err
		}
	}
	return nil
}

// Writes full backfill markers returning the high water, without force only repairing a missing progress key
func (s *sessionSearch) seedFTSRebuildMarkers(ctx context.Context, conn SQLiteConn, force bool) (*big.Int, error) {
	existing, err := metaRow(ctx, conn, "fts_rebuild_high_water")
	if err != nil {
		return nil, err
	}
	if existing != nil && !force {
		if err := s.reseedMissingProgress(ctx, conn); err != nil {
			return nil, err
		}
		return pyint.Of(existing[0])
	}
	row, err := fetchOne(ctx, conn, "SELECT COALESCE(MAX(id), 0) FROM messages")
	if err != nil {
		return nil, err
	}
	if err := s.host.setMeta(ctx, "fts_rebuild_high_water", pyrepr.Str(row[0]), conn); err != nil {
		return nil, err
	}
	if err := s.host.setMeta(ctx, "fts_rebuild_progress", "0", conn); err != nil {
		return nil, err
	}
	return pyint.Of(row[0])
}

// Reseeds orphan progress and claims a full backfill for an empty external index, never on a legacy inline store
func (s *sessionSearch) repairOptimizeBookkeeping(ctx context.Context) error {
	return s.host.executeWrite(ctx, func(conn SQLiteConn) error {
		hw, err := metaRow(ctx, conn, "fts_rebuild_high_water")
		if err != nil {
			return err
		}
		if hw != nil {
			return s.reseedMissingProgress(ctx, conn)
		}
		legacy, err := dbHasLegacyInlineFTS(ctx, conn)
		if err != nil || legacy {
			return err
		}
		empty, err := s.ftsExternalIndexEmptyWithMessages(ctx, conn)
		if err != nil || !empty {
			return err
		}
		if err := deleteMeta(ctx, conn, "fts_storage_version"); err != nil {
			return err
		}
		_, err = s.seedFTSRebuildMarkers(ctx, conn, true)
		return err
	}, nil)
}

// Reports whether optimize storage has work, false without FTS5 or on a read only handle
func (s *sessionSearch) FTSOptimizeAvailable(ctx context.Context) (bool, error) {
	if !s.fts.ftsEnabled.Load() || s.host.readOnly() {
		return false, nil
	}
	available := false
	err := s.host.readCtx(ctx, true, func(conn SQLiteConn) error {
		available = false
		checks := []func() (bool, error){
			func() (bool, error) { return dbNeedsFTSStorageUpgrade(ctx, conn) },
			func() (bool, error) { return metaPresent(ctx, conn, "fts_rebuild_high_water") },
			func() (bool, error) {
				if !s.fts.ftsCJKLoaded.Load() {
					return false, nil
				}
				if ok, err := metaPresent(ctx, conn, "fts_cjk_rebuild_high_water"); err != nil || ok {
					return ok, err
				}
				return metaPresent(ctx, conn, FTSCJKStaleKey)
			},
			func() (bool, error) { return s.fts.hasFTSTrash(ctx, conn) },
			func() (bool, error) { return s.ftsExternalIndexEmptyWithMessages(ctx, conn) },
		}
		for _, check := range checks {
			ok, err := check()
			if err != nil || ok {
				available = ok
				return err
			}
		}
		return nil
	})
	return available, err
}

// Reports a state_meta row exists for key
func metaPresent(ctx context.Context, conn SQLiteConn, key string) (bool, error) {
	row, err := metaRow(ctx, conn, key)
	return row != nil, err
}

// Demotes legacy FTS vtables to plain trash tables with backfill markers, then recreates the v23 tables
func (s *sessionSearch) demoteLegacyFTSToTrash(ctx context.Context) (*big.Int, error) {
	var hw *big.Int
	err := s.host.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := dropFTSTriggers(ctx, conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DROP VIEW IF EXISTS messages_fts_trigram_src"); err != nil {
			return err
		}
		legacy, err := rowExists(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name IN ('messages_fts', 'messages_fts_trigram') "+
			"AND sql LIKE 'CREATE VIRTUAL TABLE%' LIMIT 1")
		if err != nil {
			return err
		}
		if legacy {
			for _, stmt := range []string{"PRAGMA writable_schema=ON",
				"DELETE FROM sqlite_master WHERE type = 'table' " +
					"AND name IN ('messages_fts', 'messages_fts_trigram') AND sql LIKE 'CREATE VIRTUAL TABLE%'",
				"PRAGMA writable_schema=RESET"} {
				if _, err := conn.ExecContext(ctx, stmt); err != nil {
					return err
				}
			}
			shadows, err := queryStrings(ctx, conn, "SELECT name FROM sqlite_master WHERE type = 'table' "+
				"AND (name LIKE 'messages_fts_%' ESCAPE '\\' "+
				"OR name LIKE 'messages_fts_trigram_%' ESCAPE '\\') "+
				"AND name NOT LIKE 'messages\\_fts\\_cjk%' ESCAPE '\\'")
			if err != nil {
				return err
			}
			for _, name := range shadows {
				if _, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s RENAME TO fts_v22_trash_%s", name, name)); err != nil {
					return err
				}
			}
		}
		if hw, err = s.seedFTSRebuildMarkers(ctx, conn, true); err != nil {
			return err
		}
		return deleteMeta(ctx, conn, "fts_optimize_available")
	}, nil)
	if err != nil {
		return nil, err
	}
	if err := s.ensureV23FTSTables(ctx, "failed to create v23 messages_fts during optimize-storage demote"); err != nil {
		return nil, err
	}
	return hw, nil
}

// Ensures the v23 base and trigram tables under the lock, an OperationalError of failureMessage without the base
func (s *sessionSearch) ensureV23FTSTables(ctx context.Context, failureMessage string) error {
	lock := s.host.writerLock()
	lock.Lock()
	defer lock.Unlock()
	conn, err := s.writer()
	if err != nil {
		return err
	}
	baseOK, err := s.fts.ensureFTSSchema(ctx, conn, "messages_fts", FTSSQL)
	if err != nil {
		return err
	}
	trigramOK, err := s.fts.ensureFTSSchema(ctx, conn, "messages_fts_trigram", FTSTrigramSQL)
	if err != nil {
		return err
	}
	s.fts.trigramAvailable.Store(trigramOK)
	if !baseOK {
		return &SQLiteError{Class: SQLiteOperationalError, Msg: failureMessage}
	}
	return endTransaction(ctx, conn, "COMMIT")
}

// Messages at or below the pass high water and how many its resume marker covers, nil when nothing is pending
func (s *sessionSearch) backfillProgress(ctx context.Context, phase, prefix string) (*backfillProgress, error) {
	st, err := s.rebuildState(ctx, prefix)
	if err != nil || st == nil {
		return nil, err
	}
	indexed, err := sqliteInt(st.indexed)
	if err != nil {
		return nil, err
	}
	total, err := sqliteInt(st.total)
	if err != nil {
		return nil, err
	}
	var count, done int64
	err = s.host.readCtx(ctx, true, func(conn SQLiteConn) error {
		return conn.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(id <= ?), 0) FROM messages WHERE +id <= ?",
			indexed, total).Scan(&count, &done)
	})
	if err != nil {
		return nil, err
	}
	return &backfillProgress{phase: phase, total: int(count), indexed: int(done)}, nil
}

// Copy of a progress payload with the paced flag appended
func withPaced(info *pyjson.Object, paced bool) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range info.Keys {
		out.Set(k, info.Vals[k])
	}
	out.Set("paced", paced)
	return out
}

// Runs step to completion with adaptive chunk sizes, pacing while a foreign holder is seen and retrying OperationalErrors
func (s *sessionSearch) optimizeDrive(step func() (bool, error), info func() *pyjson.Object,
	emit func(*pyjson.Object), watch *holderWatch) error {
	s.fts.ftsChunkRows = ftsRebuildChunkRows
	failures := 0
	for {
		t0 := monotonicSeconds()
		more, err := step()
		if err != nil {
			if !isOperationalError(err) {
				return err
			}
			failures++
			if failures >= s.optimizeChunkAttempts {
				return err
			}
			slog.Warn(fmt.Sprintf("FTS optimize chunk failed (attempt %d, retrying): %s", failures, errText(err)))
			searchSleep(0.25 * float64(failures))
			continue
		}
		failures = 0
		if !more {
			return nil
		}
		cost, paced := monotonicSeconds()-t0, watch.held()
		if info != nil {
			emit(withPaced(info(), paced))
		}
		target := ftsRebuildChunkTargetS
		if paced {
			target = ftsRebuildPacedTargetS
		}
		s.fts.ftsChunkRows = s.fts.nextChunkRows(s.fts.ftsChunkRows, cost, target)
		if paced {
			searchSleep(max(ftsRebuildMinPause, cost*ftsRebuildDutyFactor))
		}
	}
}

// Runs VACUUM then a passive checkpoint, ticking vacuum progress from a side goroutine, false when VACUUM failed
func (s *sessionSearch) optimizeVacuum(ctx context.Context, tick func(*pyjson.Object)) (bool, error) {
	var ticker sync.WaitGroup
	stop := make(chan struct{})
	if tick != nil {
		var pageSize, pages, free int64
		err := s.host.readCtx(ctx, true, func(conn SQLiteConn) error {
			for _, p := range []struct {
				name string
				dst  *int64
			}{{"page_size", &pageSize}, {"page_count", &pages}, {"freelist_count", &free}} {
				if err := conn.QueryRowContext(ctx, "PRAGMA "+p.name).Scan(p.dst); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return false, err
		}
		state := newVacuumProgress(pages*pageSize, (pages-free)*pageSize)
		interval := time.Duration(s.vacuumTickS * float64(time.Second))
		ticker.Add(1)
		go func() {
			defer ticker.Done()
			tick(state.info())
			for {
				select {
				case <-stop:
					return
				case <-time.After(interval):
					tick(state.info())
				}
			}
		}()
	}
	vacuumOK, err := func() (bool, error) {
		defer func() {
			close(stop)
			ticker.Wait()
		}()
		lock := s.host.writerLock()
		lock.Lock()
		defer lock.Unlock()
		conn, err := s.writer()
		if err == nil {
			_, err = conn.ExecContext(ctx, "VACUUM")
		}
		if err != nil {
			if isOperationalError(err) {
				slog.Warn(fmt.Sprintf("VACUUM after FTS optimize failed: %s", errText(err)))
				return false, nil
			}
			return false, err
		}
		return true, nil
	}()
	if err != nil {
		return false, err
	}
	func() {
		lock := s.host.writerLock()
		lock.Lock()
		defer lock.Unlock()
		conn, err := s.writer()
		if err == nil {
			_, err = conn.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
		}
		if err != nil {
			slog.Debug(fmt.Sprintf("WAL checkpoint (PASSIVE) after optimize VACUUM failed: %s", errText(err)))
		}
	}()
	return vacuumOK, nil
}

// Stamps the current FTS layout inside the write transaction, returning the refusal reason or empty when settled
func (s *sessionSearch) optimizeSettle(ctx context.Context, conn SQLiteConn) (string, error) {
	pending, err := metaPresent(ctx, conn, "fts_rebuild_high_water")
	if err != nil || pending {
		return refusalIf(pending, "backfill_incomplete"), err
	}
	trash, err := s.fts.hasFTSTrash(ctx, conn)
	if err != nil || trash {
		return refusalIf(trash, "teardown_incomplete"), err
	}
	empty, err := s.ftsExternalIndexEmptyWithMessages(ctx, conn)
	if err != nil || empty {
		return refusalIf(empty, "backfill_incomplete"), err
	}
	current, err := FTSLayoutCurrent(ctx, conn)
	if err != nil {
		return "", err
	}
	if !current {
		drift, err := FTSLayoutDrift(ctx, conn)
		if err != nil {
			return "", err
		}
		if len(drift) > 0 {
			return "layout_drift", nil
		}
	}
	if current {
		if err := s.host.setMeta(ctx, "fts_storage_version", fmt.Sprint(FTSStorageVersion), conn); err != nil {
			return "", err
		}
	}
	if err := deleteMeta(ctx, conn, "fts_optimize_available"); err != nil {
		return "", err
	}
	_, err = conn.ExecContext(ctx, "UPDATE schema_version SET version = ? WHERE version < ?", SchemaVersion, SchemaVersion)
	return "", err
}

// Reason when the condition holds, empty otherwise
func refusalIf(cond bool, reason string) string {
	if cond {
		return reason
	}
	return ""
}

// Repairs an older FTS layout into the v23 shape to completion, reporting ok, reason, vacuumed and vacuum_skipped
func (s *sessionSearch) OptimizeFTSStorage(ctx context.Context, progressCB func(*pyjson.Object), vacuum bool) (*pyjson.Object, error) {
	result := func(pairs ...any) *pyjson.Object {
		out := pyjson.NewObject()
		for i := 0; i < len(pairs); i += 2 {
			out.Set(pairs[i].(string), pairs[i+1])
		}
		return out
	}
	if !s.fts.ftsEnabled.Load() {
		return result("ok", false, "reason", "fts5_unavailable"), nil
	}
	if s.host.readOnly() {
		return result("ok", false, "reason", "read_only"), nil
	}
	if err := s.repairOptimizeBookkeeping(ctx); err != nil {
		return nil, err
	}
	needsStorageUpgrade, err := func() (bool, error) {
		lock := s.host.writerLock()
		lock.Lock()
		defer lock.Unlock()
		conn, err := s.writer()
		if err != nil {
			return false, err
		}
		return dbNeedsFTSStorageUpgrade(ctx, conn)
	}()
	if err != nil {
		return nil, err
	}
	_, pending, err := s.host.getMeta(ctx, "fts_rebuild_high_water")
	if err != nil {
		return nil, err
	}
	if needsStorageUpgrade && !pending {
		if _, err := s.demoteLegacyFTSToTrash(ctx); err != nil {
			return nil, err
		}
	} else if pending && !needsStorageUpgrade {
		if err := s.ensureV23FTSTables(ctx, "failed to re-create v23 messages_fts on optimize-storage resume"); err != nil {
			return nil, err
		}
	}
	if err := s.ftsCJKResetIfStale(ctx); err != nil {
		return nil, err
	}
	if s.fts.ftsCJKLoaded.Load() {
		if err := s.ensureCJKSchemaCommitted(ctx); err != nil {
			return nil, err
		}
	}
	emit := progressCB
	if emit == nil {
		emit = func(*pyjson.Object) {}
	}
	teardown := newTeardownProgress()
	teardown.counted = progressCB != nil
	if err := s.optimizePasses(ctx, progressCB, emit, teardown); err != nil {
		return nil, err
	}
	var stillPending, stillTrash, emptyIndex bool
	err = s.host.readCtx(ctx, true, func(conn SQLiteConn) error {
		var err error
		if stillPending, err = metaPresent(ctx, conn, "fts_rebuild_high_water"); err != nil {
			return err
		}
		if stillTrash, err = s.fts.hasFTSTrash(ctx, conn); err != nil {
			return err
		}
		emptyIndex, err = s.ftsExternalIndexEmptyWithMessages(ctx, conn)
		return err
	})
	if err != nil {
		return nil, err
	}
	if stillPending || stillTrash || emptyIndex {
		reason := "teardown_incomplete"
		if stillPending || emptyIndex {
			reason = "backfill_incomplete"
		}
		slog.Warn(fmt.Sprintf("FTS storage optimization did not settle (%s): pending=%s trash=%s empty_index=%s",
			reason, pyrepr.Str(stillPending), pyrepr.Str(stillTrash), pyrepr.Str(emptyIndex)))
		return result("ok", false, "reason", reason, "vacuumed", nil), nil
	}
	var vacuumOK any
	vacuumSkipped := false
	if vacuum {
		holders, err := s.host.foreignStateDBHolders()
		if err != nil {
			return nil, err
		}
		if len(holders) > 0 {
			slog.Info(fmt.Sprintf("Skipping VACUUM after FTS optimize: another process is using %s", s.host.dbPath()))
			vacuumOK, vacuumSkipped = false, true
		} else {
			ok, err := s.optimizeVacuum(ctx, progressCB)
			if err != nil {
				return nil, err
			}
			vacuumOK = ok
		}
	}
	var refusal string
	err = s.host.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		refusal, err = s.optimizeSettle(ctx, conn)
		return err
	}, nil)
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		slog.Warn(fmt.Sprintf("FTS storage optimization settle refused (%s)", refusal))
		return result("ok", false, "reason", refusal, "vacuumed", vacuumOK), nil
	}
	emit(result("phase", "done", "percent", 100, "indexed", 0, "total", 0))
	slog.Info(fmt.Sprintf("FTS storage optimization complete (layout v%d).", FTSStorageVersion))
	out := result("ok", true, "vacuumed", vacuumOK)
	if vacuumSkipped {
		out.Set("vacuum_skipped", "in_use")
	}
	return out, nil
}

// Runs the base and CJK backfills then the trash teardown while the holder watch paces chunks
func (s *sessionSearch) optimizePasses(ctx context.Context, progressCB, emit func(*pyjson.Object), teardown *teardownProgress) error {
	watch, err := newHolderWatch(func() (bool, error) {
		holders, err := s.host.foreignStateDBHolders()
		return len(holders) > 0, err
	}, s.holderScanIntervalS).enter()
	if err != nil {
		return err
	}
	defer watch.exit()
	passes := []struct {
		phase, prefix string
		step          func(context.Context, *backfillProgress) (bool, error)
	}{{"backfill", "fts_rebuild", s.FTSRebuildStep}, {"backfill_cjk", "fts_cjk_rebuild", s.FTSCJKRebuildStep}}
	for _, pass := range passes {
		var bf *backfillProgress
		if progressCB != nil {
			if bf, err = s.backfillProgress(ctx, pass.phase, pass.prefix); err != nil {
				return err
			}
		}
		var info func() *pyjson.Object
		if bf != nil {
			emit(withPaced(bf.info(), watch.held()))
			info = bf.info
		}
		step := pass.step
		if err := s.optimizeDrive(func() (bool, error) { return step(ctx, bf) }, info, emit, watch); err != nil {
			return err
		}
	}
	return s.optimizeDrive(func() (bool, error) { return s.ftsTeardownTrashStep(ctx, teardown) }, teardown.info, emit, watch)
}
