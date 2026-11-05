package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Trigger names split by the DDL half that creates them, trigram triggers needing the trigram tokenizer
var (
	ftsTrigramTriggers = func() []string {
		var out []string
		for _, name := range ftsTriggers {
			if strings.Contains(name, "_trigram_") {
				out = append(out, name)
			}
		}
		return out
	}()
	ftsBaseTriggers = func() []string {
		var out []string
		for _, name := range ftsTriggers {
			if !slices.Contains(ftsTrigramTriggers, name) {
				out = append(out, name)
			}
		}
		return out
	}()
)

// Base and trigram FTS DDL of the legacy inline layout or the v23 external content layout
func ftsDDL(legacy bool) (string, string) {
	if legacy {
		return LegacyFTSSQL, LegacyFTSTrigramSQL
	}
	return FTSSQL, FTSTrigramSQL
}

const (
	legacyInlineConcatSQL       = "COALESCE(content, '') || ' ' || COALESCE(tool_name, '') || ' ' || COALESCE(tool_calls, '') "
	staleKeyUpsertSQL           = "INSERT INTO state_meta (key, value) VALUES (?, '1') ON CONFLICT(key) DO UPDATE SET value = excluded.value"
	stateMetaUpsertSQL          = "INSERT INTO state_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value"
	clearRebuildMarkersSQL      = "DELETE FROM state_meta WHERE key IN ('fts_rebuild_high_water', 'fts_rebuild_progress')"
	dropRetiredToolHighWaterSQL = "DELETE FROM state_meta WHERE key = 'fts_tool_full_content_high_water'"
)

// Double quotes an SQL identifier
func sqlQuoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// Hex SHA-256 of the UTF-8 text, the UnicodeEncodeError Python raises for a lone surrogate
func sha256HexUTF8(text string) (string, error) {
	for i, pos := 0, 0; i < len(text); pos++ {
		r, n := pystr.DecodeRune(text[i:])
		if pystr.IsSurrogate(r) {
			return "", &pyint.Error{Kind: "UnicodeEncodeError", Msg: fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", r, pos)}
		}
		i += n
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]), nil
}

// Stores a prompt once in the content addressed table and returns its hash, nil for a nil prompt
func storeSystemPrompt(ctx context.Context, conn SQLiteConn, systemPrompt any) (any, error) {
	if systemPrompt == nil {
		return nil, nil
	}
	text, ok := systemPrompt.(string)
	if !ok {
		return nil, &pyint.Error{Kind: "AttributeError", Msg: "'" + pyrepr.TypeName(systemPrompt) + "' object has no attribute 'encode'"}
	}
	hash, err := sha256HexUTF8(text)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO system_prompts (hash, prompt) VALUES (?, ?)", hash, text); err != nil {
		return nil, err
	}
	return hash, nil
}

// Feeds each complete SQL statement accumulated line by line to fn, returning the incomplete remainder
func forEachCompleteStatement(script string, fn func(statement string) error) (string, error) {
	if strings.IndexByte(script, 0) >= 0 {
		return "", &pyint.Error{Kind: "ValueError", Msg: "embedded null character"}
	}
	tls := libc.NewTLS()
	defer tls.Close()
	statement := ""
	for _, line := range pystr.Splitlines(script, false) {
		statement += line + "\n"
		p, err := libc.CString(statement)
		if err != nil {
			return "", err
		}
		complete := sqlite3.Xsqlite3_complete(tls, p) != 0
		libc.Xfree(tls, p)
		if !complete {
			continue
		}
		current := statement
		statement = ""
		if err := fn(current); err != nil {
			return "", err
		}
	}
	return statement, nil
}

var (
	readProbeMu         sync.Mutex
	readProbeStatements []string
)

// SELECTs that fail at prepare time exactly when a live store is behind the declared schema, computed once per process
func SchemaReadProbeStatements(ctx context.Context) ([]string, error) {
	readProbeMu.Lock()
	defer readProbeMu.Unlock()
	if readProbeStatements == nil {
		tables, err := parseSchemaColumns(ctx, SchemaSQL)
		if err != nil {
			return nil, err
		}
		names := slices.Clone(tables.Keys)
		sort.Strings(names)
		statements := []string{}
		for _, table := range names {
			raw, _ := tables.Get(table)
			cols := raw.(*pyjson.Object)
			parts := make([]string, len(cols.Keys))
			for i, col := range cols.Keys {
				parts[i] = sqlQuoteIdent(table) + "." + sqlQuoteIdent(col)
			}
			statements = append(statements, "SELECT "+strings.Join(parts, ", ")+" FROM "+sqlQuoteIdent(table)+" LIMIT 0")
		}
		readProbeStatements = statements
	}
	return slices.Clone(readProbeStatements), nil
}

