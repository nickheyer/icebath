package state

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Message uid backfill markers and the per open slice size and time budget
const (
	messageUIDBackfillDone    = "message_uid_backfill"
	messageUIDBackfillCursor  = "message_uid_backfill_id"
	messageUIDBackfillChunk   = 2000
	messageUIDBackfillBudgetS = 1.0
	titleUniqueIndexSQL       = "CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_title_unique ON sessions(title) WHERE title IS NOT NULL"
)

// Indexes recreated after every session_model_usage rebuild
var sessionModelUsageIndexSQL = []string{
	"CREATE INDEX IF NOT EXISTS idx_session_model_usage_session ON session_model_usage(session_id)",
	"CREATE INDEX IF NOT EXISTS idx_session_model_usage_model ON session_model_usage(model)",
}

// Reports sqlite3.IntegrityError
func isIntegrityError(err error) bool {
	class, ok := sqliteClassOf(err)
	return ok && class == SQLiteIntegrityError
}

// Swallows a sqlite3.OperationalError and returns every other error
func suppressOperational(err error) error {
	if err != nil && isOperationalError(err) {
		return nil
	}
	return err
}

// Adds every SCHEMA_SQL column a live table lacks, re raising lock contention so the open retries
func (s *sessionSchema) reconcileColumns(ctx context.Context, conn SQLiteConn) error {
	expected, err := parseSchemaColumns(ctx, SchemaSQL)
	if err != nil {
		return err
	}
	for _, table := range expected.Keys {
		declared, _ := expected.Vals[table].(*pyjson.Object)
		rows, err := fetchAllRows(ctx, conn, fmt.Sprintf(`PRAGMA table_info("%s")`, table))
		if err != nil {
			if isOperationalError(err) {
				continue
			}
			return err
		}
		if len(rows) == 0 {
			continue
		}
		live := map[any]bool{}
		for _, row := range rows {
			live[row[1]] = true
		}
		for _, column := range declared.Keys {
			if live[column] {
				continue
			}
			decl, _ := declared.Vals[column].(string)
			_, err := conn.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN %s %s`, table, sqlQuoteIdent(column), decl))
			if err == nil {
				continue
			}
			if !isOperationalError(err) {
				return err
			}
			if strings.Contains(pystr.Lower(errText(err)), "duplicate column") {
				slog.Debug(fmt.Sprintf("reconcile %s.%s: %s", table, column, errText(err)))
				continue
			}
			if IsSQLiteLockError(err) {
				return err
			}
			slog.Warn(fmt.Sprintf("reconcile %s.%s failed; store remains behind SCHEMA_SQL: %s", table, column, errText(err)))
		}
	}
	return nil
}

// Every row of a query as positional values
func fetchAllRows(ctx context.Context, conn SQLiteConn, query string, args ...any) ([][]any, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

// Primary key columns of a table in key order, nil when the table is missing or has no columns
func livePKColumns(ctx context.Context, conn SQLiteConn, table string) ([]string, error) {
	rows, err := fetchAllRows(ctx, conn, fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		if !isOperationalError(err) {
			return nil, err
		}
		rows = nil
	}
	if len(rows) == 0 {
		return nil, nil
	}
	var keyed [][]any
	for _, row := range rows {
		if pyTruthy(row[5]) {
			keyed = append(keyed, row)
		}
	}
	slices.SortStableFunc(keyed, func(a, b []any) int {
		x, _ := a[5].(int64)
		y, _ := b[5].(int64)
		return int(x - y)
	})
	out := []string{}
	for _, row := range keyed {
		name, _ := row[1].(string)
		out = append(out, name)
	}
	return out, nil
}

// Renames a table aside, recreates it, copies rows back, drops the copy and recreates indexes in one write transaction
func rebuildTable(ctx context.Context, conn SQLiteConn, table, legacyName, ddl, copySQL string, indexes []string) (err error) {
	auto, err := sqliteAutocommit(conn)
	if err != nil {
		return err
	}
	if !auto {
		return rebuildTableStatements(ctx, conn, table, legacyName, ddl, copySQL, indexes)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			conn.ExecContext(ctx, "ROLLBACK")
			panic(p)
		}
	}()
	if err := rebuildTableStatements(ctx, conn, table, legacyName, ddl, copySQL, indexes); err != nil {
		if _, rerr := conn.ExecContext(ctx, "ROLLBACK"); rerr != nil {
			return rerr
		}
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// Statements of one table rebuild, run inside the caller transaction
func rebuildTableStatements(ctx context.Context, conn SQLiteConn, table, legacyName, ddl, copySQL string, indexes []string) error {
	statements := append([]string{fmt.Sprintf("ALTER TABLE %s RENAME TO %s", table, legacyName), ddl, copySQL,
		fmt.Sprintf("DROP TABLE %s", legacyName)}, indexes...)
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// Python list repr of strings
func stringsRepr(values []string) string {
	items := make([]any, len(values))
	for i, v := range values {
		items[i] = v
	}
	return pyrepr.Repr(items)
}

// Rebuilds gateway_routing whose primary key predates scoping, the newest row winning a key collision
func (s *sessionSchema) healGatewayRoutingPK(ctx context.Context, conn SQLiteConn) error {
	pk, err := livePKColumns(ctx, conn, "gateway_routing")
	if err != nil || pk == nil || slices.Equal(pk, []string{"scope", "session_key"}) {
		return err
	}
	slog.Info(fmt.Sprintf("gateway_routing has legacy primary key %s; rebuilding with composite (scope, session_key) key", stringsRepr(pk)))
	return rebuildTable(ctx, conn, "gateway_routing", "gateway_routing_legacy_pk", gatewayRoutingScopedDDL,
		"INSERT OR REPLACE INTO gateway_routing (scope, session_key, entry_json, updated_at) "+
			"SELECT COALESCE(scope, ''), session_key, entry_json, updated_at "+
			"FROM gateway_routing_legacy_pk ORDER BY updated_at ASC", nil)
}

// Rebuilds session_model_usage whose primary key lacks task, with foreign keys off for the copy
func (s *sessionSchema) healSessionModelUsagePK(ctx context.Context, conn SQLiteConn) error {
	pk, err := livePKColumns(ctx, conn, "session_model_usage")
	if err != nil || pk == nil || slices.Contains(pk, "task") {
		return err
	}
	sorted := slices.Clone(pk)
	slices.Sort(sorted)
	slog.Info(fmt.Sprintf("session_model_usage has legacy primary key %s (missing task); rebuilding with composite 6-column key", stringsRepr(sorted)))
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	err = rebuildTable(ctx, conn, "session_model_usage", "session_model_usage_legacy_pk", sessionModelUsageHealDDL,
		sessionModelUsageHealCopySQL, sessionModelUsageIndexSQL)
	if err != nil && isOperationalError(err) {
		slog.Debug(fmt.Sprintf("session_model_usage PK heal skipped: %s", errText(err)))
		err = nil
	}
	if _, ferr := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); ferr != nil {
		return ferr
	}
	return err
}

// Creates the schema, reconciles columns, heals table shapes, runs data migrations and sets up FTS, then commits
func (s *sessionSchema) initSchema(ctx context.Context) error {
	reportStartupProgress(600.0, "state_db_init_schema")
	conn := s.host.writerConn()
	if err := executeScript(ctx, conn, SchemaSQL); err != nil {
		return err
	}
	for _, step := range []func(context.Context, SQLiteConn) error{s.reconcileColumns, s.healGatewayRoutingPK, s.healSessionModelUsagePK} {
		if err := step(ctx, conn); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_messages_platform_msg_id "+
		"ON messages(session_id, platform_message_id) WHERE platform_message_id IS NOT NULL"); err != nil {
		if !isOperationalError(err) {
			return err
		}
		slog.Debug(fmt.Sprintf("idx_messages_platform_msg_id create skipped: %s", errText(err)))
	}
	if err := executeDDLSkippingSettledTriggers(ctx, conn, DeferredIndexSQL); err != nil {
		return err
	}
	nullActive, err := fetchOne(ctx, conn, "SELECT 1 FROM messages WHERE active IS NULL LIMIT 1")
	if err == nil && nullActive != nil {
		_, err = conn.ExecContext(ctx, "UPDATE messages SET active = 1 WHERE active IS NULL")
	}
	if err := suppressOperational(err); err != nil {
		return err
	}
	fts5, err := s.sqliteSupportsFTS5(ctx, conn)
	if err != nil {
		return err
	}
	stale, err := fetchOne(ctx, conn, "SELECT 1 FROM state_meta WHERE key = ? LIMIT 1", FTSStaleKey)
	if err != nil {
		return err
	}
	s.fts.ftsStale.Store(stale != nil)
	if stale != nil {
		if err := s.fts.dropAllFTSTriggers(ctx, conn); err != nil {
			return err
		}
	}
	if !fts5 {
		if err := dropFTSTriggers(ctx, conn); err != nil {
			return err
		}
	}
	version, err := fetchOne(ctx, conn, "SELECT version FROM schema_version LIMIT 1")
	if err != nil {
		return err
	}
	if version == nil {
		if err := s.stampFreshStore(ctx, conn); err != nil {
			return err
		}
	} else if err := s.runDataMigrations(ctx, conn, version[0], fts5); err != nil {
		return err
	}
	if err := s.ensureUniqueTitleIndex(ctx, conn); err != nil {
		return err
	}
	if fts5 {
		if err := s.initFTS(ctx, conn); err != nil {
			return err
		}
		if err := s.stampSettledFTSLayout(ctx, conn); err != nil {
			return err
		}
	}
	return endTransaction(ctx, conn, "COMMIT")
}

// Writes the current schema version and the provenance markers of a freshly created store
func (s *sessionSchema) stampFreshStore(ctx context.Context, conn SQLiteConn) error {
	if _, err := conn.ExecContext(ctx, "INSERT INTO schema_version (version) VALUES (?)", SchemaVersion); err != nil {
		return err
	}
	nowISO := pydatetime.ISOFormatUTC(time.Now())
	for _, kv := range [][2]string{{"store_instance_id", uuid.NewString()}, {"store_created_at_utc", nowISO}, {messageUIDBackfillDone, "1"}} {
		if _, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO state_meta (key, value) VALUES (?, ?)", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// Writes fts_storage_version once the stored layout is exactly current and no optimize work is in flight
func (s *sessionSchema) stampSettledFTSLayout(ctx context.Context, conn SQLiteConn) error {
	if s.fts.ftsStale.Load() {
		return nil
	}
	checks := []func() (bool, error){
		func() (bool, error) { current, err := FTSLayoutCurrent(ctx, conn); return !current, err },
		func() (bool, error) { row, err := metaRow(ctx, conn, "fts_rebuild_high_water"); return row != nil, err },
		func() (bool, error) { return s.fts.hasFTSTrash(ctx, conn) },
		func() (bool, error) { return s.host.ftsExternalIndexEmptyWithMessages(ctx, conn) },
	}
	for _, unsettled := range checks {
		if hit, err := unsettled(); err != nil || hit {
			return err
		}
	}
	version := strconv.Itoa(FTSStorageVersion)
	row, err := fetchOne(ctx, conn, "SELECT 1 FROM state_meta WHERE key = 'fts_storage_version' AND value = ? LIMIT 1", version)
	if err != nil || row != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, stateMetaUpsertSQL, "fts_storage_version", version)
	return err
}

// Python less than between a stored schema version and an int, a TypeError for a non numeric version
func schemaVersionBelow(version any, n int) (bool, error) {
	switch v := version.(type) {
	case int64:
		return v < int64(n), nil
	case float64:
		return v < float64(n), nil
	}
	return false, &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("'<' not supported between instances of '%s' and 'int'", sqliteValueTypeName(version))}
}

// Python type name of a value read from SQLite
func sqliteValueTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case string:
		return "str"
	case []byte:
		return "bytes"
	}
	return pyrepr.TypeName(v)
}

// Version gated data migrations, advancing schema_version unless FTS5 is unavailable or an FTS step is unfinished
func (s *sessionSchema) runDataMigrations(ctx context.Context, conn SQLiteConn, currentVersion any, fts5 bool) error {
	reportStartupProgress(600.0, "state_db_data_migrations")
	below := func(n int) bool {
		b, _ := schemaVersionBelow(currentVersion, n)
		return b
	}
	if _, err := schemaVersionBelow(currentVersion, 0); err != nil {
		return err
	}
	if below(16) {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET model_config = json_set("+
			"COALESCE(model_config, '{}'), '$._delegate_from', parent_session_id) "+
			"WHERE parent_session_id IS NOT NULL "+
			"AND "+sqlJSONExtract("model_config", "$._delegate_from")+" IS NULL "+
			"AND "+ephemeralChildSQL("sessions"))
		if err == nil {
			_, err = conn.ExecContext(ctx, "UPDATE sessions SET model_config = json_set("+
				"COALESCE(model_config, '{}'), '$._delegate_from', '__orphaned__') WHERE parent_session_id IS NULL "+
				"AND "+sqlJSONExtract("model_config", "$._delegate_from")+" IS NULL "+
				"AND "+sqlJSONExtract("model_config", "$._branched_from")+" IS NULL "+
				"AND title IS NULL AND message_count <= 25 AND EXISTS (SELECT 1 FROM messages m "+
				"            WHERE m.session_id = sessions.id AND m.role = 'tool') "+
				"AND NOT EXISTS (SELECT 1 FROM sessions ch "+
				"                WHERE ch.parent_session_id = sessions.id)")
		}
		if err := suppressOperational(err); err != nil {
			return err
		}
	}
	if below(18) {
		if err := s.backfillGatewayMetadataFromSessionsJSON(ctx, conn); err != nil {
			slog.Debug(fmt.Sprintf("v18 gateway metadata backfill skipped: %s", errText(err)))
		}
	}
	if below(20) {
		if _, err := conn.ExecContext(ctx, sessionModelUsageV20SeedSQL); suppressOperational(err) != nil {
			return err
		}
	}
	if below(22) {
		if err := s.migrateV22SessionModelUsage(ctx, conn); err != nil {
			return err
		}
	}
	if below(23) && fts5 {
		needs, err := dbNeedsFTSStorageUpgrade(ctx, conn)
		if err != nil {
			return err
		}
		if needs {
			if err := s.host.setMeta(ctx, "fts_optimize_available", "1", conn); err != nil {
				return err
			}
		}
	}
	if below(25) {
		if err := s.dedupeLegacySystemPrompts(ctx, conn); err != nil {
			return err
		}
	}
	if err := s.advanceMessageUIDBackfill(ctx, conn); err != nil {
		return err
	}
	complete := true
	if below(30) && fts5 {
		var err error
		if complete, err = s.migrateTrigramCronExclusion(ctx, conn); err != nil {
			return err
		}
	}
	if below(SchemaVersion) && complete && fts5 {
		if _, err := conn.ExecContext(ctx, "UPDATE schema_version SET version = ?", SchemaVersion); err != nil {
			return err
		}
	}
	return nil
}

// Mints message_uid onto legacy rows one bounded rowid slice per open, resuming from the stored cursor
func (s *sessionSchema) advanceMessageUIDBackfill(ctx context.Context, conn SQLiteConn) error {
	done, err := metaRow(ctx, conn, messageUIDBackfillDone)
	if err != nil || done != nil {
		return err
	}
	cursorRow, err := metaRow(ctx, conn, messageUIDBackfillCursor)
	if err != nil {
		return err
	}
	doneThrough := big.NewInt(0)
	if cursorRow != nil {
		if doneThrough, err = pyint.Of(cursorRow[0]); err != nil {
			return err
		}
	}
	highRow, err := fetchOne(ctx, conn, "SELECT MAX(rowid) FROM messages")
	if err != nil {
		return err
	}
	high, _ := highRow[0].(int64)
	if doneThrough.Cmp(big.NewInt(high)) < 0 {
		if !doneThrough.IsInt64() || doneThrough.Int64() > math.MaxInt64-messageUIDBackfillChunk {
			return &pyint.Error{Kind: "OverflowError", Msg: "Python int too large to convert to SQLite INTEGER"}
		}
		through := doneThrough.Int64()
		deadline := monotonicSeconds() + messageUIDBackfillBudgetS
		for through < high {
			upper := through + messageUIDBackfillChunk
			if _, err := conn.ExecContext(ctx, "UPDATE messages SET message_uid = lower(hex(randomblob(16))) "+
				"WHERE rowid > ? AND rowid <= ? AND message_uid IS NULL", through, upper); err != nil {
				return err
			}
			through = upper
			if through < high && monotonicSeconds() >= deadline {
				return s.host.setMeta(ctx, messageUIDBackfillCursor, strconv.FormatInt(through, 10), conn)
			}
		}
	}
	if err := s.host.setMeta(ctx, messageUIDBackfillDone, "1", conn); err != nil {
		return err
	}
	return deleteMeta(ctx, conn, messageUIDBackfillCursor)
}

// Rebuilds session_model_usage with task in its primary key, existing rows becoming main loop rows
func (s *sessionSchema) migrateV22SessionModelUsage(ctx context.Context, conn SQLiteConn) error {
	row, err := fetchOne(ctx, conn, "SELECT COUNT(*) FROM pragma_table_info('session_model_usage') WHERE name = 'task' AND pk > 0")
	if err == nil && pyTruthy(row[0]) {
		return nil
	}
	if err == nil {
		err = rebuildTable(ctx, conn, "session_model_usage", "session_model_usage_v21", sessionModelUsageV22DDL,
			sessionModelUsageV22CopySQL, sessionModelUsageIndexSQL)
	}
	if err != nil && isOperationalError(err) {
		slog.Debug(fmt.Sprintf("v22 session_model_usage rebuild skipped: %s", errText(err)))
		return nil
	}
	return err
}

// One duplicate title row of the repair scan
type titleDupRow struct {
	rowid     int64
	title     any
	source    any
	startedAt any
}

// Python ordering of two started_at values, a TypeError for values Python cannot compare
func pyCompareValues(a, b any) (int, error) {
	if pyrepr.Equal(a, b) {
		return 0, nil
	}
	if x, ok := a.(float64); ok && math.IsNaN(x) {
		return 0, nil
	}
	if y, ok := b.(float64); ok && math.IsNaN(y) {
		return 0, nil
	}
	if ab, ok := a.([]byte); ok {
		if bb, ok := b.([]byte); ok {
			return strings.Compare(string(ab), string(bb)), nil
		}
	}
	af, aNum := pyNumber(a)
	bf, bNum := pyNumber(b)
	if aNum && bNum {
		return af.Cmp(bf), nil
	}
	as, aStr := a.(string)
	bs, bStr := b.(string)
	if aStr && bStr {
		return strings.Compare(as, bs), nil
	}
	return 0, &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("'<' not supported between instances of '%s' and '%s'",
		sqliteValueTypeName(b), sqliteValueTypeName(a))}
}

// Exact numeric value of an int or float read from SQLite
func pyNumber(v any) (*big.Float, bool) {
	switch t := v.(type) {
	case int64:
		return new(big.Float).SetInt64(t), true
	case float64:
		if math.IsNaN(t) {
			return nil, false
		}
		return big.NewFloat(t), true
	}
	return nil, false
}

// Creates the unique title index, first resolving duplicate titles by provenance inside a savepoint
func (s *sessionSchema) ensureUniqueTitleIndex(ctx context.Context, conn SQLiteConn) error {
	_, err := conn.ExecContext(ctx, titleUniqueIndexSQL)
	if err == nil || isOperationalError(err) {
		return nil
	}
	if !isIntegrityError(err) {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SAVEPOINT title_repair"); err != nil {
		return err
	}
	if err := s.repairDuplicateTitles(ctx, conn); err != nil {
		if _, ok := sqliteClassOf(err); !ok {
			return err
		}
		if _, err := conn.ExecContext(ctx, "ROLLBACK TO title_repair"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "RELEASE title_repair"); err != nil {
			return err
		}
		slog.Error("Could not repair duplicate session titles; unique title index not created", "error", err)
		return nil
	}
	return nil
}

// Renames newer duplicate user titles, clears lower ranked auto duplicates and creates the index
func (s *sessionSchema) repairDuplicateTitles(ctx context.Context, conn SQLiteConn) error {
	raw, err := fetchAllRows(ctx, conn, "SELECT rowid, title, title_source, started_at FROM sessions WHERE title IN "+
		"(SELECT title FROM sessions WHERE title IS NOT NULL GROUP BY title HAVING COUNT(*) > 1)")
	if err != nil {
		return err
	}
	rows := make([]titleDupRow, len(raw))
	for i, r := range raw {
		rowid, _ := r[0].(int64)
		rows[i] = titleDupRow{rowid: rowid, title: r[1], source: r[2], startedAt: r[3]}
	}
	var sortErr error
	slices.SortStableFunc(rows, func(a, b titleDupRow) int {
		if c := titleRankOf(b.source) - titleRankOf(a.source); c != 0 {
			return c
		}
		c, err := pyCompareValues(b.startedAt, a.startedAt)
		if err != nil && sortErr == nil {
			sortErr = err
		}
		if c != 0 {
			return c
		}
		return int(b.rowid - a.rowid)
	})
	if sortErr != nil {
		return sortErr
	}
	var order []string
	groups := map[string][]titleDupRow{}
	for _, row := range rows {
		title, _ := row.title.(string)
		if _, seen := groups[title]; !seen {
			order = append(order, title)
		}
		groups[title] = append(groups[title], row)
	}
	userRank := titleSourceRank[TitleSourceUser]
	for _, title := range order {
		group := groups[title]
		var users []titleDupRow
		for i := len(group) - 1; i >= 0; i-- {
			if titleRankOf(group[i].source) == userRank {
				users = append(users, group[i])
			}
		}
		keep := group[0]
		if len(users) > 0 {
			keep = users[0]
		}
		for _, row := range users[min(1, len(users)):] {
			renamed, err := nextTitleInLineage(ctx, conn, title)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET title = ? WHERE rowid = ?", renamed, row.rowid); err != nil {
				return err
			}
			slog.Warn(fmt.Sprintf("Renamed duplicate user session title %s to %s", pyrepr.StrRepr(title), pyrepr.StrRepr(renamed)))
		}
		for _, row := range group {
			if row.rowid != keep.rowid && titleRankOf(row.source) != userRank {
				if _, err := conn.ExecContext(ctx, "UPDATE sessions SET title = NULL WHERE rowid = ?", row.rowid); err != nil {
					return err
				}
				slog.Warn(fmt.Sprintf("Cleared duplicate %s session title %s", pyrepr.Str(row.source), pyrepr.StrRepr(title)))
			}
		}
	}
	if _, err := conn.ExecContext(ctx, titleUniqueIndexSQL); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "RELEASE title_repair")
	return err
}

// Creates or repairs the FTS objects, recovering a stale index and rebuilding under admission when triggers are missing
func (s *sessionSchema) initFTS(ctx context.Context, conn SQLiteConn) error {
	legacy, err := dbHasLegacyInlineFTS(ctx, conn)
	if err != nil {
		return err
	}
	orphanRepaired, err := dropOrphanFTSShadowTables(ctx, conn, []string{"messages_fts", "messages_fts_trigram", "messages_fts_cjk"})
	if err != nil {
		return err
	}
	if !s.fts.ftsStale.Load() {
		if err := s.migrateMisalignedFTSSource(ctx, conn, legacy); err != nil {
			return err
		}
	}
	if s.fts.ftsStale.Load() {
		recovered, err := s.recoverStaleFTS(ctx, conn, legacy, nil)
		if err != nil {
			return err
		}
		if recovered {
			if err := s.fts.ensureFTSCJKSchema(ctx, conn); err != nil {
				return err
			}
		} else {
			s.fts.ftsEnabled.Store(false)
			s.fts.trigramAvailable.Store(false)
			s.fts.ftsCJKAvailable.Store(false)
		}
	} else if err := s.initLiveFTS(ctx, conn, legacy, orphanRepaired); err != nil {
		return err
	}
	if s.fts.ftsEnabled.Load() {
		_, err := s.migrateBroadFTSUpdateTriggers(ctx, conn)
		return err
	}
	return nil
}

// Ensures the base, trigram and CJK indexes of a non stale store, rebuilding missing trigger families under admission
func (s *sessionSchema) initLiveFTS(ctx context.Context, conn SQLiteConn, legacy bool, orphanRepaired []string) error {
	baseSQL, trigramSQL := ftsDDL(legacy)
	baseMissing, err := ftsTriggersMissing(ctx, conn, ftsBaseTriggers)
	if err != nil {
		return err
	}
	baseMissing = baseMissing || slices.Contains(orphanRepaired, "messages_fts")
	trigramMissing, err := ftsTriggersMissing(ctx, conn, ftsTrigramTriggers)
	if err != nil {
		return err
	}
	trigramMissing = trigramMissing || slices.Contains(orphanRepaired, "messages_fts_trigram")
	ensureAndRebuild := func() error {
		enabled, err := s.fts.ensureFTSSchema(ctx, conn, "messages_fts", baseSQL)
		if err != nil {
			return err
		}
		s.fts.ftsEnabled.Store(enabled)
		if !enabled {
			return nil
		}
		trigram, err := s.fts.ensureFTSSchema(ctx, conn, "messages_fts_trigram", trigramSQL)
		if err != nil {
			return err
		}
		s.fts.trigramAvailable.Store(trigram)
		if err := rebuildFTSIndexes(ctx, conn, legacy, trigram); err != nil {
			return err
		}
		if !legacy {
			return s.fts.ensureFTSCJKSchema(ctx, conn)
		}
		return nil
	}
	if baseMissing {
		if err := s.runAdmittedStartupRebuild(ctx, conn, ensureAndRebuild); err != nil {
			return err
		}
	} else {
		enabled, err := s.fts.ensureFTSSchema(ctx, conn, "messages_fts", baseSQL)
		if err != nil {
			return err
		}
		s.fts.ftsEnabled.Store(enabled)
		if enabled {
			trigram, err := s.fts.ensureFTSSchema(ctx, conn, "messages_fts_trigram", trigramSQL)
			if err != nil {
				return err
			}
			s.fts.trigramAvailable.Store(trigram)
			if trigram && trigramMissing {
				if err := s.runAdmittedStartupRebuild(ctx, conn, func() error {
					return rebuildFTSIndexes(ctx, conn, legacy, trigram)
				}); err != nil {
					return err
				}
			}
		}
	}
	if s.fts.ftsEnabled.Load() && !legacy && !baseMissing {
		return s.fts.ensureFTSCJKSchema(ctx, conn)
	}
	return nil
}

// Python sqlite3 binding of a decoded JSON value, the ProgrammingError or encode error sqlite3 raises for the rest
func sqliteParam(index int, v any) (any, error) {
	switch t := v.(type) {
	case nil, int64, float64, []byte:
		return t, nil
	case bool:
		if t {
			return int64(1), nil
		}
		return int64(0), nil
	case string:
		if _, bad := pystr.FirstUnencodableSurrogate(t); bad {
			return nil, pystr.StrictUTF8EncodeError(t)
		}
		return t, nil
	case *big.Int:
		if t.IsInt64() {
			return t.Int64(), nil
		}
		return nil, &pyint.Error{Kind: "OverflowError", Msg: "Python int too large to convert to SQLite INTEGER"}
	}
	return nil, &SQLiteError{Class: SQLiteProgrammingError, Msg: fmt.Sprintf("Error binding parameter %d: type '%s' is not supported", index, pyrepr.TypeName(v))}
}

// Fills NULL gateway metadata columns from the legacy sessions.json index, never overwriting newer data
func (s *sessionSchema) backfillGatewayMetadataFromSessionsJSON(ctx context.Context, conn SQLiteConn) error {
	path := filepath.Join(home.Dir(ctx), "sessions", "sessions.json")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text, err := pyDecodeUTF8Strict(raw)
	if err != nil {
		return err
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return err
	}
	data, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil
	}
	for _, key := range data.Keys {
		entry, ok := data.Vals[key].(*pyjson.Object)
		if strings.HasPrefix(key, "_") || !ok {
			continue
		}
		get := func(k string) any { v, _ := entry.Get(k); return v }
		sessionID := get("session_id")
		if !pyrepr.Truthy(sessionID) {
			continue
		}
		origin, isDict := get("origin").(*pyjson.Object)
		var chatID, threadID, originJSON any
		if isDict {
			chatID, _ = origin.Get("chat_id")
			threadID, _ = origin.Get("thread_id")
			if originJSON, err = pyjson.Dumps(origin); err != nil {
				return err
			}
		}
		sessionKey := get("session_key")
		if !pyrepr.Truthy(sessionKey) {
			sessionKey = key
		}
		finalized := int64(0)
		if pyrepr.Truthy(get("expiry_finalized")) || pyrepr.Truthy(get("memory_flushed")) {
			finalized = 1
		}
		values := []any{sessionKey, chatID, get("chat_type"), threadID, get("display_name"), originJSON, finalized, pyrepr.Str(sessionID)}
		params := make([]any, len(values))
		for i, v := range values {
			if params[i], err = sqliteParam(i+1, v); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, gatewayMetadataBackfillSQL, params...); err != nil {
			return err
		}
	}
	return nil
}

// Brings a raw state.db connection to the declared shape without committing, columns before and after the DDL
func ApplyDeclaredSchema(ctx context.Context, conn SQLiteConn) error {
	shim := &sessionSchema{}
	if err := shim.reconcileColumns(ctx, conn); err != nil {
		return err
	}
	if err := executeDDLScriptTransactional(ctx, conn, SchemaSQL); err != nil {
		return err
	}
	return shim.reconcileColumns(ctx, conn)
}

// Brings a raw state.db connection to the declared shape and commits an open transaction
func ReconcileStateSchema(ctx context.Context, conn SQLiteConn) error {
	if err := ApplyDeclaredSchema(ctx, conn); err != nil {
		return err
	}
	return endTransaction(ctx, conn, "COMMIT")
}
