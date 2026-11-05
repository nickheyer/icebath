package state

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	sqlite3 "modernc.org/sqlite/lib"
)

// SessionDB stand in that records the FTS lifecycle calls into hermes_state.py members
type fakeFTSHost struct {
	path        string
	mu          sync.Mutex
	conn        SQLiteConn
	reopenConn  SQLiteConn
	corruptErr  error
	replacedErr error
	reopenErr   error
	reopens     []string
	sleep       func(deadline, patienceS float64) bool
	patience    float64
}

func (h *fakeFTSHost) dbPath() string                      { return h.path }
func (h *fakeFTSHost) raiseIfDBCorrupt(storage bool) error { return h.corruptErr }
func (h *fakeFTSHost) raiseIfDBReplaced() error            { return h.replacedErr }
func (h *fakeFTSHost) writerLock() sync.Locker             { return &h.mu }
func (h *fakeFTSHost) writerConn() SQLiteConn              { return h.conn }
func (h *fakeFTSHost) writePatienceS() float64             { return h.patience }

func (h *fakeFTSHost) reopenAfterCloseLocked(context string) error {
	h.reopens = append(h.reopens, context)
	if h.reopenErr != nil {
		return h.reopenErr
	}
	h.conn = h.reopenConn
	return nil
}

func (h *fakeFTSHost) sleepBeforeWriteRetry(deadline, patienceS float64) bool {
	if h.sleep == nil {
		return false
	}
	return h.sleep(deadline, patienceS)
}

func metaValue(t *testing.T, conn SQLiteConn, key string) (string, bool) {
	t.Helper()
	row, err := fetchOne(context.Background(), conn, "SELECT value FROM state_meta WHERE key = ?", key)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		return "", false
	}
	return row[0].(string), true
}

