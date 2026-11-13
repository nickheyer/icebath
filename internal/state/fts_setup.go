package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Result of probing one full-text table
type ftsProbe int

// Queryable, absent, or present but not servable because a module or tokenizer is missing
const (
	probeQueryable ftsProbe = iota
	probeAbsent
	probeUnavailable
)

// Reports a missing FTS5 module or a missing optional tokenizer
func isFTS5UnavailableError(err error) bool {
	text := lowerText(err)
	return (strings.Contains(text, "no such module") && strings.Contains(text, "fts5")) || isTrigramUnavailableError(err)
}

// Reports that only an optional tokenizer is missing, one index unservable rather than FTS disabled
func isTrigramUnavailableError(err error) bool {
	text := lowerText(err)
	return strings.Contains(text, "no such tokenizer: trigram") || strings.Contains(text, "no such tokenizer: cjk_unicode61")
}

// Logs once that the trigram tokenizer is missing, base FTS5 staying enabled
func (s *Store) warnTrigramUnavailable(err error) {
	f := s.fts()
	f.mu.Lock()
	warned := f.trigramWarned
	f.trigramWarned = true
	f.mu.Unlock()
	if !warned {
		slog.Info(fmt.Sprintf("SQLite trigram tokenizer unavailable for %s (requires SQLite >= 3.34, this build is %s); "+
			"CJK/substring search will fall back to LIKE: %s", s.path, SQLiteVersion(), errText(err)))
	}
}

// Disables FTS on the store and logs once that FTS5 is missing
func (s *Store) warnFTS5Unavailable(err error) {
	f := s.fts()
	f.mu.Lock()
	f.enabled = false
	warned := f.fts5Warned
	f.fts5Warned = true
	f.mu.Unlock()
	if !warned {
		slog.Warn(fmt.Sprintf("SQLite FTS5 unavailable for %s; full-text session search disabled. (underlying error: %s)",
			s.path, errText(err)))
	}
}

// Whether table is queryable, absent or unservable, other errors such as a corrupt table rising
func ftsTableProbe(ctx context.Context, c Conn, s *Store, table string) (ftsProbe, error) {
	_, err := c.ExecContext(ctx, "SELECT * FROM "+table+" LIMIT 0")
	if err == nil {
		return probeQueryable, nil
	}
	if familyOf(err) != callFamily {
		return 0, err
	}
	if isFTS5UnavailableError(err) {
		if isTrigramUnavailableError(err) {
			s.warnTrigramUnavailable(err)
		} else {
			s.warnFTS5Unavailable(err)
		}
		return probeUnavailable, nil
	}
	if strings.Contains(lowerText(err), "no such table") {
		return probeAbsent, nil
	}
	return 0, err
}

// Reports whether the runtime can create an FTS5 table, logging once when it cannot
func (s *Store) sqliteSupportsFTS5(ctx context.Context, c Conn) (bool, error) {
	if _, err := c.ExecContext(ctx, "CREATE VIRTUAL TABLE temp._icebath_fts5_probe USING fts5(x)"); err != nil {
		if familyOf(err) != callFamily || !isFTS5UnavailableError(err) {
			return false, err
		}
		s.warnFTS5Unavailable(err)
		return false, nil
	}
	if _, err := c.ExecContext(ctx, "DROP TABLE temp._icebath_fts5_probe"); err != nil {
		return false, err
	}
	return true, nil
}

// Probes the trigram tokenizer with a temporary table that never reaches the file
func (s *Store) trigramTokenizerAvailable(ctx context.Context, c Conn) (bool, error) {
	const probe = "temp.icebath_fts5_trigram_probe"
	if _, err := c.ExecContext(ctx, "DROP TABLE IF EXISTS "+probe); err != nil {
		return false, err
	}
	_, err := c.ExecContext(ctx, "CREATE VIRTUAL TABLE "+probe+" USING fts5(content, tokenize='trigram')")
	_, derr := c.ExecContext(ctx, "DROP TABLE IF EXISTS "+probe)
	if err != nil {
		if familyOf(err) != callFamily || !isTrigramUnavailableError(err) {
			return false, err
		}
		s.warnTrigramUnavailable(err)
		return false, derr
	}
	return true, derr
}

