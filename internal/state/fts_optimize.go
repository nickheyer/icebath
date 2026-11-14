package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ftsRebuildChunkRows    = 500
	ftsRebuildChunkMinRows = 50
	ftsRebuildChunkMaxRows = 100_000
	ftsRebuildChunkTarget  = 250 * time.Millisecond
	ftsRebuildPacedTarget  = 50 * time.Millisecond
	ftsRebuildDutyFactor   = 4.0
	ftsRebuildMinPause     = 200 * time.Millisecond
	optimizeChunkAttempts  = 3
	optimizeVacuumTick     = time.Second
	holderScanInterval     = 2 * time.Second
)

var (
	ftsBaseBoundarySweepSQL = "INSERT INTO messages_fts(rowid, content, tool_name, tool_calls) " +
		"SELECT m.id, CASE WHEN m.role = 'tool' THEN substr(COALESCE(m.content, ''), 1, ?) " +
		"ELSE m.content END, m.tool_name, m.tool_calls FROM messages m WHERE m.id > ? AND m.id <= ? " +
		"AND NOT EXISTS (SELECT 1 FROM messages_fts_docsize d WHERE d.id = m.id)"
	ftsTrigramBoundarySweepSQL = "INSERT INTO messages_fts_trigram(rowid, content, tool_name) " +
		"SELECT m.id, m.content, m.tool_name FROM messages m JOIN sessions s ON s.id = m.session_id " +
		"WHERE m.id > ? AND m.id <= ? AND m.role <> 'tool' AND " + FTSTrigramSessionSQL("s") + " " +
		"AND NOT EXISTS (SELECT 1 FROM messages_fts_trigram_docsize d WHERE d.id = m.id)"
	ftsCJKBoundarySweepSQL = "INSERT INTO messages_fts_cjk(rowid, content, tool_name, tool_calls) " +
		"SELECT m.id, m.content, m.tool_name, m.tool_calls FROM messages m WHERE m.id > ? AND m.id <= ? AND m.role <> 'tool' " +
		"AND NOT EXISTS (SELECT 1 FROM messages_fts_cjk_docsize d WHERE d.id = m.id)"
	ftsTrigramChunkInsertSQL = "INSERT INTO messages_fts_trigram(rowid, content, tool_name) " +
		"SELECT m.id, m.content, m.tool_name FROM messages m JOIN sessions s ON s.id = m.session_id " +
		"WHERE m.id > ? AND m.id <= ? AND m.role <> 'tool' AND " + FTSTrigramSessionSQL("s")
)

func ftsChunkInsertSQL(table, extra string) string {
	return "INSERT INTO " + table + "(rowid, content, tool_name, tool_calls) " +
		"SELECT id, CASE WHEN role = 'tool' " +
		"THEN substr(COALESCE(content, ''), 1, " + strconv.Itoa(FTSToolContentPrefixChars) + ") " +
		"ELSE content END, tool_name, tool_calls FROM messages WHERE id > ? AND id <= ?" + extra
}

type RebuildStatus struct {
	Total   int64
	Indexed int64
	Percent int
}

func (s *Store) FTSRebuildStatus(ctx context.Context) (RebuildStatus, bool, error) {
	return s.rebuildStatus(ctx, "fts_rebuild")
}

func (s *Store) rebuildStatus(ctx context.Context, prefix string) (RebuildStatus, bool, error) {
	meta := map[string]string{}
	err := s.Read(ctx, func(c Conn) error {
		clear(meta)
		rows, err := c.QueryContext(ctx, "SELECT key, value FROM state_meta WHERE key IN (?, ?)", prefix+"_high_water", prefix+"_progress")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k, v sql.NullString
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			meta[k.String] = v.String
		}
		return rows.Err()
	})
	if err != nil {
		return RebuildStatus{}, false, err
	}
	raw, ok := meta[prefix+"_high_water"]
	if !ok {
		return RebuildStatus{}, false, nil
	}
	total, err := metaInteger(raw)
	if err != nil || total <= 0 {
		return RebuildStatus{}, false, err
	}
	var progress int64
	if p := meta[prefix+"_progress"]; p != "" {
		if progress, err = metaInteger(p); err != nil {
			return RebuildStatus{}, false, err
		}
	}
	return RebuildStatus{Total: total, Indexed: progress, Percent: min(100, int(100*float64(progress)/float64(total)))}, true, nil
}