// Validated cached column map whose schema hash matches, nil when the cache is absent, stale or malformed
func cachedSchemaColumns(cachePath, schemaHash string) *pyjson.Object {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil
	}
	text, err := pyjson.DecodeBytes(data)
	if err != nil {
		return nil
	}
	blob, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	obj, ok := blob.(*pyjson.Object)
	if !ok {
		return nil
	}
	if hash, _ := obj.Get("schema_hash"); hash != schemaHash {
		return nil
	}
	raw, _ := obj.Get("tables")
	tables, ok := raw.(*pyjson.Object)
	if !ok {
		return nil
	}
	for _, table := range tables.Keys {
		cols, ok := tables.Vals[table].(*pyjson.Object)
		if !ok {
			return nil
		}
		for _, col := range cols.Keys {
			if _, ok := cols.Vals[col].(string); !ok {
				return nil
			}
		}
	}
	return tables
}

// Declared columns per table with ALTER TABLE type expressions, read from an in memory copy of the schema and cached on disk by DDL hash
func parseSchemaColumns(ctx context.Context, schemaSQL string) (*pyjson.Object, error) {
	schemaHash, err := sha256HexUTF8(schemaSQL)
	if err != nil {
		return nil, err
	}
	cachePath := filepath.Join(home.Dir(ctx), "cache", "schema_columns.json")
	if tables := cachedSchemaColumns(cachePath, schemaHash); tables != nil {
		return tables, nil
	}
	tables, err := func() (*pyjson.Object, error) {
		ref, err := ConnectSQLite(":memory:", SQLiteConfig{Timeout: 5})
		if err != nil {
			return nil, err
		}
		defer ref.Close()
		if err := executeScript(ctx, ref, schemaSQL); err != nil {
			return nil, err
		}
		names, err := queryStrings(ctx, ref, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
		if err != nil {
			return nil, err
		}
		tables := pyjson.NewObject()
		for _, table := range names {
			info, err := queryObjects(ctx, ref, `PRAGMA table_info("`+table+`")`)
			if err != nil {
				return nil, err
			}
			cols := pyjson.NewObject()
			for _, row := range info {
				name, _ := row.Get("name")
				colType, _ := row.Get("type")
				notNull, _ := row.Get("notnull")
				dflt, _ := row.Get("dflt_value")
				pk, _ := row.Get("pk")
				var parts []string
				if pyTruthy(colType) {
					parts = append(parts, pyStr(colType))
				}
				if pyTruthy(notNull) && !pyTruthy(pk) {
					parts = append(parts, "NOT NULL")
				}
				if dflt != nil {
					parts = append(parts, "DEFAULT "+pyStr(dflt))
				}
				cols.Set(pyStr(name), strings.Join(parts, " "))
			}
			tables.Set(table, cols)
		}
		return tables, nil
	}()
	if err != nil {
		return nil, err
	}
	writeSchemaColumnsCache(cachePath, schemaHash, tables)
	return tables, nil
}

// Atomically replaces the schema column cache, every failure leaving the cache for the next startup to recompute
func writeSchemaColumnsCache(cachePath, schemaHash string, tables *pyjson.Object) {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o777); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(cachePath), ".schema_columns.*")
	if err != nil {
		return
	}
	blob := pyjson.NewObject()
	blob.Set("schema_hash", schemaHash)
	blob.Set("tables", tables)
	text, err := pyjson.Dumps(blob)
	if err == nil {
		_, err = tmp.WriteString(text)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		os.Rename(tmp.Name(), cachePath)
	}
}