// Drops the named triggers, a refused drop skipped and damage beyond a failed call rising
func dropTriggers(ctx context.Context, c Conn, names []string) error {
	for _, t := range names {
		if _, err := c.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+t); err != nil && familyOf(err) != callFamily {
			return err
		}
	}
	return nil
}

// Drops the base and trigram sync triggers
func dropFTSTriggers(ctx context.Context, c Conn) error { return dropTriggers(ctx, c, ftsTriggers) }

// Drops every full-text sync trigger, the cjk ones included
func dropAllFTSTriggers(ctx context.Context, c Conn) error {
	if err := dropFTSTriggers(ctx, c); err != nil {
		return err
	}
	return dropTriggers(ctx, c, ftsCJKTriggers)
}

// Stored SQL of one sqlite_master object of kind, false when absent
func masterSQL(ctx context.Context, c Conn, kind, name string) (string, bool, error) {
	var text sql.NullString
	err := c.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = ? AND name = ?", kind, name).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return text.String, true, nil
}

// Reports a base index in any pre-v23 shape, every one of which lacks the tool_name column
func dbHasLegacyInlineFTS(ctx context.Context, c Conn) (bool, error) {
	text, ok, err := masterSQL(ctx, c, "table", "messages_fts")
	return ok && !strings.Contains(text, "tool_name"), err
}

// Reports a trigram table still indexing the tool_calls payload
func dbHasTrigramToolCallsProjection(ctx context.Context, c Conn) (bool, error) {
	text, ok, err := masterSQL(ctx, c, "table", "messages_fts_trigram")
	return ok && strings.Contains(strings.ToLower(text), "tool_calls"), err
}

// Patterns of the stored form, separator padding and a quoted object name
var (
	storedFormPadding = regexp.MustCompile(`\s*([(),])\s*`)
	storedFormQuoted  = regexp.MustCompile(`^CREATE (VIRTUAL TABLE|VIEW) "([^"]+)"`)
)

// Stored SQL with whitespace collapsed, none around parentheses and commas, and the object name unquoted
func storedForm(text string) string {
	collapsed := storedFormPadding.ReplaceAllString(strings.Join(strings.Fields(text), " "), "$1")
	return storedFormQuoted.ReplaceAllString(collapsed, "CREATE $1 $2")
}

// Stored form of the CREATE kind IF NOT EXISTS name statement of ddl once it has run
func expectedStoredSQL(ddl, kind, name string) string {
	start := strings.Index(ddl, "CREATE "+kind+" IF NOT EXISTS "+name+" ")
	end := start + strings.Index(ddl[start:], ";")
	return storedForm(strings.Replace(ddl[start:end], " IF NOT EXISTS", "", 1))
}

// Stored forms of the base and trigram tables that exist
func storedVTables(ctx context.Context, c Conn) (map[string]string, error) {
	rows, err := c.QueryContext(ctx, "SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name IN (?, ?)", ftsDDLTables[0], ftsDDLTables[1])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, text sql.NullString
		if err := rows.Scan(&name, &text); err != nil {
			return nil, err
		}
		out[name.String] = storedForm(text.String)
	}
	return out, rows.Err()
}

// Reports a definition an earlier release created, the shape checks keeping every pre-registry old store opt-in
func isOlderLayout(table, stored string) bool {
	layouts := ftsVTableLayouts[table]
	if containsString(layouts[:len(layouts)-1], stored) {
		return true
	}
	if table == "messages_fts" {
		return !strings.Contains(stored, "tool_name")
	}
	return strings.Contains(strings.ToLower(stored), "tool_calls")
}

// Reports a base index this code or an earlier release created
func baseIndexRecognised(ctx context.Context, c Conn) (bool, error) {
	stored, err := storedVTables(ctx, c)
	if err != nil {
		return false, err
	}
	v, ok := stored["messages_fts"]
	return ok && containsString(ftsVTableLayouts["messages_fts"], v), nil
}