func metaInteger(raw string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("state_meta holds a non-integer marker %q: %w", raw, err)
	}
	return n, nil
}

type OptimizeProgress struct {
	Phase          string
	Percent        int
	Indexed        int64
	Total          int64
	Paced          bool
	Table          string
	TableIndex     int
	TableCount     int
	Elapsed        time.Duration
	SizeBytes      int64
	EstimatedBytes int64
}

type backfillProgress struct {
	phase   string
	total   int64
	indexed int64
}

func (b *backfillProgress) info() OptimizeProgress {
	percent := 100
	if b.total > 0 {
		percent = int(min(100, 100*b.indexed/b.total))
	}
	return OptimizeProgress{Phase: b.phase, Percent: percent, Indexed: min(b.indexed, b.total), Total: b.total}
}

type teardownProgress struct {
	table      string
	tableIndex int
	tableCount int
	total      int64
	drained    int64
	counted    bool
}

func (t *teardownProgress) info() OptimizeProgress {
	percent := 100
	if t.total > 0 {
		percent = int(min(100, 100*t.drained/t.total))
	}
	return OptimizeProgress{Phase: "teardown", Percent: percent, Indexed: t.drained, Total: t.total, Table: t.table,
		TableIndex: t.tableIndex, TableCount: t.tableCount}
}

type holderWatch struct {
	held atomic.Bool
	stop chan struct{}
	done sync.WaitGroup
}

func (s *Store) watchHolders() *holderWatch {
	w := &holderWatch{stop: make(chan struct{})}
	refresh := func() { w.held.Store(len(ForeignHolders(s.path)) > 0) }
	refresh()
	w.done.Add(1)
	go func() {
		defer w.done.Done()
		t := time.NewTicker(holderScanInterval)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
				refresh()
			}
		}
	}()
	return w
}

func (w *holderWatch) close() {
	close(w.stop)
	w.done.Wait()
}

type ftsOptimizer struct {
	s     *Store
	chunk int
}

func nextChunkRows(rows int, cost, target time.Duration) int {
	scale := target.Seconds() / max(cost.Seconds(), 1e-4)
	next := int(float64(rows) * min(2.0, max(0.5, scale)))
	return min(ftsRebuildChunkMaxRows, max(ftsRebuildChunkMinRows, next))
}

