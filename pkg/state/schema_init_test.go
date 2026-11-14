package state

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type fakeSchemaHost struct {
	*fakeSearchHost
	search     *sessionSearch
	quarantine string
	reap       func(holders []Holder, minAge float64) []int
	reapCalls  int
}

func (h *fakeSchemaHost) quarantineReason() (string, bool) { return h.quarantine, h.quarantine != "" }

func (h *fakeSchemaHost) reapInactiveOrphanDesktopHolders(holders []Holder, minAgeSeconds float64) ([]int, error) {
	h.reapCalls++
	if h.reap == nil {
		return nil, nil
	}
	return h.reap(holders, minAgeSeconds), nil
}

func (h *fakeSchemaHost) ftsExternalIndexEmptyWithMessages(ctx context.Context, conn SQLiteConn) (bool, error) {
	return h.search.ftsExternalIndexEmptyWithMessages(ctx, conn)
}

// Schema mixin over an empty WAL file database with its own writer and reader
func freshSchema(t *testing.T) (*sessionSchema, *fakeSchemaHost) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "state.db")
	open := func() SQLiteConn {
		conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	writer := open()
	mustExecConn(t, writer, "PRAGMA journal_mode=WAL")
	search := &fakeSearchHost{fakeFTSHost: fakeFTSHost{path: path, conn: writer}, reader: open()}
	fts := newSessionFTSSetup(search)
	host := &fakeSchemaHost{fakeSearchHost: search, search: newSessionSearch(search, fts)}
	return newSessionSchema(host, fts), host
}

func scalarOf(t *testing.T, conn SQLiteConn, query string, args ...any) any {
	t.Helper()
	row, err := fetchOne(context.Background(), conn, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		return nil
	}
	return row[0]
}

func metaAny(t *testing.T, conn SQLiteConn, key string) any {
	t.Helper()
	return scalarOf(t, conn, "SELECT value FROM state_meta WHERE key = ?", key)
}

type interleavingConn struct {
	SQLiteConn
	after func(query string)
}

func (c *interleavingConn) Raw(f func(driverConn any) error) error {
	return c.SQLiteConn.(rawConner).Raw(f)
}

func (c *interleavingConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := c.SQLiteConn.ExecContext(ctx, query, args...)
	c.after(query)
	return res, err
}

func rebuildDB(t *testing.T) (SQLiteConn, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	mustExecConn(t, conn, "PRAGMA journal_mode=WAL")
	mustExecConn(t, conn, "CREATE TABLE t (a INTEGER PRIMARY KEY, b TEXT)")
	mustExecConn(t, conn, "INSERT INTO t VALUES (1, 'x'), (2, 'y')")
	return conn, path
}

const rebuildDDL, rebuildCopy = "CREATE TABLE t (a INTEGER, b TEXT, c INTEGER NOT NULL DEFAULT 0)", "INSERT INTO t (a, b) SELECT a, b FROM t_legacy"