// Reports an external content base index not reading the projection view, which the next writable open rebuilds
func baseIndexRealignedOnOpen(stored string) bool {
	return strings.Contains(stored, "tool_name") && !strings.Contains(stored, "messages_fts_src")
}

// Tables on an older release's definition that only optimize-storage replaces
func ftsLayoutDrift(ctx context.Context, c Conn) ([]string, error) {
	stored, err := storedVTables(ctx, c)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, table := range ftsDDLTables {
		v, ok := stored[table]
		if ok && isOlderLayout(table, v) && !(table == "messages_fts" && baseIndexRealignedOnOpen(v)) {
			out = append(out, table)
		}
	}
	return out, nil
}

// Reports every present table and the base source view exactly as this code creates them
func ftsLayoutCurrent(ctx context.Context, c Conn) (bool, error) {
	stored, err := storedVTables(ctx, c)
	if err != nil {
		return false, err
	}
	for table, v := range stored {
		layouts := ftsVTableLayouts[table]
		if v != layouts[len(layouts)-1] {
			return false, nil
		}
	}
	if _, ok := stored["messages_fts"]; !ok {
		return true, nil
	}
	view, ok, err := masterSQL(ctx, c, "view", "messages_fts_src")
	return ok && storedForm(view) == expectedStoredSQL(ftsSQL, "VIEW", "messages_fts_src"), err
}

// Reports a projection view that differs from what this code creates
func baseSourceViewIsStale(ctx context.Context, c Conn) (bool, error) {
	view, ok, err := masterSQL(ctx, c, "view", "messages_fts_src")
	return ok && storedForm(view) != expectedStoredSQL(ftsSQL, "VIEW", "messages_fts_src"), err
}

// Shadow table suffixes FTS5 owns per index
var fts5ShadowSuffixes = []string{"content", "data", "docsize", "idx", "config"}

// Drops shadow tables whose virtual table row is gone, returning the families repaired
func dropOrphanFTSShadowTables(ctx context.Context, c Conn, families []string) ([]string, error) {
	var repaired []string
	for _, family := range families {
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ? AND sql LIKE 'CREATE VIRTUAL TABLE%'", family).Scan(&one)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		shadows := make([]string, len(fts5ShadowSuffixes))
		for i, suffix := range fts5ShadowSuffixes {
			shadows[i] = family + "_" + suffix
		}
		orphans, err := queryStrings(ctx, c, "SELECT name FROM sqlite_master WHERE type = 'table' AND name IN ("+bindMarks(len(shadows))+")", stringArgs(shadows)...)
		if err != nil {
			return nil, err
		}
		if len(orphans) == 0 {
			continue
		}
		for _, name := range orphans {
			if _, err := c.ExecContext(ctx, "DROP TABLE "+quoteIdent(name)); err != nil {
				return nil, err
			}
		}
		slog.Warn(fmt.Sprintf("Dropped orphan FTS5 shadow tables of %s (%s); the index is recreated from messages", family, strings.Join(orphans, ", ")))
		repaired = append(repaired, family)
	}
	return repaired, nil
}

// First column of every row of a query as text
func queryStrings(ctx context.Context, c Conn, query string, args ...any) ([]string, error) {
	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v.String)
	}
	return out, rows.Err()
}

// Runs the table's DDL even when it exists so dropped triggers return, false when the module or tokenizer is missing
func (s *Store) ensureFTSSchema(ctx context.Context, c Conn, table, ddl string) (bool, error) {
	status, err := ftsTableProbe(ctx, c, s, table)
	if err != nil || status == probeUnavailable {
		return false, err
	}
	if _, err := c.ExecContext(ctx, ddl); err != nil {
		if familyOf(err) != callFamily || !isFTS5UnavailableError(err) {
			return false, err
		}
		if isTrigramUnavailableError(err) {
			s.warnTrigramUnavailable(err)
		} else {
			s.warnFTS5Unavailable(err)
		}
		return false, nil
	}
	return true, nil
}