func (o *ftsOptimizer) rebuildStep(ctx context.Context, prefix string, inserts []string, finish func(ctx context.Context) error,
	finishWhenEmpty bool, progress *backfillProgress) (bool, error) {
	s := o.s
	raw, ok, err := s.GetMeta(ctx, prefix+"_high_water")
	if err != nil || !ok {
		return false, err
	}
	highWater, err := metaInteger(raw)
	if err != nil {
		return false, err
	}
	chunk := o.chunk
	var more bool
	var covered int64
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		more, covered = false, 0
		doneRaw, ok, err := metaValue(ctx, c, prefix+"_progress")
		if err != nil || !ok {
			return err
		}
		done, err := metaInteger(doneRaw)
		if err != nil || done >= highWater {
			return err
		}
		var nth int64
		upper := highWater
		switch err := c.QueryRowContext(ctx, "SELECT id FROM messages WHERE id > ? AND id <= ? ORDER BY id LIMIT 1 OFFSET ?",
			done, highWater, chunk-1).Scan(&nth); {
		case err == nil:
			upper, covered = nth, int64(chunk)
		case errors.Is(err, sql.ErrNoRows):
			if progress != nil {
				if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE id > ? AND id <= ?", done, upper).Scan(&covered); err != nil {
					return err
				}
			}
		default:
			return err
		}
		for _, q := range inserts {
			if _, err := c.ExecContext(ctx, q, done, upper); err != nil {
				return err
			}
		}
		if _, err := c.ExecContext(ctx, "UPDATE state_meta SET value = ? WHERE key = ?", strconv.FormatInt(upper, 10), prefix+"_progress"); err != nil {
			return err
		}
		more = upper < highWater
		return nil
	})
	if err != nil {
		return false, err
	}
	if progress != nil {
		progress.indexed += covered
	}
	if more {
		return true, nil
	}
	status, pending, err := s.rebuildStatus(ctx, prefix)
	if err != nil {
		return false, err
	}
	if (finishWhenEmpty && highWater <= 0) || (pending && status.Indexed >= status.Total) {
		if err := finish(ctx); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (o *ftsOptimizer) baseRebuildStep(ctx context.Context, progress *backfillProgress) (bool, error) {
	f := o.s.fts()
	enabled, trigram := ftsGet(f, func(f *ftsFlags) bool { return f.enabled }), ftsGet(f, func(f *ftsFlags) bool { return f.trigram })
	if !enabled {
		return false, nil
	}
	inserts := []string{ftsChunkInsertSQL("messages_fts", "")}
	if trigram {
		inserts = append(inserts, ftsTrigramChunkInsertSQL)
	}
	return o.rebuildStep(ctx, "fts_rebuild", inserts, o.s.ftsRebuildFinish, true, progress)
}

func (o *ftsOptimizer) cjkRebuildStep(ctx context.Context, progress *backfillProgress) (bool, error) {
	f := o.s.fts()
	if !ftsGet(f, func(f *ftsFlags) bool { return f.enabled && f.cjkLoaded }) {
		return false, nil
	}
	return o.rebuildStep(ctx, "fts_cjk_rebuild", []string{ftsChunkInsertSQL("messages_fts_cjk", " AND role <> 'tool'")},
		o.s.ftsCJKRebuildFinish, false, progress)
}

type boundarySweep struct {
	query   string
	bounded bool
}

func (s *Store) ftsRebuildFinish(ctx context.Context) error {
	sweeps := []boundarySweep{{ftsBaseBoundarySweepSQL, true}}
	if ftsGet(s.fts(), func(f *ftsFlags) bool { return f.trigram }) {
		sweeps = append(sweeps, boundarySweep{ftsTrigramBoundarySweepSQL, false})
	}
	if err := s.rebuildFinish(ctx, "fts_rebuild", sweeps); err != nil {
		return err
	}
	slog.Info("Deferred FTS rebuild complete — all messages indexed.")
	return nil
}

func (s *Store) ftsCJKRebuildFinish(ctx context.Context) error {
	if err := s.rebuildFinish(ctx, "fts_cjk_rebuild", []boundarySweep{{ftsCJKBoundarySweepSQL, false}}); err != nil {
		return err
	}
	s.fts().set(func(f *ftsFlags) { f.cjkAvailable = true })
	slog.Info("CJK FTS index backfill complete — serving CJK search.")
	return nil
}

func (s *Store) rebuildFinish(ctx context.Context, prefix string, sweeps []boundarySweep) error {
	return s.Write(ctx, WritePatience, func(c Conn) error {
		raw, ok, err := metaValue(ctx, c, prefix+"_high_water")
		if err != nil {
			return err
		}
		if ok {
			hw, err := metaInteger(raw)
			if err != nil {
				return err
			}
			for _, sw := range sweeps {
				var args []any
				if sw.bounded {
					args = append(args, FTSToolContentPrefixChars)
				}
				if _, err := c.ExecContext(ctx, sw.query, append(args, hw-1000, hw+1000)...); err != nil {
					return err
				}
			}
		}
		return deleteMeta(ctx, c, prefix+"_high_water", prefix+"_progress")
	})
}

func ftsTrashTables(ctx context.Context, c Conn) ([]string, error) {
	return queryStrings(ctx, c, `SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE ? ESCAPE '\'`,
		strings.ReplaceAll(ftsTrashPrefix, "_", `\_`)+"%")
}

func (o *ftsOptimizer) teardownStep(ctx context.Context, progress *teardownProgress) (bool, error) {
	s := o.s
	limit := o.chunk
	var trash []string
	started := false
	err := s.Read(ctx, func(c Conn) error {
		var err error
		if trash, err = ftsTrashTables(ctx, c); err != nil {
			return err
		}
		if len(trash) > 0 && progress != nil && progress.table != trash[0] {
			started = true
			return o.teardownStartTable(ctx, c, trash, progress)
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
	var more, dropped bool
	var deleted int64
	drop := func(c Conn, markerKey string) error {
		if _, err := c.ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl); err != nil {
			return err
		}
		if markerKey != "" {
			if err := deleteMeta(ctx, c, markerKey); err != nil {
				return err
			}
		}
		dropped = true
		return nil
	}
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		more, dropped, deleted = false, false, 0
		rows, err := c.QueryContext(ctx, "PRAGMA table_info("+tbl+")")
		if err != nil {
			return err
		}
		var names, types []string
		for rows.Next() {
			var cid, notNull, pk int64
			var name, typ sql.NullString
			var dflt any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
				_ = rows.Close()
				return err
			}
			if pk > 0 {
				names, types = append(names, name.String), append(types, strings.ToUpper(typ.String))
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		key := "rowid"
		if len(names) > 0 {
			key = strings.Join(names, ", ")
		}
		if len(names) == 1 && types[0] == "INTEGER" {
			markerKey := "fts_teardown_" + tbl + "_progress"
			var highWater int64
			if raw, ok, err := metaValue(ctx, c, markerKey); err != nil {
				return err
			} else if ok {
				if highWater, err = metaInteger(raw); err != nil {
					return err
				}
			}
			var upper sql.NullInt64
			err := c.QueryRowContext(ctx, "SELECT "+key+" FROM "+tbl+" WHERE "+key+" > ? ORDER BY "+key+" LIMIT 1 OFFSET ?",
				highWater, limit-1).Scan(&upper)
			if errors.Is(err, sql.ErrNoRows) {
				err = c.QueryRowContext(ctx, "SELECT MAX("+key+") FROM "+tbl+" WHERE "+key+" > ?", highWater).Scan(&upper)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if !upper.Valid {
				more = true
				return drop(c, markerKey)
			}
			res, err := c.ExecContext(ctx, "DELETE FROM "+tbl+" WHERE "+key+" > ? AND "+key+" <= ?", highWater, upper.Int64)
			if err != nil {
				return err
			}
			if deleted, err = res.RowsAffected(); err != nil {
				return err
			}
			if deleted > 0 {
				if err := SetMetaIn(ctx, c, markerKey, strconv.FormatInt(upper.Int64, 10)); err != nil {
					return err
				}
			}
			more = true
			return nil
		}
		res, err := c.ExecContext(ctx, "DELETE FROM "+tbl+" WHERE ("+key+") IN (SELECT "+key+" FROM "+tbl+" LIMIT "+strconv.Itoa(limit)+")")
		if err != nil {
			return err
		}
		if deleted, err = res.RowsAffected(); err != nil {
			return err
		}
		more = true
		if deleted == 0 {
			return drop(c, "")
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if dropped {
		slog.Info(fmt.Sprintf("Old FTS shadow table %s torn down.", tbl))
	}
	if progress != nil {
		progress.drained += deleted
	}
	return more, nil
}

func (o *ftsOptimizer) teardownStartTable(ctx context.Context, c Conn, trash []string, progress *teardownProgress) error {
	tbl := trash[0]
	where, args := "", []any{}
	raw, ok, err := metaValue(ctx, c, "fts_teardown_"+tbl+"_progress")
	if err != nil {
		return err
	}
	if ok {
		marker, err := metaInteger(raw)
		if err != nil {
			return err
		}
		where, args = "WHERE rowid > ?", []any{marker}
	}
	tableCount := progress.tableCount
	if tableCount == 0 {
		tableCount = len(trash)
	}
	var total int64
	if progress.counted {
		if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl+" "+where, args...).Scan(&total); err != nil {
			return err
		}
	}
	progress.tableCount, progress.total, progress.drained = tableCount, total, 0
	progress.tableIndex = max(1, tableCount-len(trash)+1)
	o.chunk = ftsRebuildChunkRows
	progress.table = tbl
	return nil
}

func (s *Store) ftsCJKResetIfStale(ctx context.Context) error {
	if !ftsGet(s.fts(), func(f *ftsFlags) bool { return f.cjkLoaded }) {
		return nil
	}
	reset := false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		reset = false
		_, stale, err := metaValue(ctx, c, ftsCJKStaleKey)
		if err != nil || !stale {
			return err
		}
		for _, trig := range ftsCJKTriggers {
			if _, err := c.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trig); err != nil {
				return err
			}
		}
		for _, q := range []string{"DROP TABLE IF EXISTS messages_fts_cjk", "DROP VIEW IF EXISTS messages_fts_cjk_src"} {
			if _, err := c.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		if err := deleteMeta(ctx, c, ftsCJKStaleKey, "fts_cjk_rebuild_high_water", "fts_cjk_rebuild_progress"); err != nil {
			return err
		}
		reset = true
		return nil
	})
	if err != nil || !reset {
		return err
	}
	return s.ensureCJKSchemaCommitted(ctx)
}

func (s *Store) ensureCJKSchemaCommitted(ctx context.Context) error {
	return s.withWriter(ctx, "write", func(c Conn) error { return s.ensureFTSCJKSchema(ctx, c) })
}

func ftsIndexKnownEmpty(ctx context.Context, c Conn) (bool, error) {
	var n int64
	if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages_fts_docsize").Scan(&n); err != nil {
		if familyOf(err) == callFamily {
			return true, nil
		}
		return false, err
	}
	return n == 0, nil
}

func resetFTSIndexToEmpty(ctx context.Context, c Conn) error {
	for _, tbl := range []string{"messages_fts", "messages_fts_trigram"} {
		if _, err := c.ExecContext(ctx, "INSERT INTO "+tbl+"("+tbl+") VALUES('delete-all')"); err != nil && familyOf(err) != callFamily {
			return err
		}
	}
	return nil
}

func reseedMissingProgress(ctx context.Context, c Conn) error {
	_, ok, err := metaValue(ctx, c, "fts_rebuild_progress")
	if err != nil || ok {
		return err
	}
	empty, err := ftsIndexKnownEmpty(ctx, c)
	if err != nil {
		return err
	}
	if !empty {
		if err := resetFTSIndexToEmpty(ctx, c); err != nil {
			return err
		}
	}
	return SetMetaIn(ctx, c, "fts_rebuild_progress", "0")
}

func seedFTSRebuildMarkers(ctx context.Context, c Conn, force bool) (int64, error) {
	raw, ok, err := metaValue(ctx, c, "fts_rebuild_high_water")
	if err != nil {
		return 0, err
	}
	if ok && !force {
		if err := reseedMissingProgress(ctx, c); err != nil {
			return 0, err
		}
		return metaInteger(raw)
	}
	var hw int64
	if err := c.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM messages").Scan(&hw); err != nil {
		return 0, err
	}
	if err := SetMetaIn(ctx, c, "fts_rebuild_high_water", strconv.FormatInt(hw, 10)); err != nil {
		return 0, err
	}
	return hw, SetMetaIn(ctx, c, "fts_rebuild_progress", "0")
}

func (s *Store) repairOptimizeBookkeeping(ctx context.Context) error {
	return s.Write(ctx, WritePatience, func(c Conn) error {
		if _, ok, err := metaValue(ctx, c, "fts_rebuild_high_water"); err != nil {
			return err
		} else if ok {
			return reseedMissingProgress(ctx, c)
		}
		if legacy, err := dbHasLegacyInlineFTS(ctx, c); err != nil || legacy {
			return err
		}
		empty, err := ftsExternalIndexEmptyWithMessages(ctx, c)
		if err != nil || !empty {
			return err
		}
		if err := deleteMeta(ctx, c, "fts_storage_version"); err != nil {
			return err
		}
		_, err = seedFTSRebuildMarkers(ctx, c, true)
		return err
	})
}

func (s *Store) FTSOptimizeAvailable(ctx context.Context) (bool, error) {
	if !ftsGet(s.fts(), func(f *ftsFlags) bool { return f.enabled }) || s.readOnly {
		return false, nil
	}
	cjkLoaded := ftsGet(s.fts(), func(f *ftsFlags) bool { return f.cjkLoaded })
	available := false
	err := s.Read(ctx, func(c Conn) error {
		available = false
		drift, err := ftsLayoutDrift(ctx, c)
		if err != nil || len(drift) > 0 {
			available = err == nil
			return err
		}
		keys := []string{"fts_rebuild_high_water"}
		if cjkLoaded {
			keys = append(keys, "fts_cjk_rebuild_high_water", ftsCJKStaleKey)
		}
		for _, k := range keys {
			_, ok, err := metaValue(ctx, c, k)
			if err != nil || ok {
				available = err == nil
				return err
			}
		}
		if available, err = hasFTSTrash(ctx, c); err != nil || available {
			return err
		}
		available, err = ftsExternalIndexEmptyWithMessages(ctx, c)
		return err
	})
	return available, err
}

func (s *Store) demoteLegacyFTSToTrash(ctx context.Context) (int64, error) {
	var hw int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		if err := dropFTSTriggers(ctx, c); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "DROP VIEW IF EXISTS messages_fts_trigram_src"); err != nil {
			return err
		}
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name IN ('messages_fts', 'messages_fts_trigram') "+
			"AND sql LIKE 'CREATE VIRTUAL TABLE%' LIMIT 1").Scan(&one)
		switch {
		case err == nil:
			for _, q := range []string{
				"PRAGMA writable_schema=ON",
				"DELETE FROM sqlite_master WHERE type = 'table' " +
					"AND name IN ('messages_fts', 'messages_fts_trigram') AND sql LIKE 'CREATE VIRTUAL TABLE%'",
				"PRAGMA writable_schema=RESET",
			} {
				if _, err := c.ExecContext(ctx, q); err != nil {
					return err
				}
			}
			names, err := queryStrings(ctx, c, "SELECT name FROM sqlite_master WHERE type = 'table' "+
				`AND (name LIKE 'messages_fts_%' ESCAPE '\' `+
				`OR name LIKE 'messages_fts_trigram_%' ESCAPE '\') `+
				`AND name NOT LIKE 'messages\_fts\_cjk%' ESCAPE '\'`)
			if err != nil {
				return err
			}
			for _, name := range names {
				if _, err := c.ExecContext(ctx, "ALTER TABLE "+name+" RENAME TO "+ftsTrashPrefix+name); err != nil {
					return err
				}
			}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if hw, err = seedFTSRebuildMarkers(ctx, c, true); err != nil {
			return err
		}
		return deleteMeta(ctx, c, "fts_optimize_available")
	})
	if err != nil {
		return 0, err
	}
	return hw, s.ensureV23FTSTables(ctx, "failed to create v23 messages_fts during optimize-storage demote")
}