// SessionDB members the schema migrations call, implemented by the hermes_state SessionDB
type schemaHost interface {
	ftsSetupHost
	setMeta(ctx context.Context, key, value string, cursor SQLiteConn) error
	readOnly() bool
	quarantineReason() (string, bool)
	foreignStateDBHolders() ([]Holder, error)
	reapInactiveOrphanDesktopHolders(holders []Holder, minAgeSeconds float64) []int
	ftsExternalIndexEmptyWithMessages(ctx context.Context, conn SQLiteConn) (bool, error)
}

// Schema creation, column reconciliation and FTS DDL management, embedded in SessionDB so its methods are SessionDB methods
type sessionSchema struct {
	host                  schemaHost
	fts                   *sessionFTSSetup
	retryMu               sync.Mutex
	ftsDeferredHolderPIDs []int
	ftsStaleRetryAfter    float64
	ftsStaleRetryInterval float64
}

func newSessionSchema(host schemaHost, fts *sessionFTSSetup) *sessionSchema {
	return &sessionSchema{host: host, fts: fts}
}

// Moves inline prompt snapshots into the content addressed table, pausing without error on contention
func (s *sessionSchema) dedupeLegacySystemPrompts(ctx context.Context, conn SQLiteConn) error {
	rows, err := queryObjects(ctx, conn, "SELECT id, system_prompt FROM sessions WHERE system_prompt IS NOT NULL")
	if err != nil {
		if isOperationalError(err) {
			return nil
		}
		return err
	}
	for _, row := range rows {
		sessionID, _ := row.Get("id")
		prompt, _ := row.Get("system_prompt")
		hash, err := storeSystemPrompt(ctx, conn, prompt)
		if err == nil {
			_, err = conn.ExecContext(ctx, "UPDATE sessions SET system_prompt_hash = ?, system_prompt = NULL WHERE id = ?", hash, sessionID)
		}
		if err != nil {
			if isOperationalError(err) {
				slog.Warn(fmt.Sprintf("v25 prompt dedupe paused after contention (%s); "+
					"unmigrated rows keep the legacy inline prompt and the next schema init resumes the migration.", errText(err)))
				return nil
			}
			return err
		}
	}
	return nil
}

// Reports whether the linked SQLite can create an FTS5 table, warning once when the module is missing
func (s *sessionSchema) sqliteSupportsFTS5(ctx context.Context, conn SQLiteConn) (bool, error) {
	_, err := conn.ExecContext(ctx, "CREATE VIRTUAL TABLE temp._hermes_fts5_probe USING fts5(x)")
	if err == nil {
		_, err = conn.ExecContext(ctx, "DROP TABLE temp._hermes_fts5_probe")
	}
	if err == nil {
		return true, nil
	}
	if !isOperationalError(err) || !isFTS5UnavailableError(err) {
		return false, err
	}
	s.fts.warnFTS5Unavailable(err)
	return false, nil
}