// Creates, repairs or detaches the CJK bigram index, a failed call leaving CJK search on trigram and LIKE
func (s *Store) ensureFTSCJKSchema(ctx context.Context, c Conn) error {
	available, err := s.ensureFTSCJK(ctx, c)
	if err != nil {
		available = false
		if familyOf(err) != callFamily {
			s.fts().set(func(f *ftsFlags) { f.cjkAvailable = false })
			return err
		}
		slog.Warn(fmt.Sprintf("messages_fts_cjk ensure failed; CJK search stays on trigram/LIKE: %s", errText(err)))
	}
	s.fts().set(func(f *ftsFlags) { f.cjkAvailable = available })
	return nil
}

// Body of the CJK ensure, true when the index is servable now
func (s *Store) ensureFTSCJK(ctx context.Context, c Conn) (bool, error) {
	_, present, err := masterSQL(ctx, c, "table", "messages_fts_cjk")
	if err != nil {
		return false, err
	}
	if !ftsGet(s.fts(), func(f *ftsFlags) bool { return f.cjkLoaded }) {
		if !present {
			return false, nil
		}
		live, err := queryStrings(ctx, c, "SELECT name FROM sqlite_master WHERE type = 'trigger' AND name IN ("+bindMarks(len(ftsCJKTriggers))+")", stringArgs(ftsCJKTriggers)...)
		if err != nil || len(live) == 0 {
			return false, err
		}
		slog.Warn("messages_fts_cjk triggers present but the cjk_unicode61 tokenizer is not registered on this connection — dropping " +
			"the cjk triggers so message writes keep working. CJK search falls back to trigram/LIKE; run `icebath sessions " +
			"optimize-storage` with sessions.cjk_fts enabled to rebuild.")
		if _, err := c.ExecContext(ctx, "INSERT INTO state_meta (key, value) VALUES (?, '1') ON CONFLICT(key) DO UPDATE SET value = '1'", ftsCJKStaleKey); err != nil {
			return false, err
		}
		for _, t := range live {
			if _, err := c.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+t); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if _, err := c.ExecContext(ctx, ftsCJKTableSQL); err != nil {
		return false, err
	}
	if !present {
		if err := deleteMeta(ctx, c, ftsCJKStaleKey); err != nil {
			return false, err
		}
		var rows int64
		if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE role <> 'tool'").Scan(&rows); err != nil {
			return false, err
		}
		if rows > 0 {
			var hw int64
			if err := c.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM messages").Scan(&hw); err != nil {
				return false, err
			}
			for _, kv := range [][2]string{{"fts_cjk_rebuild_high_water", fmt.Sprint(hw)}, {"fts_cjk_rebuild_progress", "0"}} {
				if _, err := c.ExecContext(ctx, stateMetaUpsertSQL, kv[0], kv[1]); err != nil {
					return false, err
				}
			}
		}
	}
	if _, stale, err := metaValue(ctx, c, ftsCJKStaleKey); err != nil || stale {
		return false, err
	}
	if _, err := c.ExecContext(ctx, ftsCJKTriggerSQL); err != nil {
		return false, err
	}
	_, pending, err := metaValue(ctx, c, "fts_cjk_rebuild_high_water")
	return !pending, err
}

// Reports whether any trigger of names is missing, an empty list never missing
func ftsTriggersMissing(ctx context.Context, c Conn, names []string) (bool, error) {
	if len(names) == 0 {
		return false, nil
	}
	var n int
	err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name IN ("+bindMarks(len(names))+")", stringArgs(names)...).Scan(&n)
	return n < len(names), err
}

// Reports a broad AFTER UPDATE trigger that lacks an OF column list
func ftsUpdateTriggerNeedsNarrowing(text string) bool {
	if text == "" {
		return false
	}
	compact := strings.ToUpper(strings.Join(strings.Fields(text), " "))
	return !strings.Contains(compact, "AFTER UPDATE OF ") && strings.Contains(compact, "AFTER UPDATE ON ")
}