func (s *Store) ensureV23FTSTables(ctx context.Context, failure string) error {
	return s.withWriter(ctx, "write", func(c Conn) error {
		base, err := s.ensureFTSSchema(ctx, c, "messages_fts", ftsSQL)
		if err != nil {
			return err
		}
		trigram, err := s.ensureFTSSchema(ctx, c, "messages_fts_trigram", ftsTrigramSQL)
		if err != nil {
			return err
		}
		s.fts().set(func(f *ftsFlags) { f.trigram = trigram })
		if !base {
			return errors.New(failure)
		}
		return nil
	})
}

func (s *Store) backfillProgressOf(ctx context.Context, phase, prefix string) (*backfillProgress, error) {
	st, pending, err := s.rebuildStatus(ctx, prefix)
	if err != nil || !pending {
		return nil, err
	}
	var total, done int64
	err = s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(id <= ?), 0) FROM messages WHERE +id <= ?",
			st.Indexed, st.Total).Scan(&total, &done)
	})
	if err != nil {
		return nil, err
	}
	return &backfillProgress{phase: phase, total: total, indexed: done}, nil
}

func (o *ftsOptimizer) drive(ctx context.Context, step func(ctx context.Context) (bool, error), info func() OptimizeProgress,
	emit func(OptimizeProgress), watch *holderWatch) error {
	o.chunk = ftsRebuildChunkRows
	failures := 0
	for {
		t0 := time.Now()
		more, err := step(ctx)
		if err != nil {
			if familyOf(err) != callFamily || ctx.Err() != nil {
				return err
			}
			failures++
			if failures >= optimizeChunkAttempts {
				return err
			}
			slog.Warn(fmt.Sprintf("FTS optimize chunk failed (attempt %d, retrying): %s", failures, errText(err)))
			if err := sleepCtx(ctx, time.Duration(failures)*250*time.Millisecond); err != nil {
				return err
			}
			continue
		}
		failures = 0
		if !more {
			return nil
		}
		cost, paced := time.Since(t0), watch.held.Load()
		if info != nil {
			p := info()
			p.Paced = paced
			emit(p)
		}
		target := ftsRebuildChunkTarget
		if paced {
			target = ftsRebuildPacedTarget
		}
		o.chunk = nextChunkRows(o.chunk, cost, target)
		if paced {
			if err := sleepCtx(ctx, max(ftsRebuildMinPause, time.Duration(float64(cost)*ftsRebuildDutyFactor))); err != nil {
				return err
			}
		}
	}
}

