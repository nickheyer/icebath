package selfupdate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// A real state.db built through the session store API, then closed
func populatedDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sess-alpha", "sess-beta"} {
		if err := db.CreateSession(state.NewSession{ID: id, Source: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range [][3]string{{"sess-alpha", "user", "hello world one"}, {"sess-alpha", "assistant", "hi back"}, {"sess-beta", "user", "second session message"}} {
		if _, err := db.AppendMessage(ctx, m[0], map[string]any{"role": m[1], "content": m[2]}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

// Stats of a database, failing the test when collection errors
func collectStats(t *testing.T, dbPath string) *pyjson.Object {
	t.Helper()
	stats, err := state.CollectStateDBStats(dbPath)
	if err != nil || stats == nil {
		t.Fatalf("stats = %v, err = %v", stats, err)
	}
	return stats
}

// Integer value of a stats field, failing the test for a missing or non integer value
func statInt(t *testing.T, stats *pyjson.Object, key string) int64 {
	t.Helper()
	v, _ := stats.Get(key)
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	}
	t.Fatalf("%s = %#v is not an integer", key, v)
	return 0
}

// Runs statements on a plain connection to a database
func execStatements(t *testing.T, dbPath string, statements ...[]any) {
	t.Helper()
	conn, err := state.ConnectSQLite(dbPath, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, statement := range statements {
		if _, err := conn.ExecContext(context.Background(), statement[0].(string), statement[1:]...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCollectStatsSaneValues(t *testing.T) {
	stats := collectStats(t, populatedDB(t))
	pageCount, pageSize := statInt(t, stats, "page_count"), statInt(t, stats, "page_size")
	if pageCount <= 0 || pageSize <= 0 || statInt(t, stats, "logical_size_bytes") != pageCount*pageSize {
		t.Fatalf("page_count = %d, page_size = %d", pageCount, pageSize)
	}
	if statInt(t, stats, "freelist_count") < 0 || statInt(t, stats, "wal_size_bytes") < 0 {
		t.Fatal("negative freelist or WAL size")
	}
	if mode, _ := stats.Get("journal_mode"); mode == "" || reflect.TypeOf(mode).Kind() != reflect.String {
		t.Fatalf("journal_mode = %#v", mode)
	}
	if statInt(t, stats, "messages") < 3 || statInt(t, stats, "sessions") < 2 {
		t.Fatal("row counts below what was written")
	}
	raw, _ := stats.Get("fts_tables")
	fts, ok := raw.(*pyjson.Object)
	if !ok {
		t.Fatalf("fts_tables = %#v", raw)
	}
	names := map[string]bool{}
	for _, name := range fts.Keys {
		names[name] = true
		if _, isBool := fts.Vals[name].(bool); !isBool {
			t.Fatalf("fts_tables[%s] = %#v", name, fts.Vals[name])
		}
	}
	if !reflect.DeepEqual(names, map[string]bool{"messages_fts": true, "messages_fts_trigram": true, "messages_fts_cjk": true}) {
		t.Fatalf("fts tables = %v", names)
	}
	if version, _ := stats.Get("fts_storage_version"); version != nil {
		statInt(t, stats, "fts_storage_version")
	}
	if pending, _ := stats.Get("fts_rebuild_pending"); pending != nil && pending != false {
		t.Fatalf("fts_rebuild_pending = %#v", pending)
	}
}

func TestCollectStatsIsReadOnly(t *testing.T) {
	dbPath := populatedDB(t)
	beforeBytes, _ := os.ReadFile(dbPath)
	beforeInfo, _ := os.Stat(dbPath)
	collectStats(t, dbPath)
	afterBytes, _ := os.ReadFile(dbPath)
	afterInfo, _ := os.Stat(dbPath)
	if !bytes.Equal(beforeBytes, afterBytes) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("collecting stats changed the database")
	}
	db, err := state.NewSessionDB(context.Background(), dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if session, err := db.GetSession(context.Background(), "sess-alpha"); err != nil || session == nil {
		t.Fatalf("session = %v, err = %v", session, err)
	}
}

func TestCollectAndRenderStaleFtsHolderDeferral(t *testing.T) {
	dbPath := populatedDB(t)
	execStatements(t, dbPath, []any{"INSERT INTO state_meta (key, value) VALUES (?, ?)", "fts_rebuild_deferral",
		`{"attempts": 4, "holder_pids": [4242], "first_seen": 1.0}`})
	stats := collectStats(t, dbPath)
	deferral, _ := stats.Get("fts_rebuild_deferral")
	if attempts, _ := doctorDictGet(deferral, "attempts"); !pyjson.Equal(attempts, int64(4)) && !pyjson.Equal(attempts, 4) {
		t.Fatalf("attempts = %#v", attempts)
	}
	pids, _ := doctorDictGet(deferral, "holder_pids")
	if list, ok := pids.([]any); !ok || len(list) != 1 || (!pyjson.Equal(list[0], int64(4242)) && !pyjson.Equal(list[0], 4242)) {
		t.Fatalf("holder_pids = %#v", pids)
	}
	found := false
	for _, row := range doctorRenderStateDBStats(stats, nil, "") {
		warning := row.Text + " " + row.Detail
		found = found || (row.Kind == "warn" && strings.Contains(warning, "4242") && strings.Contains(warning, "optimize-storage"))
	}
	if !found {
		t.Fatalf("rows = %+v", doctorRenderStateDBStats(stats, nil, ""))
	}
}

func TestCollectStatsMissingFileNeverRaises(t *testing.T) {
	stats := collectStats(t, filepath.Join(t.TempDir(), "nope", "state.db"))
	for _, key := range []string{"page_count", "messages", "logical_size_bytes"} {
		if v, _ := stats.Get(key); v != nil {
			t.Fatalf("%s = %#v", key, v)
		}
	}
}

func TestCollectStatsRebuildPendingFlag(t *testing.T) {
	dbPath := populatedDB(t)
	execStatements(t, dbPath,
		[]any{"INSERT OR REPLACE INTO state_meta(key, value) VALUES ('fts_rebuild_high_water', '100')"},
		[]any{"INSERT OR REPLACE INTO state_meta(key, value) VALUES ('fts_rebuild_progress', '40')"})
	stats := collectStats(t, dbPath)
	if pending, _ := stats.Get("fts_rebuild_pending"); pending != true {
		t.Fatalf("fts_rebuild_pending = %#v", pending)
	}
	if statInt(t, stats, "fts_rebuild_high_water") != 100 || statInt(t, stats, "fts_rebuild_progress") != 40 {
		t.Fatal("rebuild bookkeeping does not read back")
	}
}

// Asserts the holder count sees a connection this process holds open
func assertSeesOwnOpenConnection(t *testing.T, dbPath string) {
	t.Helper()
	conn, err := state.ConnectSQLite(dbPath, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if holders, ok := state.CountDBHolders(dbPath); !ok || holders < 1 {
		t.Fatalf("holders = %d, counted = %v", holders, ok)
	}
}

func TestCountDbHoldersSeesOpenConnectionLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux holder scan")
	}
	assertSeesOwnOpenConnection(t, populatedDB(t))
}

func TestCountDbHoldersSeesOpenConnectionMacos(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS holder scan")
	}
	assertSeesOwnOpenConnection(t, populatedDB(t))
}

func TestCountDbHoldersMissingPathNoRaise(t *testing.T) {
	if holders, ok := state.CountDBHolders(filepath.Join(t.TempDir(), "absent.db")); ok && holders < 0 {
		t.Fatalf("holders = %d", holders)
	}
}

// Healthy stats mapping the render cases override field by field
func baseStats(overrides map[string]any) *pyjson.Object {
	fts := pyjson.NewObject()
	fts.Set("messages_fts", true)
	fts.Set("messages_fts_trigram", true)
	fts.Set("messages_fts_cjk", false)
	stats := pyjson.NewObject()
	for _, field := range []struct {
		key   string
		value any
	}{
		{"page_count", int64(100)}, {"page_size", int64(4096)}, {"freelist_count", int64(2)}, {"logical_size_bytes", int64(100 * 4096)},
		{"wal_size_bytes", int64(1024)}, {"journal_mode", "wal"}, {"messages", int64(42)}, {"sessions", int64(7)}, {"fts_tables", fts},
		{"fts_storage_version", int64(1)}, {"fts_storage_upgrade_needed", false}, {"fts_rebuild_pending", false},
		{"fts_rebuild_high_water", nil}, {"fts_rebuild_progress", nil},
	} {
		stats.Set(field.key, field.value)
	}
	for key, value := range overrides {
		stats.Set(key, value)
	}
	return stats
}

// Every part of every rendered row joined into one searchable text
func renderedBlob(rows []doctorStatRow) string {
	parts := []string{}
	for _, row := range rows {
		parts = append(parts, row.Kind, row.Text, row.Detail)
	}
	return strings.Join(parts, " ")
}

func TestRenderHealthyStatsNoWarnings(t *testing.T) {
	holders := 2
	lines := doctorRenderStateDBStats(baseStats(nil), &holders, "")
	texts := []string{}
	for _, row := range lines {
		if row.Kind == "warn" {
			t.Fatalf("warn row = %+v", row)
		}
		texts = append(texts, row.Text)
	}
	joined := strings.Join(texts, " | ")
	for _, want := range []string{"42", "7", "2"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rows lack %q: %s", want, joined)
		}
	}
	if !strings.Contains(strings.ToLower(joined), "wal") {
		t.Fatalf("rows lack the journal mode: %s", joined)
	}
}

func TestRenderWarnsOnLargeDb(t *testing.T) {
	big := doctorStateDBSizeWarnBytes + 1
	lines := doctorRenderStateDBStats(baseStats(map[string]any{"logical_size_bytes": big, "page_count": big / 4096, "page_size": int64(4096)}), nil, "")
	warned := false
	for _, row := range lines {
		warned = warned || row.Kind == "warn"
	}
	if !warned {
		t.Fatalf("rows = %+v", lines)
	}
}

func TestRenderLargeDbWithPendingRebuildSuggestsOptimize(t *testing.T) {
	big := doctorStateDBSizeWarnBytes + 1
	lines := doctorRenderStateDBStats(baseStats(map[string]any{"logical_size_bytes": big, "fts_rebuild_pending": true}), nil, "")
	if !strings.Contains(renderedBlob(lines), "optimize-storage") {
		t.Fatalf("rows = %+v", lines)
	}
}

func TestRenderLargeDbLegacyTrigramSuggestsOptimize(t *testing.T) {
	big := doctorStateDBSizeWarnBytes + 1
	lines := doctorRenderStateDBStats(baseStats(map[string]any{"logical_size_bytes": big, "fts_storage_version": nil, "fts_storage_upgrade_needed": true}), nil, "")
	if !strings.Contains(renderedBlob(lines), "optimize-storage") {
		t.Fatalf("rows = %+v", lines)
	}
}

func TestRenderHandlesAllNoneStats(t *testing.T) {
	empty := pyjson.NewObject()
	for _, key := range baseStats(nil).Keys {
		empty.Set(key, nil)
	}
	if lines := doctorRenderStateDBStats(empty, nil, ""); lines == nil {
		t.Fatal("render returned no list")
	}
}

func TestDoctorNamesTheCommandThatRemovesAnOrphanAlias(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv("HOME", tmp)
	icebathHome := filepath.Join(tmp, ".icebath")
	t.Setenv(home.EnvHome, icebathHome)
	profiles.ResetFileCache()
	profiles.ResetStandaloneMemo()
	home.ResetKeyCache()
	savedHooks := profiles.GatewayStatusHooks
	t.Cleanup(func() { profiles.GatewayStatusHooks = savedHooks })
	profiles.GatewayStatusHooks.ProfileRunning = func(string) bool { return false }
	profiles.GatewayStatusHooks.ServesNamedProfile = func(string) bool { return false }
	toolsSeam(t, &doctorDuplicateCredentialFindings, func(context.Context) ([]string, error) { return nil, nil })
	writeTestFile(t, filepath.Join(icebathHome, "profiles", "keep", "config.yaml"), "model: m\n", 0o644)
	wrapper, err := profiles.CreateWrapperScript("petcare", "")
	if err != nil || wrapper == "" {
		t.Fatalf("wrapper = %q, err = %v", wrapper, err)
	}
	var report bytes.Buffer
	d := &Doctor{W: &report, Home: icebathHome, ProjectRoot: t.TempDir(), DisplayHome: icebathHome}
	if _, err := checkProfiles(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.String(), "(remove with: icebath profile alias petcare --remove)") {
		t.Fatalf("output:\n%s", report.String())
	}
	if owned, err := profiles.WrapperPath("petcare"); err != nil || owned != wrapper {
		t.Fatalf("the named alias resolves %q, err = %v, want %q", owned, err, wrapper)
	}
}
