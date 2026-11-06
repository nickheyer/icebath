package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func schemaFixture(t *testing.T, rows [][3]string) (*sessionSchema, *fakeSearchHost) {
	t.Helper()
	s, host := searchStore(t, rows)
	return newSessionSchema(&fakeSchemaHost{fakeSearchHost: host, search: s}, s.fts), host
}

func triggerSQL(t *testing.T, conn SQLiteConn, name string) string {
	t.Helper()
	row, err := fetchOne(context.Background(), conn, "SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?", name)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		return ""
	}
	return row[0].(string)
}

func TestFTSTriggerHalvesAndDDL(t *testing.T) {
	if len(ftsTrigramTriggers) == 0 || len(ftsTrigramTriggers)+len(ftsBaseTriggers) != len(ftsTriggers) {
		t.Fatalf("halves %v %v", ftsTrigramTriggers, ftsBaseTriggers)
	}
	for _, name := range ftsTrigramTriggers {
		if !strings.Contains(name, "_trigram_") || slices.Contains(ftsBaseTriggers, name) {
			t.Fatalf("trigram half %q", name)
		}
	}
	if base, trigram := ftsDDL(true); base != LegacyFTSSQL || trigram != LegacyFTSTrigramSQL {
		t.Fatal("legacy DDL pair")
	}
	if base, trigram := ftsDDL(false); base != FTSSQL || trigram != FTSTrigramSQL {
		t.Fatal("v23 DDL pair")
	}
	if got := sqlQuoteIdent(`a"b`); got != `"a""b"` {
		t.Fatalf("quoted %s", got)
	}
}