// Replaces broad AFTER UPDATE full-text triggers with column scoped ones without a rebuild, returning how many it dropped
func (s *Store) migrateBroadFTSUpdateTriggers(ctx context.Context, c Conn) (int, error) {
	legacy, err := dbHasLegacyInlineFTS(ctx, c)
	if err != nil {
		return 0, err
	}
	names := []string{"messages_fts_update", "messages_fts_trigram_update"}
	if !legacy {
		names = append(names, "messages_fts_cjk_update")
	}
	rows, err := c.QueryContext(ctx, "SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND name IN ("+bindMarks(len(names))+")", stringArgs(names)...)
	if err != nil {
		return 0, err
	}
	var toDrop []string
	for rows.Next() {
		var name, text sql.NullString
		if err := rows.Scan(&name, &text); err != nil {
			rows.Close()
			return 0, err
		}
		if ftsUpdateTriggerNeedsNarrowing(text.String) {
			toDrop = append(toDrop, name.String)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	if len(toDrop) == 0 {
		return 0, nil
	}
	for _, name := range toDrop {
		if _, err := c.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+name); err != nil {
			return 0, err
		}
	}
	base, trigram := ftsSQL, ftsTrigramSQL
	if legacy {
		base, trigram = legacyFTSSQL, legacyFTSTrigramSQL
	}
	if _, err := s.ensureFTSSchema(ctx, c, "messages_fts", base); err != nil {
		return 0, err
	}
	if _, err := s.ensureFTSSchema(ctx, c, "messages_fts_trigram", trigram); err != nil {
		return 0, err
	}
	if containsString(toDrop, "messages_fts_cjk_update") {
		if err := s.ensureFTSCJKSchema(ctx, c); err != nil {
			s.quarantineCJKAfterUpdateOfMigration(ctx, c)
			slog.Error(fmt.Sprintf("CJK FTS re-ensure after UPDATE OF migration failed: %s", errText(err)))
			return 0, err
		}
		text, ok, err := masterSQL(ctx, c, "trigger", "messages_fts_cjk_update")
		if err != nil {
			return 0, err
		}
		if !ok || ftsUpdateTriggerNeedsNarrowing(text) {
			s.quarantineCJKAfterUpdateOfMigration(ctx, c)
			slog.Warn("CJK FTS UPDATE trigger missing or still broad after UPDATE OF migration; marked stale and unavailable")
		}
	}
	slog.Info(fmt.Sprintf("Migrated %d broad FTS UPDATE trigger(s) to AFTER UPDATE OF (no rebuild required)", len(toDrop)))
	return len(toDrop), nil
}

// Fails closed after the CJK update trigger was dropped mid migration, the stale breadcrumb persisted and the residue dropped
func (s *Store) quarantineCJKAfterUpdateOfMigration(ctx context.Context, c Conn) {
	s.fts().set(func(f *ftsFlags) { f.cjkAvailable = false })
	if err := SetMetaIn(ctx, c, ftsCJKStaleKey, "1"); err != nil {
		slog.Debug(fmt.Sprintf("Could not persist CJK FTS stale breadcrumb: %s", errText(err)))
	}
	if _, err := c.ExecContext(ctx, "DROP TRIGGER IF EXISTS messages_fts_cjk_update"); err != nil {
		slog.Debug(fmt.Sprintf("Could not drop residual CJK UPDATE trigger after quarantine: %s", errText(err)))
	}
}

// Reports a base index still external content over the raw messages table
func ftsIndexIsMisalignedSource(ctx context.Context, c Conn) (bool, error) {
	text, ok, err := masterSQL(ctx, c, "table", "messages_fts")
	return ok && !strings.Contains(text, "messages_fts_src"), err
}

// Reports whether messages holds any row
func hasMessages(ctx context.Context, c Conn) (bool, error) {
	var one int
	err := c.QueryRowContext(ctx, "SELECT 1 FROM messages LIMIT 1").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Re-points the base index at the projection view and rebuilds it once, storage layout 2 to 3
func (s *Store) migrateMisalignedFTSSource(ctx context.Context, c Conn, legacy bool) error {
	if legacy {
		return nil
	}
	recognised, err := baseIndexRecognised(ctx, c)
	if err != nil || !recognised {
		return err
	}
	staleView, err := baseSourceViewIsStale(ctx, c)
	if err != nil {
		return err
	}
	if !staleView {
		misaligned, err := ftsIndexIsMisalignedSource(ctx, c)
		if err != nil || !misaligned {
			return err
		}
	}
	populated, err := hasMessages(ctx, c)
	if err != nil {
		return err
	}
	align := func() error {
		for _, name := range ftsBaseTriggers {
			if _, err := c.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+name); err != nil {
				return err
			}
		}
		if _, err := c.ExecContext(ctx, "DROP TABLE IF EXISTS messages_fts"); err != nil {
			return err
		}
		if staleView {
			if _, err := c.ExecContext(ctx, "DROP VIEW messages_fts_src"); err != nil {
				return err
			}
		}
		if _, err := s.ensureFTSSchema(ctx, c, "messages_fts", ftsSQL); err != nil {
			return err
		}
		if populated {
			if _, err := c.ExecContext(ctx, "INSERT INTO messages_fts(messages_fts) VALUES('rebuild')"); err != nil {
				return err
			}
			if _, pending, err := metaValue(ctx, c, "fts_rebuild_high_water"); err != nil {
				return err
			} else if pending {
				trigram, err := ftsTableProbe(ctx, c, s, "messages_fts_trigram")
				if err != nil {
					return err
				}
				switch trigram {
				case probeQueryable:
					if _, err := c.ExecContext(ctx, "INSERT INTO messages_fts_trigram(messages_fts_trigram) VALUES('rebuild')"); err != nil {
						return err
					}
				case probeUnavailable:
					for _, name := range ftsTrigramTriggers {
						if _, err := c.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+name); err != nil {
							return err
						}
					}
				}
			}
		}
		if _, err := c.ExecContext(ctx, clearRebuildMarkersSQL); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx, dropRetiredToolHighWaterSQL)
		return err
	}
	if !populated {
		if _, err := c.ExecContext(ctx, "SAVEPOINT fts_align_empty"); err != nil {
			return err
		}
		if err := align(); err != nil {
			_, _ = c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK TO SAVEPOINT fts_align_empty")
			_, _ = c.ExecContext(context.WithoutCancel(ctx), "RELEASE SAVEPOINT fts_align_empty")
			return err
		}
		_, err := c.ExecContext(ctx, "RELEASE SAVEPOINT fts_align_empty")
		return err
	}
	return s.runAdmittedStartupRebuild(ctx, c, align)
}