// Reports whether any trigger of one DDL half is absent, false for an empty half
func ftsTriggersMissing(ctx context.Context, conn SQLiteConn, names []string) (bool, error) {
	if len(names) == 0 {
		return false, nil
	}
	row, err := fetchOne(ctx, conn, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name IN ("+sqlBindMarks(len(names))+")", stringArgs(names)...)
	if err != nil {
		return false, err
	}
	n, err := pyint.Of(row[0])
	if err != nil {
		return false, err
	}
	return n.Cmp(big.NewInt(int64(len(names)))) < 0, nil
}

// Reports whether trigger SQL is a broad AFTER UPDATE lacking an OF column list
func ftsUpdateTriggerNeedsNarrowing(sql any) bool {
	if !pyTruthy(sql) {
		return false
	}
	compact := pystr.Upper(strings.Join(pystr.SplitWhitespace(pyStr(sql), -1), " "))
	return !strings.Contains(compact, "AFTER UPDATE OF ") && strings.Contains(compact, "AFTER UPDATE ON ")
}

// Replaces broad AFTER UPDATE FTS triggers with AFTER UPDATE OF variants without a rebuild, returning how many were dropped
func (s *sessionSchema) migrateBroadFTSUpdateTriggers(ctx context.Context, conn SQLiteConn) (int, error) {
	legacy, err := dbHasLegacyInlineFTS(ctx, conn)
	if err != nil {
		return 0, err
	}
	updateNames := []string{"messages_fts_update", "messages_fts_trigram_update"}
	if !legacy {
		updateNames = append(updateNames, "messages_fts_cjk_update")
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(updateNames)), ", ")
	rows, err := queryObjects(ctx, conn, "SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND name IN ("+placeholders+")", stringArgs(updateNames)...)
	if err != nil {
		return 0, err
	}
	var toDrop []string
	for _, row := range rows {
		name, _ := row.Get("name")
		sql, _ := row.Get("sql")
		if ftsUpdateTriggerNeedsNarrowing(sql) {
			toDrop = append(toDrop, pyStr(name))
		}
	}
	if len(toDrop) == 0 {
		return 0, nil
	}
	for _, name := range toDrop {
		if _, err := conn.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+name); err != nil {
			return 0, err
		}
	}
	baseSQL, trigramSQL := ftsDDL(legacy)
	if _, err := s.fts.ensureFTSSchema(ctx, conn, "messages_fts", baseSQL); err != nil {
		return 0, err
	}
	if _, err := s.fts.ensureFTSSchema(ctx, conn, "messages_fts_trigram", trigramSQL); err != nil {
		return 0, err
	}
	if slices.Contains(toDrop, "messages_fts_cjk_update") {
		if err := s.fts.ensureFTSCJKSchema(ctx, conn); err != nil {
			s.quarantineCJKAfterUpdateOfMigration(ctx, conn)
			slog.Error("CJK FTS re-ensure after UPDATE OF migration failed\n" + errText(err))
			return 0, err
		}
		row, err := fetchOne(ctx, conn, "SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?", "messages_fts_cjk_update")
		if err != nil {
			return 0, err
		}
		if row == nil || ftsUpdateTriggerNeedsNarrowing(row[0]) {
			s.quarantineCJKAfterUpdateOfMigration(ctx, conn)
			slog.Warn("CJK FTS UPDATE trigger missing or still broad after " +
				"UPDATE OF migration; marked stale and unavailable")
		}
	}
	slog.Info(fmt.Sprintf("Migrated %d broad FTS UPDATE trigger(s) to AFTER UPDATE OF (no rebuild required)", len(toDrop)))
	return len(toDrop), nil
}