func TestSchemaColumnsCacheAndReadProbes(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cachePath := filepath.Join(os.Getenv("ICEBATH_HOME"), "cache", "schema_columns.json")
	tables, err := parseSchemaColumns(ctx, SchemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := tables.Get("sessions")
	if decl, _ := sessions.(*pyjson.Object).Get("archived"); decl != "INTEGER NOT NULL DEFAULT 0" {
		t.Fatalf("archived declaration %v", decl)
	}
	if decl, _ := sessions.(*pyjson.Object).Get("id"); decl != "TEXT" {
		t.Fatalf("primary key drops NOT NULL %v", decl)
	}
	body, err := os.ReadFile(cachePath)
	hash, _ := sha256HexUTF8(SchemaSQL)
	if err != nil || !strings.HasPrefix(string(body), `{"schema_hash": "`+hash+`", "tables": {`) {
		t.Fatalf("cache %q %v", body, err)
	}
	if err := os.WriteFile(cachePath, []byte(`{"schema_hash": "`+hash+`", "tables": {"only": {"c": "TEXT"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cached, err := parseSchemaColumns(ctx, SchemaSQL)
	if err != nil || objJSON(t, cached) != `{"only":{"c":"TEXT"}}` {
		t.Fatalf("a matching cache is served %v", err)
	}
	for _, bad := range []string{`{"schema_hash": "other", "tables": {}}`, `{"schema_hash": "` + hash + `", "tables": {"t": {"c": 1}}}`, `not json`, `[]`} {
		if err := os.WriteFile(cachePath, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		again, err := parseSchemaColumns(ctx, SchemaSQL)
		if err != nil || !slices.Contains(again.Keys, "sessions") {
			t.Fatalf("cache %s recomputes: %v", bad, err)
		}
	}
	statements, err := SchemaReadProbeStatements(ctx)
	if err != nil || len(statements) != len(tables.Keys) {
		t.Fatalf("one probe per table %d %v", len(statements), err)
	}
	if !slices.IsSortedFunc(statements, func(a, b string) int {
		return strings.Compare(a[strings.Index(a, " FROM "):], b[strings.Index(b, " FROM "):])
	}) {
		t.Fatal("probes ordered by table name")
	}
	conn, _ := schemaConn(t)
	for _, stmt := range statements {
		if !strings.HasPrefix(stmt, "SELECT \"") || !strings.HasSuffix(stmt, " LIMIT 0") {
			t.Fatalf("probe text %s", stmt)
		}
		rows, err := conn.QueryContext(ctx, stmt)
		if err != nil {
			t.Fatalf("current store passes %s: %v", stmt, err)
		}
		rows.Close()
	}
	mustExecConn(t, conn, "ALTER TABLE sessions DROP COLUMN archived")
	failed := 0
	for _, stmt := range statements {
		rows, err := conn.QueryContext(ctx, stmt)
		if err != nil {
			failed++
			continue
		}
		rows.Close()
	}
	if failed != 1 {
		t.Fatalf("a missing column fails exactly its table probe: %d", failed)
	}
}

func TestStoreSystemPromptAndLegacyDedupe(t *testing.T) {
	ctx := context.Background()
	schema, host := schemaFixture(t, nil)
	conn := host.conn
	if hash, err := storeSystemPrompt(ctx, conn, nil); hash != nil || err != nil {
		t.Fatalf("nil prompt %v %v", hash, err)
	}
	hash, err := storeSystemPrompt(ctx, conn, "be brief")
	want, _ := sha256HexUTF8("be brief")
	if err != nil || hash != want || len(want) != 64 {
		t.Fatalf("hash %v %v", hash, err)
	}
	var pe *pyint.Error
	if _, err := storeSystemPrompt(ctx, conn, []byte("x")); !errors.As(err, &pe) || pe.Msg != "'bytes' object has no attribute 'encode'" {
		t.Fatalf("bytes prompt %v", err)
	}
	if _, err := storeSystemPrompt(ctx, conn, "a\xed\xa0\x80"); !errors.As(err, &pe) || pe.Kind != "UnicodeEncodeError" || !strings.Contains(pe.Msg, `'\ud800' in position 1`) {
		t.Fatalf("surrogate prompt %v", err)
	}
	mustExecConn(t, conn, "DELETE FROM system_prompts")
	addTitledSession(t, conn, "a", 1, map[string]any{"system_prompt": "shared prompt"})
	addTitledSession(t, conn, "b", 2, map[string]any{"system_prompt": "shared prompt"})
	addTitledSession(t, conn, "c", 3, map[string]any{"system_prompt": "own prompt"})
	addTitledSession(t, conn, "d", 4, nil)
	if err := schema.dedupeLegacySystemPrompts(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if n := scalarCount(t, conn, "SELECT COUNT(*) FROM system_prompts"); n != 2 {
		t.Fatalf("content addressed prompts %d", n)
	}
	if n := scalarCount(t, conn, "SELECT COUNT(*) FROM sessions WHERE system_prompt IS NULL AND system_prompt_hash IS NOT NULL"); n != 3 {
		t.Fatalf("migrated sessions %d", n)
	}
	mustExecConn(t, conn, "ALTER TABLE sessions RENAME TO sessions_gone")
	if err := schema.dedupeLegacySystemPrompts(ctx, conn); err != nil {
		t.Fatalf("an OperationalError on the scan returns quietly: %v", err)
	}
}

func TestTriggerPredicatesAndFTS5Probe(t *testing.T) {
	ctx := context.Background()
	schema, host := schemaFixture(t, nil)
	if ok, err := schema.sqliteSupportsFTS5(ctx, host.conn); !ok || err != nil {
		t.Fatalf("fts5 available %v %v", ok, err)
	}
	if missing, err := ftsTriggersMissing(ctx, host.conn, nil); missing || err != nil {
		t.Fatal("empty half is never missing")
	}
	if missing, err := ftsTriggersMissing(ctx, host.conn, ftsBaseTriggers); missing || err != nil {
		t.Fatalf("base half present %v %v", missing, err)
	}
	if missing, _ := ftsTriggersMissing(ctx, host.conn, append(slices.Clone(ftsBaseTriggers), "nope")); !missing {
		t.Fatal("an absent trigger is missing")
	}
	for sql, want := range map[any]bool{
		nil: false, "": false,
		"CREATE TRIGGER x AFTER UPDATE ON messages BEGIN SELECT 1; END":            true,
		"create trigger x after\n  update\ton messages begin select 1; end":        true,
		"CREATE TRIGGER x AFTER UPDATE OF content ON messages BEGIN SELECT 1; END": false,
		"CREATE TRIGGER x AFTER INSERT ON messages BEGIN SELECT 1; END":            false,
	} {
		if got := ftsUpdateTriggerNeedsNarrowing(sql); got != want {
			t.Fatalf("narrowing %q = %v", sql, got)
		}
	}
	if misaligned, err := ftsIndexIsMisalignedSource(ctx, host.conn); misaligned || err != nil {
		t.Fatalf("current layout is aligned %v %v", misaligned, err)
	}
	if exists, _ := sqliteTableExists(ctx, host.conn, "messages"); !exists {
		t.Fatal("messages table exists")
	}
	if exists, _ := sqliteTableExists(ctx, host.conn, "messages_fts_src"); exists {
		t.Fatal("a view is not a table")
	}
}

func TestMigrateBroadFTSUpdateTriggers(t *testing.T) {
	ctx := context.Background()
	schema, host := schemaFixture(t, [][3]string{{"user", "hello world", ""}})
	if n, err := schema.migrateBroadFTSUpdateTriggers(ctx, host.conn); n != 0 || err != nil {
		t.Fatalf("narrowed triggers stay %d %v", n, err)
	}
	mustExecConn(t, host.conn, "DROP TRIGGER messages_fts_update")
	mustExecConn(t, host.conn, "CREATE TRIGGER messages_fts_update AFTER UPDATE ON messages BEGIN SELECT 1; END")
	n, err := schema.migrateBroadFTSUpdateTriggers(ctx, host.conn)
	if n != 1 || err != nil {
		t.Fatalf("one broad trigger migrated %d %v", n, err)
	}
	if sql := triggerSQL(t, host.conn, "messages_fts_update"); ftsUpdateTriggerNeedsNarrowing(sql) || sql == "" {
		t.Fatalf("recreated narrowed %q", sql)
	}
}

func TestExecuteDDLSkippingSettledTriggers(t *testing.T) {
	ctx := context.Background()
	_, host := schemaFixture(t, nil)
	tracking := &trackingConn{SQLiteConn: host.conn}
	if err := executeDDLSkippingSettledTriggers(ctx, tracking, FTSSQL); err != nil {
		t.Fatal(err)
	}
	if drops := tracking.matching(func(q string) bool { return strings.HasPrefix(strings.ToUpper(q), "DROP TRIGGER") }); len(drops) != 0 {
		t.Fatalf("settled triggers are not dropped: %q", drops)
	}
	mustExecConn(t, host.conn, "DROP TRIGGER messages_fts_insert")
	mustExecConn(t, host.conn, "CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN SELECT 1; END")
	body := strings.Split(FTSSQL, "CREATE TRIGGER IF NOT EXISTS messages_fts_insert")[1]
	ddl := "DROP TRIGGER IF EXISTS messages_fts_insert;\nCREATE TRIGGER IF NOT EXISTS messages_fts_insert" + body[:strings.Index(body, "END;")+4]
	tracking = &trackingConn{SQLiteConn: host.conn}
	if err := executeDDLSkippingSettledTriggers(ctx, tracking, ddl); err != nil {
		t.Fatal(err)
	}
	if got := tracking.matching(func(q string) bool { return strings.Contains(q, "TRIGGER") }); len(got) != 2 || !strings.HasPrefix(got[0], "DROP TRIGGER IF EXISTS messages_fts_insert") {
		t.Fatalf("a changed body is dropped and recreated: %q", got)
	}
	if strings.Contains(triggerSQL(t, host.conn, "messages_fts_insert"), "SELECT 1") {
		t.Fatal("trigger body rolled out")
	}
	var se *SQLiteError
	if err := executeDDLSkippingSettledTriggers(ctx, host.conn, "CREATE TABLE half (x"); !errors.As(err, &se) || se.Msg != "incomplete DDL statement" {
		t.Fatalf("incomplete %v", err)
	}
	mustExecConn(t, host.conn, "BEGIN")
	if err := executeDDLScriptTransactional(ctx, host.conn, "CREATE TABLE t1 (x);\nCREATE TABLE t2 (\n  y\n);\n"); err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, host.conn, "ROLLBACK")
	if exists, _ := sqliteTableExists(ctx, host.conn, "t2"); exists {
		t.Fatal("transactional DDL never commits on its own")
	}
	if err := executeDDLScriptTransactional(ctx, host.conn, "CREATE TABLE t3 (x);\nCREATE TABLE t4 (x"); !errors.As(err, &se) || se.Msg != "incomplete FTS DDL statement" {
		t.Fatalf("incomplete transactional %v", err)
	}
	if _, err := forEachCompleteStatement("SELECT 1;\x00", func(string) error { return nil }); err == nil {
		t.Fatal("embedded NUL is a ValueError")
	}
}

func TestRebuildFTSIndexesAndAdmission(t *testing.T) {
	ctx := context.Background()
	schema, host := schemaFixture(t, [][3]string{{"user", "zebra quokka", ""}, {"assistant", "plain reply", ""}})
	mustExecConn(t, host.conn, "INSERT INTO state_meta (key, value) VALUES ('fts_rebuild_high_water', '9'), ('fts_rebuild_progress', '3')")
	mustExecConn(t, host.conn, "DELETE FROM messages_fts")
	if err := rebuildFTSIndexes(ctx, host.conn, false, true); err != nil {
		t.Fatal(err)
	}
	if n := ftsMatchCount(t, host.conn, "messages_fts", "quokka"); n != 1 {
		t.Fatalf("rebuilt base index %d", n)
	}
	if n := scalarCount(t, host.conn, "SELECT COUNT(*) FROM state_meta WHERE key LIKE 'fts_rebuild_%'"); n != 0 {
		t.Fatalf("markers cleared %d", n)
	}
	legacy, legacyHost := legacySearch(t)
	mustExecConn(t, legacyHost.conn, "INSERT INTO state_meta (key, value) VALUES ('fts_rebuild_progress', '3')")
	if err := rebuildFTSIndexes(ctx, legacyHost.conn, true, false); err != nil {
		t.Fatal(err)
	}
	if n := ftsMatchCount(t, legacyHost.conn, "messages_fts", "quokka"); n != 1200 {
		t.Fatalf("legacy reinsert %d", n)
	}
	if n := scalarCount(t, legacyHost.conn, "SELECT COUNT(*) FROM state_meta WHERE key = 'fts_rebuild_progress'"); n != 1 {
		t.Fatal("legacy rebuild keeps markers")
	}
	_ = legacy

	prev := startupRebuildAdmission
	t.Cleanup(func() { startupRebuildAdmission = prev })
	startupRebuildAdmission = func(dbPath string, timeoutSeconds *float64, fn func(bool) error) error { return fn(false) }
	ran := false
	if err := schema.runAdmittedStartupRebuild(ctx, host.conn, func() error { ran = true; return nil }); err != nil || ran {
		t.Fatalf("refused rebuild never runs %v %v", ran, err)
	}
	if v, ok := metaValue(t, host.conn, FTSStaleKey); !ok || v != "1" {
		t.Fatal("stale breadcrumb persisted")
	}
	if missing, _ := ftsTriggersMissing(ctx, host.conn, ftsTriggers); !missing || len(liveTriggers(t, host.conn)) != 0 {
		t.Fatal("every FTS trigger detached")
	}
	if !schema.fts.ftsStale.Load() || schema.fts.ftsEnabled.Load() || schema.fts.trigramAvailable.Load() || schema.fts.ftsCJKAvailable.Load() {
		t.Fatal("availability flags cleared")
	}
	startupRebuildAdmission = prev
	sentinel := errors.New("rebuild failed")
	if err := schema.runAdmittedStartupRebuild(ctx, host.conn, func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("admitted rebuild error propagates %v", err)
	}
}

func TestMigrateTrigramAndMisalignedSource(t *testing.T) {
	ctx := context.Background()
	schema, host := schemaFixture(t, [][3]string{{"user", "cron noise words", ""}, {"user", "keep these words", ""}})
	ok, err := schema.migrateTrigramCronExclusion(ctx, host.conn)
	if !ok || err != nil {
		t.Fatalf("trigram view reinstalled %v %v", ok, err)
	}
	if n := ftsMatchCount(t, host.conn, "messages_fts_trigram", "words"); n != 2 {
		t.Fatalf("trigram rebuilt from the view %d", n)
	}
	mustExecConn(t, host.conn, "DROP TABLE messages_fts_trigram")
	if ok, err := schema.migrateTrigramCronExclusion(ctx, host.conn); !ok || err != nil {
		t.Fatalf("absent trigram index lets the version advance %v %v", ok, err)
	}
	if err := schema.migrateMisalignedFTSSource(ctx, host.conn, true); err != nil {
		t.Fatal(err)
	}
	if err := schema.migrateMisalignedFTSSource(ctx, host.conn, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range ftsBaseTriggers {
		mustExecConn(t, host.conn, "DROP TRIGGER IF EXISTS "+name)
	}
	mustExecConn(t, host.conn, "DROP TABLE messages_fts")
	mustExecConn(t, host.conn, "CREATE VIRTUAL TABLE messages_fts USING fts5(content,tool_name,tool_calls,content='messages',content_rowid='id')")
	mustExecConn(t, host.conn, "INSERT INTO state_meta (key, value) VALUES ('fts_tool_full_content_high_water', '1')")
	if misaligned, _ := ftsIndexIsMisalignedSource(ctx, host.conn); !misaligned {
		t.Fatal("raw messages source is misaligned")
	}
	if err := schema.migrateMisalignedFTSSource(ctx, host.conn, false); err != nil {
		t.Fatal(err)
	}
	if misaligned, _ := ftsIndexIsMisalignedSource(ctx, host.conn); misaligned {
		t.Fatal("realigned onto the source view")
	}
	if n := ftsMatchCount(t, host.conn, "messages_fts", "words"); n != 2 {
		t.Fatalf("rebuilt from the view %d", n)
	}
	if missing, _ := ftsTriggersMissing(ctx, host.conn, ftsBaseTriggers); missing {
		t.Fatal("base triggers restored")
	}
	if _, ok := metaValue(t, host.conn, "fts_tool_full_content_high_water"); ok {
		t.Fatal("retired tool high water dropped")
	}
}

func scalarCount(t *testing.T, conn SQLiteConn, query string) int64 {
	t.Helper()
	row, err := fetchOne(context.Background(), conn, query)
	if err != nil {
		t.Fatal(err)
	}
	return row[0].(int64)
}