// Installs the source filtered trigram view and rebuilds once, false holding schema_version back for a retry
func (s *Store) migrateTrigramCronExclusion(ctx context.Context, c Conn) (bool, error) {
	legacy, err := dbHasLegacyInlineFTS(ctx, c)
	if err != nil {
		return false, err
	}
	toolCalls, err := dbHasTrigramToolCallsProjection(ctx, c)
	if err != nil {
		return false, err
	}
	if legacy || toolCalls {
		return true, nil
	}
	trigram, err := ftsTableProbe(ctx, c, s, "messages_fts_trigram")
	if err != nil {
		return false, err
	}
	if trigram != probeQueryable {
		return trigram == probeAbsent, nil
	}
	for _, name := range ftsTrigramTriggers {
		if _, err := c.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+name); err != nil {
			return false, err
		}
	}
	if _, err := c.ExecContext(ctx, "DROP VIEW IF EXISTS messages_fts_trigram_src"); err != nil {
		return false, err
	}
	ok, err := s.ensureFTSSchema(ctx, c, "messages_fts_trigram", ftsTrigramSQL)
	if err != nil || !ok {
		return false, err
	}
	err = s.runAdmittedStartupRebuild(ctx, c, func() error {
		_, err := c.ExecContext(ctx, "INSERT INTO messages_fts_trigram(messages_fts_trigram) VALUES('rebuild')")
		return err
	})
	return err == nil, err
}