// Reports whether messages_fts is still external content over raw messages instead of the aligned source view
func ftsIndexIsMisalignedSource(ctx context.Context, conn SQLiteConn) (bool, error) {
	row, err := fetchOne(ctx, conn, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'messages_fts'")
	if err != nil || row == nil {
		return false, err
	}
	sql := ""
	if pyTruthy(row[0]) {
		sql = pyStr(row[0])
	}
	return !strings.Contains(sql, "messages_fts_src"), nil
}

// Runs DDL statement by statement, skipping each drop and create trigger pair whose stored body already matches
func executeDDLSkippingSettledTriggers(ctx context.Context, conn SQLiteConn, ddl string) error {
	rows, err := queryObjects(ctx, conn, "SELECT name, sql FROM sqlite_master WHERE type = 'trigger'")
	if err != nil {
		return err
	}
	stored := map[string]any{}
	for _, row := range rows {
		name, _ := row.Get("name")
		sql, _ := row.Get("sql")
		stored[pyStr(name)] = sql
	}
	pendingDrop := ""
	remainder, err := forEachCompleteStatement(ddl, func(statement string) error {
		current := pystr.Strip(statement)
		upper := pystr.Upper(current)
		if strings.HasPrefix(upper, "DROP TRIGGER IF EXISTS ") {
			pendingDrop = current
			return nil
		}
		if strings.HasPrefix(upper, "CREATE TRIGGER IF NOT EXISTS ") {
			desired := strings.TrimRight(strings.Replace(current, "IF NOT EXISTS ", "", 1), ";")
			fields := pystr.SplitWhitespace(desired, 3)
			if len(fields) < 3 {
				return &pyint.Error{Kind: "IndexError", Msg: "list index out of range"}
			}
			if have, ok := stored[fields[2]]; ok && have == desired {
				pendingDrop = ""
				return nil
			}
		}
		if pendingDrop != "" {
			if _, err := conn.ExecContext(ctx, pendingDrop); err != nil {
				return err
			}
			pendingDrop = ""
		}
		_, err := conn.ExecContext(ctx, current)
		return err
	})
	if err != nil {
		return err
	}
	if pystr.Strip(remainder) != "" {
		return &SQLiteError{Class: SQLiteOperationalError, Msg: "incomplete DDL statement"}
	}
	return nil
}

// Executes a DDL script statement by statement without the implicit commit of executescript
func executeDDLScriptTransactional(ctx context.Context, conn SQLiteConn, ddl string) error {
	remainder, err := forEachCompleteStatement(ddl, func(statement string) error {
		_, err := conn.ExecContext(ctx, statement)
		return err
	})
	if err != nil {
		return err
	}
	if pystr.Strip(remainder) != "" {
		return &SQLiteError{Class: SQLiteOperationalError, Msg: "incomplete FTS DDL statement"}
	}
	return nil
}

// Repoints messages_fts at the aligned source view and rebuilds it once under the shared rebuild admission
func (s *sessionSchema) migrateMisalignedFTSSource(ctx context.Context, conn SQLiteConn, legacy bool) error {
	if legacy {
		return nil
	}
	recognised, err := BaseIndexRecognised(ctx, conn)
	if err != nil || !recognised {
		return err
	}
	staleView, err := BaseSourceViewIsStale(ctx, conn)
	if err != nil {
		return err
	}
	if !staleView {
		misaligned, err := ftsIndexIsMisalignedSource(ctx, conn)
		if err != nil || !misaligned {
			return err
		}
	}
	hasMessages, err := rowExists(ctx, conn, "SELECT 1 FROM messages LIMIT 1")
	if err != nil {
		return err
	}
	exec := func(query string) error {
		_, err := conn.ExecContext(ctx, query)
		return err
	}
	doAlign := func() error {
		for _, name := range ftsBaseTriggers {
			if err := exec("DROP TRIGGER IF EXISTS " + name); err != nil {
				return err
			}
		}
		if err := exec("DROP TABLE IF EXISTS messages_fts"); err != nil {
			return err
		}
		if staleView {
			if err := exec("DROP VIEW messages_fts_src"); err != nil {
				return err
			}
		}
		if _, err := s.fts.ensureFTSSchema(ctx, conn, "messages_fts", FTSSQL); err != nil {
			return err
		}
		if hasMessages {
			if err := exec("INSERT INTO messages_fts(messages_fts) VALUES('rebuild')"); err != nil {
				return err
			}
			marker, err := metaRow(ctx, conn, "fts_rebuild_high_water")
			if err != nil {
				return err
			}
			if marker != nil {
				trigram, err := s.fts.ftsTableProbe(ctx, conn, "messages_fts_trigram")
				if err != nil {
					return err
				}
				switch trigram {
				case ftsTableStatusQueryable:
					if err := exec("INSERT INTO messages_fts_trigram(messages_fts_trigram) VALUES('rebuild')"); err != nil {
						return err
					}
				case ftsTableStatusDegraded:
					for _, name := range ftsTrigramTriggers {
						if err := exec("DROP TRIGGER IF EXISTS " + name); err != nil {
							return err
						}
					}
				}
			}
		}
		if err := exec(clearRebuildMarkersSQL); err != nil {
			return err
		}
		return exec(dropRetiredToolHighWaterSQL)
	}
	if !hasMessages {
		return withSavepoint(ctx, conn, "fts_align_empty", doAlign)
	}
	return s.runAdmittedStartupRebuild(ctx, conn, doAlign)
}

// Runs body inside a named savepoint, rolling back to it on an error or panic before propagating
func withSavepoint(ctx context.Context, conn SQLiteConn, name string, body func() error) (err error) {
	if _, err := conn.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if _, rbErr := conn.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name); rbErr != nil && err == nil {
			err = rbErr
		}
		if _, relErr := conn.ExecContext(ctx, "RELEASE SAVEPOINT "+name); relErr != nil && err == nil {
			err = relErr
		}
	}()
	if err := body(); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "RELEASE SAVEPOINT "+name); err != nil {
		return err
	}
	committed = true
	return nil
}