func (s *Store) optimizeVacuum(ctx context.Context, tick func(OptimizeProgress)) (bool, error) {
	stop := make(chan struct{})
	var ticker sync.WaitGroup
	if tick != nil {
		var pageSize, pages, free int64
		err := s.Read(ctx, func(c Conn) error {
			for _, p := range []struct {
				name string
				dst  *int64
			}{{"page_size", &pageSize}, {"page_count", &pages}, {"freelist_count", &free}} {
				if err := c.QueryRowContext(ctx, "PRAGMA "+p.name).Scan(p.dst); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return false, err
		}
		started := time.Now()
		info := func() OptimizeProgress {
			return OptimizeProgress{Phase: "vacuum", Elapsed: time.Since(started), SizeBytes: pages * pageSize,
				EstimatedBytes: (pages - free) * pageSize}
		}
		ticker.Add(1)
		go func() {
			defer ticker.Done()
			tick(info())
			t := time.NewTicker(optimizeVacuumTick)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					tick(info())
				}
			}
		}()
	}
	err := s.withWriter(ctx, "write", func(c Conn) error {
		_, err := c.ExecContext(ctx, "VACUUM")
		return err
	})
	close(stop)
	ticker.Wait()
	vacuumed := err == nil
	if err != nil {
		if familyOf(err) != callFamily || ctx.Err() != nil {
			return false, presentable(err)
		}
		slog.Warn(fmt.Sprintf("VACUUM after FTS optimize failed: %s", errText(err)))
	}
	if err := s.withWriter(ctx, "write", func(c Conn) error {
		_, err := c.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
		return err
	}); err != nil {
		slog.Debug(fmt.Sprintf("WAL checkpoint (PASSIVE) after optimize VACUUM failed: %s", errText(err)))
	}
	return vacuumed, nil
}