func liveTriggers(t *testing.T, conn SQLiteConn) []string {
	t.Helper()
	names, err := queryStrings(context.Background(), conn, "SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'messages_fts%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func tableNames(t *testing.T, conn SQLiteConn, like string) []string {
	t.Helper()
	names, err := queryStrings(context.Background(), conn, "SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE ? ORDER BY name", like)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestDropOrphanFTSShadowTablesRepairsOnlyDeadFamilies(t *testing.T) {
	ctx := context.Background()
	conn, err := ConnectSQLite(":memory:", SQLiteConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, name := range []string{"messages_fts_data", "messages_fts_config", "messages_fts_trigram_data", "messages_fts_extra"} {
		mustExecConn(t, conn, "CREATE TABLE "+name+" (x)")
	}
	logs := captureSlog(t)
	repaired, err := dropOrphanFTSShadowTables(ctx, conn, []string{"messages_fts", "messages_fts_cjk"})
	if err != nil || !slices.Equal(repaired, []string{"messages_fts"}) {
		t.Fatalf("repaired = %v %v", repaired, err)
	}
	if got := tableNames(t, conn, "messages_fts%"); !slices.Equal(got, []string{"messages_fts_extra", "messages_fts_trigram_data"}) {
		t.Fatalf("exact shadow names only, left %v", got)
	}
	if !strings.Contains(logs.String(), "Dropped orphan FTS5 shadow tables of messages_fts (messages_fts_data, messages_fts_config)") {
		t.Fatalf("warning = %s", logs.String())
	}
	mustExecConn(t, conn, "CREATE VIRTUAL TABLE messages_fts USING fts5(content)")
	mustExecConn(t, conn, "CREATE VIRTUAL TABLE messages_fts_trigram_x USING fts5(content)")
	repaired, err = dropOrphanFTSShadowTables(ctx, conn, []string{"messages_fts"})
	if err != nil || len(repaired) != 0 {
		t.Fatalf("a live family is left alone: %v %v", repaired, err)
	}
	if got := tableNames(t, conn, "messages_fts_data"); len(got) != 1 {
		t.Fatalf("live shadow tables kept: %v", got)
	}
}

func TestFTSCapabilityAndLayoutPredicates(t *testing.T) {
	ctx := context.Background()
	for text, want := range map[string][2]bool{
		"no such module: fts5":             {true, false},
		"No Such Module: FTS5":             {true, false},
		"no such module: rtree":            {false, false},
		"no such tokenizer: trigram":       {true, true},
		"no such tokenizer: cjk_unicode61": {true, true},
		"no such tokenizer: porter2":       {false, false},
	} {
		err := &SQLiteError{Class: SQLiteOperationalError, Msg: text}
		if got := [2]bool{isFTS5UnavailableError(err), isTrigramUnavailableError(err)}; got != want {
			t.Errorf("%q = %v, want %v", text, got, want)
		}
	}
	conn, _ := schemaConn(t)
	for _, check := range []func(context.Context, SQLiteConn) (bool, error){dbHasLegacyInlineFTS, dbHasTrigramToolCallsProjection, dbNeedsFTSStorageUpgrade} {
		if got, err := check(ctx, conn); err != nil || got {
			t.Fatalf("the shipping layout reports work: %v %v", got, err)
		}
	}
	bare, err := ConnectSQLite(":memory:", SQLiteConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close()
	if got, err := dbHasLegacyInlineFTS(ctx, bare); err != nil || got {
		t.Fatalf("absent messages_fts is not legacy: %v %v", got, err)
	}
	mustExecConn(t, bare, "CREATE VIRTUAL TABLE messages_fts USING fts5(content)")
	mustExecConn(t, bare, "CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content, Tool_Calls, tokenize='trigram')")
	if got, err := dbHasLegacyInlineFTS(ctx, bare); err != nil || !got {
		t.Fatalf("single column messages_fts is legacy: %v %v", got, err)
	}
	if got, err := dbHasTrigramToolCallsProjection(ctx, bare); err != nil || !got {
		t.Fatalf("trigram tool_calls projection: %v %v", got, err)
	}
	legacy := declaredSchemaDB(t, true)
	lconn, err := legacy.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lconn.Close()
	if got, err := dbNeedsFTSStorageUpgrade(ctx, lconn); err != nil || !got {
		t.Fatalf("the legacy layout needs optimize: %v %v", got, err)
	}
	f := newSessionFTSSetup(&fakeFTSHost{path: "/x/state.db"})
	for rows, want := range map[[3]float64]int{
		{500, 0.25, 0.25}:   500,
		{500, 0.01, 0.25}:   1000,
		{500, 10, 0.25}:     250,
		{60, 10, 0.25}:      50,
		{90000, 0, 0.25}:    100000,
		{501, 0.3, 0.25}:    417,
		{500, 0.05, 0.05}:   500,
		{100, 0.00001, 0.1}: 200,
	} {
		if got := f.nextChunkRows(int(rows[0]), rows[1], rows[2]); got != want {
			t.Errorf("nextChunkRows%v = %d, want %d", rows, got, want)
		}
	}
	if f.ftsChunkRows != ftsRebuildChunkRows || ftsTables[2] != "messages_fts_cjk" {
		t.Fatal("instance chunk size starts at the first chunk rows")
	}
	if got, err := f.hasFTSTrash(ctx, bare); err != nil || got {
		t.Fatalf("no trash: %v %v", got, err)
	}
	mustExecConn(t, bare, "CREATE TABLE ftsxv22xtrashxdata (x)")
	if got, _ := f.hasFTSTrash(ctx, bare); got {
		t.Fatal("underscores match literally")
	}
	mustExecConn(t, bare, "CREATE TABLE fts_v22_trash_messages_fts_data (x)")
	if got, err := f.hasFTSTrash(ctx, bare); err != nil || !got {
		t.Fatalf("trash present: %v %v", got, err)
	}
}

func TestCorruptionErrorClassificationRequiresFTSEvidence(t *testing.T) {
	corrupt := func(class SQLiteClass, msg string, code int) error {
		return &SQLiteError{Class: class, Msg: msg, Code: code, HasCode: code != 0}
	}
	if isFTSWriteCorruptionError(corrupt(SQLiteDatabaseError, "database disk image is malformed", 0)) {
		t.Fatal("generic malformed text")
	}
	if isFTSWriteCorruptionError(corrupt(SQLiteDatabaseError, "database disk image is malformed", sqlite3.SQLITE_CORRUPT)) {
		t.Fatal("structural SQLITE_CORRUPT")
	}
	if !isFTSWriteCorruptionError(corrupt(SQLiteDatabaseError, "database disk image is malformed", SQLiteCorruptVtab)) {
		t.Fatal("SQLITE_CORRUPT_VTAB")
	}
	if isFTSWriteCorruptionError(corrupt(SQLiteIntegrityError, `fts5: corrupt structure record for table "messages_fts"`, 1811)) {
		t.Fatal("contradictory SQLITE_CONSTRAINT_TRIGGER")
	}
	if !isFTSWriteCorruptionError(corrupt(SQLiteDatabaseError, `fts5: corrupt structure record for table "messages_fts"`, 0)) {
		t.Fatal("fts5 report naming messages_fts")
	}
	if isFTSWriteCorruptionError(corrupt(SQLiteDatabaseError, "no such table: nothing_fts_related", 0)) {
		t.Fatal("unrelated table")
	}
}

func TestFTSTableProbeAndEnsureSchema(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_FTS5_CJK_SO", "")
	t.Setenv("ICEBATH_CJK_FTS", "")
	conn, path := schemaConn(t)
	f := newSessionFTSSetup(&fakeFTSHost{path: path})
	if st, err := f.ftsTableProbe(ctx, conn, "messages_fts"); err != nil || st != ftsTableStatusQueryable {
		t.Fatalf("queryable = %v %v", st, err)
	}
	if st, err := f.ftsTableProbe(ctx, conn, "messages_fts_cjk"); err != nil || st != ftsTableStatusAbsent {
		t.Fatalf("absent = %v %v", st, err)
	}
	if st, err := f.ftsTableProbe(ctx, conn, "messages_fts LIMIT"); err == nil || st != ftsTableStatusDegraded {
		t.Fatalf("other operational errors raise: %v %v", st, err)
	}
	if !LoadFTS5CJKExtension(ctx, conn) {
		t.Fatal("tokenizer")
	}
	mustExecConn(t, conn, FTSCJKTableSQL)
	plain, err := ConnectSQLite(path, SQLiteConfig{Timeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	logs := captureSlog(t)
	if st, err := f.ftsTableProbe(ctx, plain, "messages_fts_cjk"); err != nil || st != ftsTableStatusQueryable {
		t.Fatalf("SQLite loads an FTS5 tokenizer lazily, so a bare SELECT connects: %v %v", st, err)
	}
	noTokenizer := failingConn{SQLiteConn: plain, err: &SQLiteError{Class: SQLiteOperationalError, Msg: "no such tokenizer: cjk_unicode61"}}
	if st, err := f.ftsTableProbe(ctx, noTokenizer, "messages_fts_cjk"); err != nil || st != ftsTableStatusDegraded {
		t.Fatalf("missing tokenizer degrades: %v %v", st, err)
	}
	if !f.trigramUnavailableWarned.Load() || !strings.Contains(logs.String(), "SQLite trigram tokenizer unavailable for "+path) {
		t.Fatalf("trigram warning: %s", logs.String())
	}
	f.ftsTableProbe(ctx, noTokenizer, "messages_fts_cjk")
	if strings.Count(logs.String(), "trigram tokenizer unavailable") != 1 {
		t.Fatal("the trigram warning is logged once")
	}
	if ok, err := f.ensureFTSSchema(ctx, conn, "messages_fts", FTSSQL); err != nil || !ok {
		t.Fatalf("ensure on a live table = %v %v", ok, err)
	}
	mustExecConn(t, conn, "DROP TRIGGER messages_fts_insert")
	if ok, err := f.ensureFTSSchema(ctx, conn, "messages_fts", FTSSQL); err != nil || !ok || !slices.Contains(liveTriggers(t, conn), "messages_fts_insert") {
		t.Fatalf("ensure recreates a dropped trigger = %v %v", ok, err)
	}
	if ok, err := f.ensureFTSSchema(ctx, noTokenizer, "messages_fts_cjk", FTSCJKTableSQL); err != nil || ok {
		t.Fatalf("degraded probe = %v %v", ok, err)
	}
	if ok, err := f.ensureFTSSchema(ctx, plain, "other_fts", "CREATE VIRTUAL TABLE other_fts USING fts5(c, tokenize='cjk_unicode61')"); err != nil || ok {
		t.Fatalf("missing tokenizer in the DDL = %v %v", ok, err)
	}
	if _, err := f.ensureFTSSchema(ctx, plain, "other_fts", "CREATE GARBAGE"); err == nil {
		t.Fatal("other DDL errors raise")
	}
	f.ftsEnabled.Store(true)
	if ok, err := f.ensureFTSSchema(ctx, plain, "x_fts", "CREATE VIRTUAL TABLE x_fts USING nofts5(c)"); err != nil || ok {
		t.Fatalf("no such module = %v %v", ok, err)
	}
	if _, err := f.ensureFTSSchema(ctx, plain, "x_fts", "CREATE VIRTUAL TABLE x_fts USING nofts5x(c)"); err != nil {
		t.Fatal(err)
	}
	if f.ftsEnabled.Load() || strings.Count(logs.String(), "SQLite FTS5 unavailable for") != 1 || !strings.Contains(logs.String(), "icebath update") {
		t.Fatalf("FTS5 unavailable disables search and warns once: %s", logs.String())
	}
	if err := f.dropAllFTSTriggers(ctx, conn); err != nil || len(liveTriggers(t, conn)) != 0 {
		t.Fatalf("every FTS trigger dropped: %v %v", liveTriggers(t, conn), err)
	}
	mustExecConn(t, conn, "BEGIN")
	mustExecConn(t, conn, "INSERT INTO state_meta (key, value) VALUES ('k', 'v')")
	if err := executeScript(ctx, conn, "CREATE TABLE s1 (x); CREATE TABLE s2 (x);"); err != nil {
		t.Fatal(err)
	}
	if auto, _ := sqliteAutocommit(conn); !auto {
		t.Fatal("executescript leaves autocommit")
	}
	if v, ok := metaValue(t, plain, "k"); !ok || v != "v" {
		t.Fatal("executescript commits the open transaction first")
	}
}

func TestEnsureFTSCJKSchemaLifecycle(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_FTS5_CJK_SO", "")
	t.Setenv("ICEBATH_CJK_FTS", "")
	conn, path := schemaConn(t)
	f := newSessionFTSSetup(&fakeFTSHost{path: path})
	f.ftsCJKLoaded.Store(LoadFTS5CJKExtension(ctx, conn))
	if err := f.ensureFTSCJKSchema(ctx, conn); err != nil || !f.ftsCJKAvailable.Load() {
		t.Fatalf("an empty DB is born complete: %v", err)
	}
	if _, ok := metaValue(t, conn, "fts_cjk_rebuild_high_water"); ok {
		t.Fatal("no backfill markers on an empty DB")
	}
	if got := liveTriggers(t, conn); !slices.Contains(got, "messages_fts_cjk_insert") {
		t.Fatalf("cjk triggers installed: %v", got)
	}

	populated, ppath := schemaConn(t)
	addMessage(t, populated, "user", "기존 메시지", "")
	addMessage(t, populated, "tool", "tool blob", "terminal")
	mustExecConn(t, populated, "INSERT INTO state_meta (key, value) VALUES ('fts_cjk_stale', '1')")
	g := newSessionFTSSetup(&fakeFTSHost{path: ppath})
	g.ftsCJKLoaded.Store(LoadFTS5CJKExtension(ctx, populated))
	if err := g.ensureFTSCJKSchema(ctx, populated); err != nil || g.ftsCJKAvailable.Load() {
		t.Fatalf("a populated DB waits for backfill: %v", err)
	}
	if hw, _ := metaValue(t, populated, "fts_cjk_rebuild_high_water"); hw != "2" {
		t.Fatalf("high water = %q", hw)
	}
	if p, _ := metaValue(t, populated, "fts_cjk_rebuild_progress"); p != "0" {
		t.Fatalf("progress = %q", p)
	}
	if _, ok := metaValue(t, populated, FTSCJKStaleKey); ok {
		t.Fatal("a stale breadcrumb for a missing table is cleared")
	}
	mustExecConn(t, populated, "INSERT INTO state_meta (key, value) VALUES ('fts_cjk_stale', '1')")
	mustExecConn(t, populated, "DROP TRIGGER messages_fts_cjk_insert")
	g.ftsCJKAvailable.Store(true)
	if err := g.ensureFTSCJKSchema(ctx, populated); err != nil || g.ftsCJKAvailable.Load() || slices.Contains(liveTriggers(t, populated), "messages_fts_cjk_insert") {
		t.Fatalf("a stale index keeps its triggers dropped: %v", err)
	}

	plain, err := ConnectSQLite(path, SQLiteConfig{Timeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	h := newSessionFTSSetup(&fakeFTSHost{path: path})
	h.ftsCJKAvailable.Store(true)
	logs := captureSlog(t)
	if err := h.ensureFTSCJKSchema(ctx, plain); err != nil || h.ftsCJKAvailable.Load() {
		t.Fatalf("unloaded tokenizer detaches: %v", err)
	}
	if got := liveTriggers(t, plain); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "messages_fts_cjk") }) {
		t.Fatalf("cjk triggers dropped: %v", got)
	}
	if v, _ := metaValue(t, plain, FTSCJKStaleKey); v != "1" || !strings.Contains(logs.String(), "icebath sessions optimize-storage") {
		t.Fatalf("stale breadcrumb and warning: %q %s", v, logs.String())
	}
	addMessage(t, plain, "user", "writes keep working", "")

	h.ftsCJKLoaded.Store(true)
	if err := h.ensureFTSCJKSchema(ctx, plain); err != nil || h.ftsCJKAvailable.Load() {
		t.Fatalf("a stale breadcrumb blocks reinstall: %v", err)
	}
	mustExecConn(t, plain, "DELETE FROM state_meta WHERE key = 'fts_cjk_stale'")
	mustExecConn(t, conn, "DROP TABLE messages_fts_cjk")
	if err := h.ensureFTSCJKSchema(ctx, plain); err != nil || h.ftsCJKAvailable.Load() || !strings.Contains(logs.String(), "messages_fts_cjk ensure failed") {
		t.Fatalf("a missing tokenizer on a loaded flag soft fails: %v", err)
	}

	h.ftsCJKLoaded.Store(false)
	h.ftsCJKAvailable.Store(true)
	locked := failingConn{SQLiteConn: plain, err: &SQLiteError{Class: SQLiteOperationalError, Msg: "database is locked"}}
	if err := h.ensureFTSCJKSchema(ctx, locked); err != nil || h.ftsCJKAvailable.Load() {
		t.Fatalf("a locked presence check degrades and never raises: %v", err)
	}
	broken := failingConn{SQLiteConn: plain, err: &SQLiteError{Class: SQLiteDatabaseError, Msg: "database disk image is malformed"}}
	if err := h.ensureFTSCJKSchema(ctx, broken); err == nil {
		t.Fatal("non operational errors propagate")
	}
}

// Connection whose every statement fails with err
type failingConn struct {
	SQLiteConn
	err error
}

func (c failingConn) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, c.err
}
func (c failingConn) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, c.err
}

func TestEnterFTSFailOpenDetachesWithinTheWriteBudget(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_FTS5_CJK_SO", "")
	t.Setenv("ICEBATH_CJK_FTS", "")
	conn, path := schemaConn(t)
	LoadFTS5CJKExtension(ctx, conn)
	mustExecConn(t, conn, FTSCJKTableSQL)
	mustExecConn(t, conn, FTSCJKTriggerSQL)
	vtab := &SQLiteError{Class: SQLiteDatabaseError, Msg: "database disk image is malformed", Code: SQLiteCorruptVtab, HasCode: true}
	host := &fakeFTSHost{path: path, reopenConn: conn, patience: 20}
	f := newSessionFTSSetup(host)
	if ok, err := f.enterFTSFailOpen(ctx, vtab, nil, nil); ok || err != nil {
		t.Fatal("disabled FTS never detaches")
	}
	f.ftsEnabled.Store(true)
	if ok, err := f.enterFTSFailOpen(ctx, errors.New("database disk image is malformed"), nil, nil); ok || err != nil {
		t.Fatal("structural corruption never detaches")
	}
	host.corruptErr = &StateDBCorruptError{Msg: "quarantined"}
	if _, err := f.enterFTSFailOpen(ctx, vtab, nil, nil); err != host.corruptErr {
		t.Fatalf("a quarantined handle raises: %v", err)
	}
	host.corruptErr = nil
	host.replacedErr = &StateDBReplacedError{Msg: "replaced"}
	if _, err := f.enterFTSFailOpen(ctx, vtab, nil, nil); err != host.replacedErr {
		t.Fatalf("a replaced file raises: %v", err)
	}
	host.replacedErr = nil
	host.reopenErr = &SQLiteError{Class: SQLiteProgrammingError, Msg: "closed (read-only handle)"}
	logs := captureSlog(t)
	if ok, err := f.enterFTSFailOpen(ctx, vtab, nil, nil); ok || err != nil || !slices.Equal(host.reopens, []string{"write"}) {
		t.Fatalf("a sqlite error from reopen gives up: %v %v %v", ok, err, host.reopens)
	}
	if !strings.Contains(logs.String(), "Could not detach corrupt FTS indexes") {
		t.Fatalf("give up logged: %s", logs.String())
	}
	host.reopenErr = nil

	sibling, err := ConnectSQLite(path, SQLiteConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer sibling.Close()
	mustExecConn(t, conn, "PRAGMA busy_timeout = 0")
	mustExecConn(t, sibling, "BEGIN IMMEDIATE")
	var sleeps []float64
	host.sleep = func(deadline, patienceS float64) bool {
		sleeps = append(sleeps, patienceS)
		if len(sleeps) == 1 {
			mustExecConn(t, sibling, "COMMIT")
		}
		return true
	}
	patience := 3.0
	ok, err := f.enterFTSFailOpen(ctx, vtab, nil, &patience)
	if !ok || err != nil || !slices.Equal(sleeps, []float64{3}) || len(host.reopens) != 2 {
		t.Fatalf("a busy write lock is waited out: %v %v %v %v", ok, err, sleeps, host.reopens)
	}
	if got := liveTriggers(t, conn); len(got) != 0 {
		t.Fatalf("every FTS trigger dropped: %v", got)
	}
	for _, key := range []string{FTSStaleKey, FTSCJKStaleKey} {
		if v, _ := metaValue(t, conn, key); v != "1" {
			t.Fatalf("%s = %q", key, v)
		}
	}
	if f.ftsEnabled.Load() || !f.ftsStale.Load() || f.trigramAvailable.Load() || f.ftsCJKAvailable.Load() {
		t.Fatal("flags after fail open")
	}
	if auto, _ := sqliteAutocommit(conn); !auto {
		t.Fatal("the detach transaction committed")
	}

	f.ftsEnabled.Store(true)
	mustExecConn(t, sibling, "BEGIN IMMEDIATE")
	defer mustExecConn(t, sibling, "ROLLBACK")
	host.sleep = func(float64, float64) bool { return false }
	deadline := monotonicSeconds()
	if ok, err := f.enterFTSFailOpen(ctx, vtab, &deadline, nil); ok || err != nil {
		t.Fatalf("an exhausted budget gives up: %v %v", ok, err)
	}
	if !f.ftsEnabled.Load() {
		t.Fatal("a failed detach leaves FTS enabled")
	}
}