func TestSiblingOpenerBetweenRenameAndCreateCannotStrandRows(t *testing.T) {
	ctx := context.Background()
	conn, path := rebuildDB(t)
	sibling, err := ConnectSQLite(path, SQLiteConfig{Timeout: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	defer sibling.Close()
	seen := ""
	wrapped := &interleavingConn{SQLiteConn: conn, after: func(query string) {
		if !strings.HasPrefix(query, "ALTER TABLE t RENAME") {
			return
		}
		_, err := sibling.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS t (a INTEGER, b TEXT, c INTEGER)")
		if err == nil {
			_, err = sibling.ExecContext(ctx, "INSERT INTO t (a, b) VALUES (3, 'z')")
		}
		switch {
		case err == nil:
			seen = "applied"
		case isOperationalError(err) && (strings.Contains(errText(err), "locked") || strings.Contains(errText(err), "busy")):
			seen = "busy"
		default:
			seen = errText(err)
		}
	}}
	if err := rebuildTable(ctx, wrapped, "t", "t_legacy", rebuildDDL, rebuildCopy, nil); err != nil {
		t.Fatal(err)
	}
	if seen != "busy" {
		t.Fatalf("sibling %q", seen)
	}
	if auto, _ := sqliteAutocommit(conn); !auto {
		t.Fatal("transaction left open")
	}
	names, _ := queryStrings(ctx, conn, "SELECT name FROM sqlite_master WHERE type='table'")
	if !slices.Equal(names, []string{"t"}) {
		t.Fatalf("tables %v", names)
	}
	if n := scalarOf(t, conn, "SELECT COUNT(*) FROM t"); n != int64(2) {
		t.Fatalf("rows %v", n)
	}
	if c := scalarOf(t, conn, "SELECT c FROM t WHERE a = 1"); c != int64(0) {
		t.Fatalf("new shape %v", c)
	}
}

func TestRebuildFailureRollsBackRename(t *testing.T) {
	ctx := context.Background()
	conn, _ := rebuildDB(t)
	err := rebuildTable(ctx, conn, "t", "t_legacy", rebuildDDL, "INSERT INTO nope SELECT * FROM t_legacy", nil)
	if !isOperationalError(err) {
		t.Fatalf("err %v", err)
	}
	if auto, _ := sqliteAutocommit(conn); !auto {
		t.Fatal("transaction left open")
	}
	if n := scalarOf(t, conn, "SELECT COUNT(*) FROM t"); n != int64(2) {
		t.Fatalf("rows %v", n)
	}
	if scalarOf(t, conn, "SELECT name FROM sqlite_master WHERE name = 't_legacy'") != nil {
		t.Fatal("legacy copy survived")
	}
	mustExecConn(t, conn, "BEGIN IMMEDIATE")
	if err := rebuildTable(ctx, conn, "t", "t_legacy", rebuildDDL, rebuildCopy, []string{"CREATE INDEX ix_t_b ON t(b)"}); err != nil {
		t.Fatal(err)
	}
	if auto, _ := sqliteAutocommit(conn); auto {
		t.Fatal("joined transaction was ended by the rebuild")
	}
	mustExecConn(t, conn, "COMMIT")
	if scalarOf(t, conn, "SELECT name FROM sqlite_master WHERE name = 'ix_t_b'") == nil {
		t.Fatal("index not recreated")
	}
}

func TestLivePKColumnsOrderAndAbsence(t *testing.T) {
	ctx := context.Background()
	conn, _ := rebuildDB(t)
	mustExecConn(t, conn, "CREATE TABLE k (x TEXT, y TEXT, z TEXT, PRIMARY KEY (z, x))")
	mustExecConn(t, conn, "CREATE TABLE nokey (x TEXT)")
	for table, want := range map[string][]string{"k": {"z", "x"}, "t": {"a"}, "nokey": {}, "missing": nil} {
		got, err := livePKColumns(ctx, conn, table)
		if err != nil || !slices.Equal(got, want) || (got == nil) != (want == nil) {
			t.Fatalf("%s %#v %v", table, got, err)
		}
	}
}

func TestApplyDeclaredSchemaAddsMissingColumnsAndCommits(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	conn, _ := rebuildDB(t)
	mustExecConn(t, conn, "CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, started_at REAL NOT NULL)")
	mustExecConn(t, conn, "BEGIN")
	if err := ReconcileStateSchema(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if auto, _ := sqliteAutocommit(conn); !auto {
		t.Fatal("reconcile left the transaction open")
	}
	cols, _ := queryStrings(ctx, conn, "SELECT name FROM pragma_table_info('sessions')")
	for _, want := range []string{"title", "model_config", "message_count", "parent_session_id"} {
		if !slices.Contains(cols, want) {
			t.Fatalf("sessions lacks %s: %v", want, cols)
		}
	}
	if scalarOf(t, conn, "SELECT name FROM sqlite_master WHERE name = 'messages'") == nil {
		t.Fatal("declared DDL not applied")
	}
	if err := ApplyDeclaredSchema(ctx, conn); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileColumnsLoggingAndLockRaise(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	conn, path := rebuildDB(t)
	mustExecConn(t, conn, "CREATE TABLE sessions (id TEXT PRIMARY KEY)")
	locker, err := ConnectSQLite(path, SQLiteConfig{Timeout: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	mustExecConn(t, locker, "BEGIN IMMEDIATE")
	err = (&sessionSchema{}).reconcileColumns(ctx, conn)
	if !IsSQLiteLockError(err) {
		t.Fatalf("lock contention swallowed: %v", err)
	}
	mustExecConn(t, locker, "ROLLBACK")
	buf := captureSlog(t)
	dup := &interleavingConn{SQLiteConn: conn, after: func(string) {}}
	racer := &racingAddConn{interleavingConn: dup, sibling: locker}
	if err := (&sessionSchema{}).reconcileColumns(ctx, racer); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "reconcile sessions.source: duplicate column name: source") {
		t.Fatalf("duplicate race not logged: %s", buf.String())
	}
}

type racingAddConn struct {
	*interleavingConn
	sibling SQLiteConn
	raced   bool
}

func (c *racingAddConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if !c.raced && strings.Contains(query, `ADD COLUMN "source"`) {
		c.raced = true
		if _, err := c.sibling.ExecContext(ctx, `ALTER TABLE "sessions" ADD COLUMN "source" TEXT`); err != nil {
			return nil, err
		}
	}
	return c.interleavingConn.ExecContext(ctx, query, args...)
}

func TestHealGatewayRoutingAndUsagePrimaryKeys(t *testing.T) {
	ctx := context.Background()
	schema, host := freshSchema(t)
	conn := host.conn
	mustExecConn(t, conn, "CREATE TABLE gateway_routing (session_key TEXT PRIMARY KEY, entry_json TEXT NOT NULL, updated_at REAL NOT NULL)")
	mustExecConn(t, conn, "INSERT INTO gateway_routing VALUES ('k1', '{}', 1.0), ('k2', '{\"a\":1}', 2.0)")
	mustExecConn(t, conn, "CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, started_at REAL NOT NULL)")
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at) VALUES ('s1', 'cli', 1.0)")
	mustExecConn(t, conn, `CREATE TABLE session_model_usage (session_id TEXT NOT NULL REFERENCES sessions(id), model TEXT NOT NULL,
		billing_provider TEXT NOT NULL DEFAULT '', billing_base_url TEXT NOT NULL DEFAULT '', billing_mode TEXT NOT NULL DEFAULT '',
		api_call_count INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		estimated_cost_usd REAL NOT NULL DEFAULT 0, actual_cost_usd REAL NOT NULL DEFAULT 0, cost_status TEXT, cost_source TEXT,
		first_seen REAL, last_seen REAL, PRIMARY KEY (session_id, model, billing_provider, billing_base_url, billing_mode))`)
	mustExecConn(t, conn, "INSERT INTO session_model_usage (session_id, model, input_tokens) VALUES ('s1', 'm', 5), ('ghost', 'm', 7)")
	buf := captureSlog(t)
	if err := schema.reconcileColumns(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := schema.healGatewayRoutingPK(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := schema.healSessionModelUsagePK(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if pk, _ := livePKColumns(ctx, conn, "gateway_routing"); !slices.Equal(pk, []string{"scope", "session_key"}) {
		t.Fatalf("routing pk %v", pk)
	}
	if n := scalarOf(t, conn, "SELECT COUNT(*) FROM gateway_routing WHERE scope = ''"); n != int64(2) {
		t.Fatalf("routing rows %v", n)
	}
	pk, _ := livePKColumns(ctx, conn, "session_model_usage")
	if !slices.Contains(pk, "task") {
		t.Fatalf("usage pk %v", pk)
	}
	if n := scalarOf(t, conn, "SELECT SUM(input_tokens) FROM session_model_usage"); n != int64(12) {
		t.Fatalf("orphan usage row dropped by the FK window: %v", n)
	}
	if fk := scalarOf(t, conn, "PRAGMA foreign_keys"); fk != int64(1) {
		t.Fatalf("foreign keys %v", fk)
	}
	text := buf.String()
	for _, want := range []string{"gateway_routing has legacy primary key ['session_key']",
		"session_model_usage has legacy primary key ['billing_base_url', 'billing_mode', 'billing_provider', 'model', 'session_id'] (missing task)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("log lacks %q: %s", want, text)
		}
	}
	if err := schema.healGatewayRoutingPK(ctx, conn); err != nil {
		t.Fatal(err)
	}
}

func TestInitSchemaFreshStoreStampsEverything(t *testing.T) {
	ctx := context.Background()
	schema, host := freshSchema(t)
	var leases []string
	SetStartupProgressLease(func(expectedS float64, phase string) { leases = append(leases, phase) })
	t.Cleanup(func() { SetStartupProgressLease(nil) })
	if err := schema.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	conn := host.conn
	if v := scalarOf(t, conn, "SELECT version FROM schema_version"); v != int64(SchemaVersion) {
		t.Fatalf("version %v", v)
	}
	for _, key := range []string{"store_instance_id", "store_created_at_utc"} {
		if v, _ := metaAny(t, conn, key).(string); v == "" {
			t.Fatalf("%s missing", key)
		}
	}
	if created, _ := metaAny(t, conn, "store_created_at_utc").(string); !strings.HasSuffix(created, "+00:00") {
		t.Fatalf("created %q", created)
	}
	if metaAny(t, conn, messageUIDBackfillDone) != "1" || metaAny(t, conn, "fts_storage_version") != "3" {
		t.Fatal("fresh store markers missing")
	}
	if !schema.fts.ftsEnabled.Load() || !schema.fts.trigramAvailable.Load() || schema.fts.ftsStale.Load() {
		t.Fatal("fts flags")
	}
	if scalarOf(t, conn, "SELECT name FROM sqlite_master WHERE name = 'idx_sessions_title_unique'") == nil {
		t.Fatal("unique title index missing")
	}
	if !slices.Equal(leases, []string{"state_db_init_schema"}) {
		t.Fatalf("leases %v", leases)
	}
	if auto, _ := sqliteAutocommit(conn); !auto {
		t.Fatal("init left a transaction open")
	}
	if err := schema.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(leases, []string{"state_db_init_schema", "state_db_init_schema", "state_db_data_migrations"}) {
		t.Fatalf("reopen leases %v", leases)
	}
}

func TestInitSchemaRunsDataMigrationsOnAnOldStore(t *testing.T) {
	ctx := context.Background()
	schema, host := freshSchema(t)
	conn := host.conn
	if err := schema.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at, input_tokens, model) VALUES ('p', 'cli', 1.0, 10, 'gpt')")
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at, parent_session_id) VALUES ('c', 'cli', 2.0, 'p')")
	mustExecConn(t, conn, "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('p', 'user', 'hi', 1.0)")
	mustExecConn(t, conn, "UPDATE messages SET message_uid = NULL")
	mustExecConn(t, conn, "UPDATE schema_version SET version = 15")
	mustExecConn(t, conn, "DELETE FROM state_meta WHERE key = 'message_uid_backfill'")
	if err := os.MkdirAll(filepath.Join(os.Getenv("ICEBATH_HOME"), "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	sessionsJSON := `{"_meta": {}, "agent:main": {"session_id": "p", "display_name": "Main", "memory_flushed": true,
		"origin": {"chat_id": "42", "thread_id": "7"}, "chat_type": "dm"}, "bad": {"session_id": ""}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "sessions", "sessions.json"), []byte(sessionsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := schema.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if v := scalarOf(t, conn, "SELECT version FROM schema_version"); v != int64(SchemaVersion) {
		t.Fatalf("version %v", v)
	}
	if v := scalarOf(t, conn, "SELECT json_extract(model_config, '$._delegate_from') FROM sessions WHERE id = 'c'"); v != "p" {
		t.Fatalf("delegate tag %v", v)
	}
	if v := scalarOf(t, conn, "SELECT input_tokens FROM session_model_usage WHERE session_id = 'p'"); v != int64(10) {
		t.Fatalf("v20 seed %v", v)
	}
	if v := scalarOf(t, conn, "SELECT COUNT(*) FROM messages WHERE message_uid IS NULL"); v != int64(0) {
		t.Fatalf("uid backfill %v", v)
	}
	row, _ := fetchOne(ctx, conn, "SELECT session_key, chat_id, chat_type, thread_id, display_name, origin_json, expiry_finalized FROM sessions WHERE id = 'p'")
	want := []any{"agent:main", "42", "dm", "7", "Main", `{"chat_id": "42", "thread_id": "7"}`, int64(1)}
	if !slices.Equal(row, want) {
		t.Fatalf("gateway backfill %#v", row)
	}
}

func TestMessageUIDBackfillResumesAcrossOpens(t *testing.T) {
	ctx := context.Background()
	schema, host := freshSchema(t)
	conn := host.conn
	if err := schema.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at) VALUES ('s', 'cli', 1.0)")
	mustExecConn(t, conn, "INSERT INTO messages (id, session_id, role, content, timestamp) VALUES (1, 's', 'user', 'a', 1.0), (4500, 's', 'user', 'b', 2.0)")
	mustExecConn(t, conn, "UPDATE messages SET message_uid = NULL")
	mustExecConn(t, conn, "DELETE FROM state_meta WHERE key = 'message_uid_backfill'")
	clock := 0.0
	pinVar(t, &monotonicSeconds, func() float64 { clock += 0.6; return clock })
	if err := schema.advanceMessageUIDBackfill(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if v := metaAny(t, conn, messageUIDBackfillCursor); v != "4000" {
		t.Fatalf("cursor %v", v)
	}
	if v := scalarOf(t, conn, "SELECT COUNT(*) FROM messages WHERE message_uid IS NULL"); v != int64(1) {
		t.Fatalf("first slice %v", v)
	}
	if err := schema.advanceMessageUIDBackfill(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if metaAny(t, conn, messageUIDBackfillCursor) != nil || metaAny(t, conn, messageUIDBackfillDone) != "1" {
		t.Fatal("backfill not finished")
	}
	mustExecConn(t, conn, "DELETE FROM state_meta WHERE key = 'message_uid_backfill'")
	mustExecConn(t, conn, "INSERT INTO state_meta (key, value) VALUES ('message_uid_backfill_id', 'abc')")
	if err := schema.advanceMessageUIDBackfill(ctx, conn); err == nil || !strings.Contains(err.Error(), "invalid literal for int()") {
		t.Fatalf("bad cursor %v", err)
	}
}

func TestEnsureUniqueTitleIndexRepairsDuplicates(t *testing.T) {
	ctx := context.Background()
	schema, host := freshSchema(t)
	conn := host.conn
	if err := schema.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, conn, "DROP INDEX idx_sessions_title_unique")
	for _, r := range [][4]any{{"u1", "Plan", nil, 1.0}, {"u2", "Plan", "user", 2.0}, {"a1", "Plan", "llm", 3.0},
		{"d1", "Notes", "derived", 1.0}, {"l1", "Notes", "llm", 2.0}, {"l2", "Notes", "llm", 3.0}} {
		if _, err := conn.ExecContext(ctx, "INSERT INTO sessions (id, source, started_at, title, title_source) VALUES (?, 'cli', ?, ?, ?)",
			r[0], r[3], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
	buf := captureSlog(t)
	if err := schema.ensureUniqueTitleIndex(ctx, conn); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, id := range []string{"u1", "u2", "a1", "d1", "l1", "l2"} {
		got[id] = scalarOf(t, conn, "SELECT title FROM sessions WHERE id = ?", id)
	}
	want := map[string]any{"u1": "Plan", "u2": "Plan #2", "a1": nil, "d1": nil, "l1": nil, "l2": "Notes"}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("titles %v", got)
		}
	}
	if scalarOf(t, conn, "SELECT name FROM sqlite_master WHERE name = 'idx_sessions_title_unique'") == nil {
		t.Fatal("index not created")
	}
	for _, line := range []string{"Renamed duplicate user session title 'Plan' to 'Plan #2'", "Cleared duplicate llm session title 'Plan'",
		"Cleared duplicate derived session title 'Notes'"} {
		if !strings.Contains(buf.String(), line) {
			t.Fatalf("log lacks %q", line)
		}
	}
}

func TestDeferStaleFTSForHoldersEscalatesAndGoesFutile(t *testing.T) {
	ctx := context.Background()
	schema, host := freshSchema(t)
	conn := host.conn
	if err := schema.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := 10_000.0
	pinVar(t, &schemaWallTime, func() float64 { return now })
	pinVar(t, &readProcArgv, func(pid int) ([]string, bool) { return []string{"icebath", "gateway", strings.Repeat("x", 200)}, true })
	holders := []Holder{{PID: 42, Target: "/x/state.db-wal"}, {PID: 42, Target: "/x/state.db"}, {PID: -1, Target: "?"}}
	buf := captureSlog(t)
	record := func() *pyjson.Object {
		v, _ := metaAny(t, conn, FTSRebuildDeferralKey).(string)
		obj, _ := pyjson.SafeLoads(v, nil).(*pyjson.Object)
		return obj
	}
	deferred, err := schema.deferStaleFTSForHolders(ctx, conn, holders)
	if err != nil || !deferred {
		t.Fatal(deferred, err)
	}
	text, _ := metaAny(t, conn, FTSRebuildDeferralKey).(string)
	if text != `{"attempts": 1, "first_seen": 10000.0, "futile": false, "holder_pids": [42], "holders_attempts": 1, "holders_since": 10000.0, "last_seen": 10000.0}` {
		t.Fatalf("record %s", text)
	}
	if !slices.Equal(schema.ftsDeferredHolderPIDs, []int{42}) {
		t.Fatalf("pids %v", schema.ftsDeferredHolderPIDs)
	}
	now += 30
	schema.deferStaleFTSForHolders(ctx, conn, holders)
	now += 40
	host.reap = func(h []Holder, minAge float64) []int { return []int{42} }
	host.holderScan = func() ([]Holder, error) { return []Holder{{PID: 7, Target: "/x/state.db"}}, nil }
	schema.deferStaleFTSForHolders(ctx, conn, holders)
	r := record()
	if a, _ := r.Get("attempts"); a != int64(3) || host.reapCalls != 1 {
		t.Fatalf("escalation %v %d", a, host.reapCalls)
	}
	if p, _ := r.Get("holder_pids"); len(p.([]any)) != 1 || p.([]any)[0] != int64(7) {
		t.Fatalf("rechecked holders %v", p)
	}
	if !strings.Contains(buf.String(), "Reaped inactive orphan Desktop backend(s) [42] after 3") {
		t.Fatalf("reap log %s", buf.String())
	}
	host.reap = nil
	for i := 0; i < 10; i++ {
		now += 200
		schema.deferStaleFTSForHolders(ctx, conn, []Holder{{PID: 7, Target: "/x/state.db"}})
	}
	r = record()
	if f, _ := r.Get("futile"); f != true {
		t.Fatalf("futile %v", r)
	}
	if !strings.Contains(buf.String(), "pid 7: icebath gateway xxx") || !strings.Contains(buf.String(), "within 60s of the holder leaving") {
		t.Fatalf("futile log %s", buf.String())
	}
	mustExecConn(t, conn, "UPDATE state_meta SET value = '{\"attempts\": \"x\", \"first_seen\": 1}' WHERE key = 'fts_rebuild_deferral'")
	schema.deferStaleFTSForHolders(ctx, conn, holders)
	if a, _ := record().Get("attempts"); a != int64(1) {
		t.Fatalf("reset %v", a)
	}
	mustExecConn(t, conn, "UPDATE state_meta SET value = '{\"attempts\": Infinity}' WHERE key = 'fts_rebuild_deferral'")
	if _, err := schema.deferStaleFTSForHolders(ctx, conn, holders); err == nil || !strings.Contains(err.Error(), "infinity") {
		t.Fatalf("overflow %v", err)
	}
	mustExecConn(t, conn, "DELETE FROM state_meta WHERE key = 'fts_rebuild_deferral'")
	if deferred, err := schema.deferStaleFTSForHolders(ctx, conn, nil); deferred || err != nil || schema.ftsDeferredHolderPIDs != nil {
		t.Fatal("no holders still deferred")
	}
	if got := holderCmdline(9); len([]rune(got)) != 120 {
		t.Fatalf("cmdline %q", got)
	}
	pinVar(t, &readProcArgv, func(pid int) ([]string, bool) { return nil, false })
	if got := holderCmdline(9); got != "<cmdline unavailable>" {
		t.Fatal(got)
	}
}

// Store with a stale FTS breadcrumb and detached triggers, as a prior corruption detach leaves it
func staleStore(t *testing.T) (*sessionSchema, *fakeSchemaHost) {
	t.Helper()
	ctx := context.Background()
	schema, host := freshSchema(t)
	if err := schema.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	conn := host.conn
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at) VALUES ('s', 'cli', 1.0)")
	mustExecConn(t, conn, "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s', 'user', 'quokka stew', 1.0)")
	if err := schema.fts.dropAllFTSTriggers(ctx, conn); err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, conn, "DELETE FROM messages_fts")
	mustExecConn(t, conn, "INSERT INTO state_meta (key, value) VALUES ('fts_stale', '1'), ('fts_rebuild_deferral', '{}')")
	schema.fts.ftsStale.Store(true)
	schema.fts.ftsEnabled.Store(false)
	return schema, host
}

func TestRecoverStaleFTSRebuildsOrDefers(t *testing.T) {
	ctx := context.Background()
	schema, host := staleStore(t)
	conn := host.conn
	host.holderScan = func() ([]Holder, error) { return []Holder{{PID: 99, Target: "/x"}}, nil }
	if ok, err := schema.recoverStaleFTS(ctx, conn, false, nil); ok || err != nil {
		t.Fatal(ok, err)
	}
	if metaAny(t, conn, FTSStaleKey) != "1" {
		t.Fatal("breadcrumb cleared while deferred")
	}
	host.holderScan = nil
	if ok, err := schema.recoverStaleFTS(ctx, conn, false, nil); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if metaAny(t, conn, FTSStaleKey) != nil || metaAny(t, conn, FTSRebuildDeferralKey) != nil {
		t.Fatal("breadcrumbs survived")
	}
	if n := scalarOf(t, conn, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'quokka'"); n != int64(1) {
		t.Fatalf("rebuilt index %v", n)
	}
	if missing, _ := ftsTriggersMissing(ctx, conn, ftsTriggers); missing {
		t.Fatal("triggers not restored")
	}
	if schema.fts.ftsStale.Load() || !schema.fts.ftsEnabled.Load() || !schema.fts.trigramAvailable.Load() {
		t.Fatal("flags")
	}
}

func TestRecoverStaleFTSLockedFailureDetaches(t *testing.T) {
	ctx := context.Background()
	schema, host := staleStore(t)
	conn := host.conn
	mustExecConn(t, conn, "CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN SELECT 1; END")
	mustExecConn(t, conn, "DROP VIEW messages_fts_trigram_src")
	mustExecConn(t, conn, "CREATE TABLE messages_fts_trigram_src (x)")
	buf := captureSlog(t)
	if ok, err := schema.recoverStaleFTSLocked(ctx, conn, false); ok || err != nil {
		t.Fatal(ok, err)
	}
	if !strings.Contains(buf.String(), "Automatic rebuild of stale FTS indexes failed") {
		t.Fatal(buf.String())
	}
	if triggerSQL(t, conn, "messages_fts_insert") != "" {
		t.Fatal("triggers left attached")
	}
	if auto, _ := sqliteAutocommit(conn); !auto {
		t.Fatal("transaction left open")
	}
	if ok, err := schema.trigramTokenizerAvailable(ctx, conn); !ok || err != nil {
		t.Fatal(ok, err)
	}
}

func TestRetryDeferredFTSRecoveryBacksOff(t *testing.T) {
	ctx := context.Background()
	schema, host := staleStore(t)
	clock := 1000.0
	pinVar(t, &monotonicSeconds, func() float64 { return clock })
	holders := []Holder{{PID: 5, Target: "/x"}}
	host.holderScan = func() ([]Holder, error) { return holders, nil }
	if ok, err := schema.RetryDeferredFTSRecovery(ctx); ok || err != nil {
		t.Fatal(ok, err)
	}
	if schema.ftsStaleRetryAfter != 1060 || schema.ftsStaleRetryInterval != 120 {
		t.Fatal(schema.ftsStaleRetryAfter, schema.ftsStaleRetryInterval)
	}
	clock = 1010
	if ok, _ := schema.RetryDeferredFTSRecovery(ctx); ok || schema.ftsStaleRetryAfter != 1060 {
		t.Fatal("backoff skipped with the same holders")
	}
	holders = nil
	if ok, err := schema.RetryDeferredFTSRecovery(ctx); !ok || err != nil {
		t.Fatal("changed holder set did not retry", ok, err)
	}
	if schema.ftsStaleRetryInterval != 0 || schema.fts.ftsStale.Load() {
		t.Fatal("recovery state")
	}
	if ok, _ := schema.RetryDeferredFTSRecovery(ctx); ok {
		t.Fatal("not stale yet retried")
	}
	schema.fts.ftsStale.Store(true)
	host.quarantine = "a replaced state.db file"
	schema.ftsStaleRetryAfter = 5
	if ok, _ := schema.RetryDeferredFTSRecovery(ctx); ok || schema.ftsStaleRetryAfter != 0 {
		t.Fatal("quarantine")
	}
	host.quarantine = ""
	host.ro = true
	if ok, _ := schema.RetryDeferredFTSRecovery(ctx); ok {
		t.Fatal("read only")
	}
}

func TestInitFTSRecoversStaleStoreOnOpen(t *testing.T) {
	ctx := context.Background()
	schema, host := staleStore(t)
	conn := host.conn
	if err := schema.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if schema.fts.ftsStale.Load() || !schema.fts.ftsEnabled.Load() || metaAny(t, conn, "fts_storage_version") != "3" {
		t.Fatal("stale store not recovered on open")
	}
	mustExecConn(t, conn, "DROP TRIGGER messages_fts_insert")
	mustExecConn(t, conn, "DELETE FROM messages_fts")
	if err := schema.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if n := scalarOf(t, conn, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'quokka'"); n != int64(1) {
		t.Fatalf("missing base trigger not rebuilt %v", n)
	}
}

func TestLegacyInlineReinsertSQLPinsWhitespace(t *testing.T) {
	want := "\n                    DELETE FROM messages_fts_trigram;\n                    INSERT INTO messages_fts_trigram(rowid, content)\n" +
		"                    SELECT id,\n                           COALESCE(content, '') || ' ' ||\n" +
		"                           COALESCE(tool_name, '') || ' ' ||\n                           COALESCE(tool_calls, '')\n" +
		"                    FROM messages;\n                "
	if got := legacyInlineReinsertSQL("messages_fts_trigram", 20, true); got != want {
		t.Fatalf("%q", got)
	}
	if got := legacyInlineReinsertSQL("x", 2, false); !strings.HasPrefix(got, "\n  INSERT INTO x") || !strings.HasSuffix(got, "FROM messages;\n") {
		t.Fatalf("%q", got)
	}
	if !strings.HasPrefix(sessionModelUsageV22DDL, "CREATE TABLE session_model_usage (\n"+strings.Repeat(" ", 35)+"session_id TEXT") ||
		!strings.HasSuffix(sessionModelUsageV22DDL, "\n"+strings.Repeat(" ", 31)+")") {
		t.Fatal(sessionModelUsageV22DDL)
	}
}