func optimizeSettle(ctx context.Context, c Conn) (string, error) {
	if _, ok, err := metaValue(ctx, c, "fts_rebuild_high_water"); err != nil || ok {
		return "backfill_incomplete", err
	}
	if trash, err := hasFTSTrash(ctx, c); err != nil || trash {
		return "teardown_incomplete", err
	}
	if empty, err := ftsExternalIndexEmptyWithMessages(ctx, c); err != nil || empty {
		return "backfill_incomplete", err
	}
	current, err := ftsLayoutCurrent(ctx, c)
	if err != nil {
		return "", err
	}
	if !current {
		drift, err := ftsLayoutDrift(ctx, c)
		if err != nil {
			return "", err
		}
		if len(drift) > 0 {
			return "layout_drift", nil
		}
	}
	if current {
		if err := SetMetaIn(ctx, c, "fts_storage_version", strconv.Itoa(FTSStorageVersion)); err != nil {
			return "", err
		}
	}
	if err := deleteMeta(ctx, c, "fts_optimize_available"); err != nil {
		return "", err
	}
	_, err = c.ExecContext(ctx, "UPDATE schema_version SET version = ? WHERE version < ?", SchemaVersion, SchemaVersion)
	return "", err
}

type OptimizeStorageOptions struct {
	Progress func(OptimizeProgress)
	Vacuum   bool
}