// Reports whether a table of the given name exists
func sqliteTableExists(ctx context.Context, conn SQLiteConn, name string) (bool, error) {
	return rowExists(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", name)
}

// Installs the source filtered trigram view and rebuilds from it, false holding the schema version back for retry
func (s *sessionSchema) migrateTrigramCronExclusion(ctx context.Context, conn SQLiteConn) (bool, error) {
	legacy, err := dbHasLegacyInlineFTS(ctx, conn)
	if err != nil {
		return false, err
	}
	if !legacy {
		legacy, err = dbHasTrigramToolCallsProjection(ctx, conn)
		if err != nil {
			return false, err
		}
	}
	if legacy {
		return true, nil
	}
	trigram, err := s.fts.ftsTableProbe(ctx, conn, "messages_fts_trigram")
	if err != nil {
		return false, err
	}
	if trigram != ftsTableStatusQueryable {
		return trigram == ftsTableStatusAbsent, nil
	}
	for _, name := range ftsTrigramTriggers {
		if _, err := conn.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+name); err != nil {
			return false, err
		}
	}
	if _, err := conn.ExecContext(ctx, "DROP VIEW IF EXISTS messages_fts_trigram_src"); err != nil {
		return false, err
	}
	ok, err := s.fts.ensureFTSSchema(ctx, conn, "messages_fts_trigram", FTSTrigramSQL)
	if err != nil || !ok {
		return false, err
	}
	err = s.runAdmittedStartupRebuild(ctx, conn, func() error {
		_, err := conn.ExecContext(ctx, "INSERT INTO messages_fts_trigram(messages_fts_trigram) VALUES('rebuild')")
		return err
	})
	return err == nil, err
}

// Fails CJK search closed after its UPDATE trigger was dropped mid migration, persisting the stale breadcrumb
func (s *sessionSchema) quarantineCJKAfterUpdateOfMigration(ctx context.Context, conn SQLiteConn) {
	s.fts.ftsCJKAvailable.Store(false)
	if err := s.host.setMeta(ctx, FTSCJKStaleKey, "1", conn); err != nil {
		slog.Debug("Could not persist CJK FTS stale breadcrumb\n" + errText(err))
	}
	if _, err := conn.ExecContext(ctx, "DROP TRIGGER IF EXISTS messages_fts_cjk_update"); err != nil {
		slog.Debug("Could not drop residual CJK UPDATE trigger after quarantine\n" + errText(err))
	}
}

// Rebuilds the FTS indexes from their source, legacy inline indexes by delete and reinsert of the concatenated columns
func rebuildFTSIndexes(ctx context.Context, conn SQLiteConn, legacy, includeTrigram bool) error {
	tables := []string{"messages_fts"}
	if includeTrigram {
		tables = append(tables, "messages_fts_trigram")
	}
	for _, table := range tables {
		var statements []string
		if legacy {
			statements = []string{"DELETE FROM " + table, "INSERT INTO " + table + "(rowid, content) SELECT id, " + legacyInlineConcatSQL + "FROM messages"}
		} else {
			statements = []string{"INSERT INTO " + table + "(" + table + ") VALUES('rebuild')"}
		}
		for _, query := range statements {
			if _, err := conn.ExecContext(ctx, query); err != nil {
				return err
			}
		}
	}
	if legacy {
		return nil
	}
	_, err := conn.ExecContext(ctx, clearRebuildMarkersSQL)
	return err
}

// Rebuild admission the startup paths consult, the schema module binding hermes tests patch
var startupRebuildAdmission = FTSRebuildAdmission

// Runs a startup FTS rebuild under cross process admission, detaching FTS sync with a stale breadcrumb when refused
func (s *sessionSchema) runAdmittedStartupRebuild(ctx context.Context, conn SQLiteConn, rebuild func() error) error {
	admittedRun := false
	err := startupRebuildAdmission(s.host.dbPath(), nil, func(admitted bool) error {
		if !admitted {
			return nil
		}
		admittedRun = true
		return rebuild()
	})
	if err != nil || admittedRun {
		return err
	}
	slog.Warn("Deferred startup FTS rebuild: another process holds the " +
		"rebuild authority for this state.db; detaching FTS sync until the stale-index recovery path rebuilds it.")
	if _, err := conn.ExecContext(ctx, staleKeyUpsertSQL, FTSStaleKey); err != nil {
		return err
	}
	if err := s.fts.dropAllFTSTriggers(ctx, conn); err != nil {
		return err
	}
	s.fts.ftsStale.Store(true)
	s.fts.ftsEnabled.Store(false)
	s.fts.trigramAvailable.Store(false)
	s.fts.ftsCJKAvailable.Store(false)
	return nil
}