// Concatenated content the legacy inline triggers index
const legacyInlineConcatSQL = "COALESCE(content, '') || ' ' || COALESCE(tool_name, '') || ' ' || COALESCE(tool_calls, '') "

// Rebuilds the external content indexes from their sources, or refills the legacy inline ones, clearing the backfill markers
func rebuildFTSIndexes(ctx context.Context, c Conn, legacy, includeTrigram bool) error {
	tables := []string{"messages_fts"}
	if includeTrigram {
		tables = append(tables, "messages_fts_trigram")
	}
	for _, t := range tables {
		var stmts []string
		if legacy {
			stmts = []string{"DELETE FROM " + t, "INSERT INTO " + t + "(rowid, content) SELECT id, " + legacyInlineConcatSQL + "FROM messages"}
		} else {
			stmts = []string{"INSERT INTO " + t + "(" + t + ") VALUES('rebuild')"}
		}
		for _, q := range stmts {
			if _, err := c.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if legacy {
		return nil
	}
	_, err := c.ExecContext(ctx, clearRebuildMarkersSQL)
	return err
}

// Runs a startup rebuild under the cross-process admission, detaching every sync trigger behind a stale breadcrumb when deferred
func (s *Store) runAdmittedStartupRebuild(ctx context.Context, c Conn, rebuild func() error) error {
	release, admitted := s.eng.ftsRebuildAdmission(ctx, s.path, ftsRebuildLockTimeout)
	if admitted {
		defer release()
		return rebuild()
	}
	slog.Warn("Deferred startup FTS rebuild: another process holds the rebuild authority for this state.db; detaching FTS " +
		"sync until the stale-index recovery path rebuilds it.")
	if _, err := c.ExecContext(ctx, staleKeyUpsertSQL, ftsStaleKey); err != nil {
		return err
	}
	if err := dropAllFTSTriggers(ctx, c); err != nil {
		return err
	}
	s.fts().set(func(f *ftsFlags) { f.stale, f.enabled, f.trigram, f.cjkAvailable = true, false, false, false })
	return nil
}

// Creates or repairs the full-text objects, realigning, recovering a stale index or rebuilding a missing trigger family
func (s *Store) initFTS(ctx context.Context, c Conn) error {
	legacy, err := dbHasLegacyInlineFTS(ctx, c)
	if err != nil {
		return err
	}
	orphans, err := dropOrphanFTSShadowTables(ctx, c, ftsTables)
	if err != nil {
		return err
	}
	if !ftsGet(s.fts(), func(f *ftsFlags) bool { return f.stale }) {
		if err := s.migrateMisalignedFTSSource(ctx, c, legacy); err != nil {
			return err
		}
	}
	if ftsGet(s.fts(), func(f *ftsFlags) bool { return f.stale }) {
		recovered, err := s.recoverStaleFTS(ctx, c, legacy, ftsRebuildLockTimeout)
		if err != nil {
			return err
		}
		if recovered {
			if err := s.ensureFTSCJKSchema(ctx, c); err != nil {
				return err
			}
		} else {
			s.fts().set(func(f *ftsFlags) { f.enabled, f.trigram, f.cjkAvailable = false, false, false })
		}
	} else if err := s.initFTSLive(ctx, c, legacy, orphans); err != nil {
		return err
	}
	if ftsGet(s.fts(), func(f *ftsFlags) bool { return f.enabled }) {
		_, err := s.migrateBroadFTSUpdateTriggers(ctx, c)
		return err
	}
	return nil
}

// Ensure path of a store not marked stale, a missing trigger family rebuilt under admission
func (s *Store) initFTSLive(ctx context.Context, c Conn, legacy bool, orphans []string) error {
	base, trigram := ftsSQL, ftsTrigramSQL
	if legacy {
		base, trigram = legacyFTSSQL, legacyFTSTrigramSQL
	}
	baseMissing, err := ftsTriggersMissing(ctx, c, ftsBaseTriggers)
	if err != nil {
		return err
	}
	baseMissing = baseMissing || containsString(orphans, "messages_fts")
	trigramMissing, err := ftsTriggersMissing(ctx, c, ftsTrigramTriggers)
	if err != nil {
		return err
	}
	trigramMissing = trigramMissing || containsString(orphans, "messages_fts_trigram")
	if baseMissing {
		return s.runAdmittedStartupRebuild(ctx, c, func() error {
			enabled, err := s.ensureFTSSchema(ctx, c, "messages_fts", base)
			s.fts().set(func(f *ftsFlags) { f.enabled = enabled })
			if err != nil || !enabled {
				return err
			}
			tri, err := s.ensureFTSSchema(ctx, c, "messages_fts_trigram", trigram)
			s.fts().set(func(f *ftsFlags) { f.trigram = tri })
			if err != nil {
				return err
			}
			if err := rebuildFTSIndexes(ctx, c, legacy, tri); err != nil {
				return err
			}
			if !legacy {
				return s.ensureFTSCJKSchema(ctx, c)
			}
			return nil
		})
	}
	enabled, err := s.ensureFTSSchema(ctx, c, "messages_fts", base)
	s.fts().set(func(f *ftsFlags) { f.enabled = enabled })
	if err != nil {
		return err
	}
	if enabled {
		tri, err := s.ensureFTSSchema(ctx, c, "messages_fts_trigram", trigram)
		s.fts().set(func(f *ftsFlags) { f.trigram = tri })
		if err != nil {
			return err
		}
		if tri && trigramMissing {
			if err := s.runAdmittedStartupRebuild(ctx, c, func() error { return rebuildFTSIndexes(ctx, c, legacy, tri) }); err != nil {
				return err
			}
		}
	}
	if ftsGet(s.fts(), func(f *ftsFlags) bool { return f.enabled }) && !legacy {
		return s.ensureFTSCJKSchema(ctx, c)
	}
	return nil
}

// Writes fts_storage_version once the stored layout is exactly current and nothing is in flight, reading before writing
func (s *Store) stampSettledFTSLayout(ctx context.Context, c Conn) error {
	if ftsGet(s.fts(), func(f *ftsFlags) bool { return f.stale }) {
		return nil
	}
	current, err := ftsLayoutCurrent(ctx, c)
	if err != nil || !current {
		return err
	}
	if _, pending, err := metaValue(ctx, c, "fts_rebuild_high_water"); err != nil || pending {
		return err
	}
	if trash, err := hasFTSTrash(ctx, c); err != nil || trash {
		return err
	}
	if empty, err := ftsExternalIndexEmptyWithMessages(ctx, c); err != nil || empty {
		return err
	}
	version := fmt.Sprint(FTSStorageVersion)
	if v, ok, err := metaValue(ctx, c, "fts_storage_version"); err != nil || (ok && v == version) {
		return err
	}
	_, err = c.ExecContext(ctx, stateMetaUpsertSQL, "fts_storage_version", version)
	return err
}

// Name prefix of demoted v22 shadow tables awaiting chunked teardown
const ftsTrashPrefix = "fts_v22_trash_"

// Reports demoted v22 shadow tables still awaiting teardown
func hasFTSTrash(ctx context.Context, c Conn) (bool, error) {
	var one int
	err := c.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name LIKE ? ESCAPE '\' LIMIT 1`,
		strings.ReplaceAll(ftsTrashPrefix, "_", `\_`)+"%").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Reports a base index holding nothing while messages has rows, an absent table not counting
func ftsExternalIndexEmptyWithMessages(ctx context.Context, c Conn) (bool, error) {
	var populated int
	if err := c.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM messages)").Scan(&populated); err != nil {
		if familyOf(err) == callFamily {
			return false, nil
		}
		return false, err
	}
	if populated == 0 {
		return false, nil
	}
	var indexed int
	if err := c.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM messages_fts_docsize)").Scan(&indexed); err != nil {
		if familyOf(err) == callFamily {
			return false, nil
		}
		return false, err
	}
	return indexed == 0, nil
}