type OptimizeStorageResult struct {
	OK                 bool
	Reason             string
	Vacuumed           sql.Null[bool]
	VacuumSkippedInUse bool
}

func (s *Store) OptimizeFTSStorage(ctx context.Context, o OptimizeStorageOptions) (OptimizeStorageResult, error) {
	if !ftsGet(s.fts(), func(f *ftsFlags) bool { return f.enabled }) {
		return OptimizeStorageResult{Reason: "fts5_unavailable"}, nil
	}
	if s.readOnly {
		return OptimizeStorageResult{Reason: "read_only"}, nil
	}
	if err := s.repairOptimizeBookkeeping(ctx); err != nil {
		return OptimizeStorageResult{}, err
	}
	var needsUpgrade bool
	if err := s.withWriter(ctx, "write", func(c Conn) error {
		drift, err := ftsLayoutDrift(ctx, c)
		needsUpgrade = len(drift) > 0
		return err
	}); err != nil {
		return OptimizeStorageResult{}, err
	}
	_, pending, err := s.GetMeta(ctx, "fts_rebuild_high_water")
	if err != nil {
		return OptimizeStorageResult{}, err
	}
	switch {
	case needsUpgrade && !pending:
		if _, err := s.demoteLegacyFTSToTrash(ctx); err != nil {
			return OptimizeStorageResult{}, err
		}
	case pending && !needsUpgrade:
		if err := s.ensureV23FTSTables(ctx, "failed to re-create v23 messages_fts on optimize-storage resume"); err != nil {
			return OptimizeStorageResult{}, err
		}
	}
	if err := s.ftsCJKResetIfStale(ctx); err != nil {
		return OptimizeStorageResult{}, err
	}
	if ftsGet(s.fts(), func(f *ftsFlags) bool { return f.cjkLoaded }) {
		if err := s.ensureCJKSchemaCommitted(ctx); err != nil {
			return OptimizeStorageResult{}, err
		}
	}
	emit := o.Progress
	if emit == nil {
		emit = func(OptimizeProgress) {}
	}
	if err := s.optimizePasses(ctx, o.Progress != nil, emit); err != nil {
		return OptimizeStorageResult{}, err
	}
	var stillPending, stillTrash, emptyIndex bool
	err = s.Read(ctx, func(c Conn) error {
		var err error
		if _, stillPending, err = metaValue(ctx, c, "fts_rebuild_high_water"); err != nil {
			return err
		}
		if stillTrash, err = hasFTSTrash(ctx, c); err != nil {
			return err
		}
		emptyIndex, err = ftsExternalIndexEmptyWithMessages(ctx, c)
		return err
	})
	if err != nil {
		return OptimizeStorageResult{}, err
	}
	if stillPending || stillTrash || emptyIndex {
		reason := "teardown_incomplete"
		if stillPending || emptyIndex {
			reason = "backfill_incomplete"
		}
		slog.Warn(fmt.Sprintf("FTS storage optimization did not settle (%s): pending=%t trash=%t empty_index=%t",
			reason, stillPending, stillTrash, emptyIndex))
		return OptimizeStorageResult{Reason: reason}, nil
	}
	var result OptimizeStorageResult
	switch {
	case o.Vacuum && len(ForeignHolders(s.path)) > 0:
		slog.Info(fmt.Sprintf("Skipping VACUUM after FTS optimize: another process is using %s", s.path))
		result.Vacuumed, result.VacuumSkippedInUse = sql.Null[bool]{Valid: true}, true
	case o.Vacuum:
		ok, err := s.optimizeVacuum(ctx, o.Progress)
		if err != nil {
			return OptimizeStorageResult{}, err
		}
		result.Vacuumed = sql.Null[bool]{V: ok, Valid: true}
	}
	var refusal string
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		var err error
		refusal, err = optimizeSettle(ctx, c)
		return err
	})
	if err != nil {
		return OptimizeStorageResult{}, err
	}
	if refusal != "" {
		slog.Warn(fmt.Sprintf("FTS storage optimization settle refused (%s)", refusal))
		return OptimizeStorageResult{Reason: refusal, Vacuumed: result.Vacuumed}, nil
	}
	emit(OptimizeProgress{Phase: "done", Percent: 100})
	slog.Info(fmt.Sprintf("FTS storage optimization complete (layout v%d).", FTSStorageVersion))
	result.OK = true
	return result, nil
}

func (s *Store) optimizePasses(ctx context.Context, reporting bool, emit func(OptimizeProgress)) error {
	o := &ftsOptimizer{s: s, chunk: ftsRebuildChunkRows}
	watch := s.watchHolders()
	defer watch.close()
	passes := []struct {
		phase, prefix string
		step          func(ctx context.Context, p *backfillProgress) (bool, error)
	}{{"backfill", "fts_rebuild", o.baseRebuildStep}, {"backfill_cjk", "fts_cjk_rebuild", o.cjkRebuildStep}}
	for _, pass := range passes {
		var bf *backfillProgress
		if reporting {
			var err error
			if bf, err = s.backfillProgressOf(ctx, pass.phase, pass.prefix); err != nil {
				return err
			}
		}
		var info func() OptimizeProgress
		if bf != nil {
			p := bf.info()
			p.Paced = watch.held.Load()
			emit(p)
			info = bf.info
		}
		step := pass.step
		if err := o.drive(ctx, func(ctx context.Context) (bool, error) { return step(ctx, bf) }, info, emit, watch); err != nil {
			return err
		}
	}
	teardown := &teardownProgress{counted: reporting}
	return o.drive(ctx, func(ctx context.Context) (bool, error) { return o.teardownStep(ctx, teardown) }, teardown.info, emit, watch)
}
