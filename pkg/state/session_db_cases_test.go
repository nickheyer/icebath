package state

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const admissionHolderHelperEnv = "ICEBATH_ADMISSION_HOLDER_HELPER"

// Opens a session store at path or fails the test
func mustSessionDB(t *testing.T, path string, readOnly bool) *SessionDB {
	t.Helper()
	db, err := NewSessionDB(context.Background(), path, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// Creates a session and appends user messages through the store
func seedSession(t *testing.T, db *SessionDB, sid string, contents ...string) {
	t.Helper()
	if err := db.CreateSession(NewSession{ID: sid, Source: "cli", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range contents {
		if _, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": "user", "content": c}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
}

// Integer of a single value query on a raw connection
func rawCount(t *testing.T, conn SQLiteConn, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Emulates the recover residue for one FTS family, its vtable row gone and its shadows kept
func orphanFTSFamily(t *testing.T, path, family string) {
	t.Helper()
	conn := openRaw(t, path)
	defer conn.Close()
	ctx := context.Background()
	execAll(t, conn, "PRAGMA writable_schema=ON")
	if _, err := conn.ExecContext(ctx, "DELETE FROM sqlite_master WHERE name = ? AND sql LIKE 'CREATE VIRTUAL TABLE%'", family); err != nil {
		t.Fatal(err)
	}
	version := rawCount(t, conn, "PRAGMA schema_version")
	execAll(t, conn, fmt.Sprintf("PRAGMA schema_version=%d", version+1), "PRAGMA writable_schema=OFF")
}

// Rowid, type and name of every sqlite_master row whose name starts with prefix
func ftsMasterRows(t *testing.T, path, prefix string) []string {
	t.Helper()
	conn := openRaw(t, path)
	defer conn.Close()
	rows, err := conn.QueryContext(context.Background(), "SELECT rowid, type, name FROM sqlite_master WHERE name LIKE ? ESCAPE '\\' ORDER BY rowid",
		strings.ReplaceAll(prefix, "_", "\\_")+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rowid int64
		var kind, name string
		if err := rows.Scan(&rowid, &kind, &name); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d %s %s", rowid, kind, name))
	}
	return out
}

func TestOrphanedBaseFamilyIsRepairedAndHealthyTrigramUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := mustSessionDB(t, path, false)
	seedSession(t, db, "s1", "recovered orphan 0", "recovered orphan 1", "recovered orphan 2")
	db.Close()
	orphanFTSFamily(t, path, "messages_fts")
	trigramBefore := ftsMasterRows(t, path, "messages_fts_trigram")
	if len(trigramBefore) == 0 {
		t.Fatal("fixture needs a live trigram family")
	}
	raw := openRaw(t, path)
	shadows := rawCount(t, raw, "SELECT count(*) FROM sqlite_master WHERE name IN ('messages_fts_data', 'messages_fts_config')")
	raw.Close()
	if shadows != 2 {
		t.Fatalf("fixture must leave the base shadows behind, got %d", shadows)
	}
	reopened := mustSessionDB(t, path, false)
	if !reopened.ftsEnabled.Load() {
		reopened.Close()
		t.Fatal("FTS not enabled after orphan repair")
	}
	reopened.mu.Lock()
	hits := rawCount(t, reopened.conn, "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'orphan'")
	reopened.mu.Unlock()
	reopened.Close()
	if hits != 3 {
		t.Fatalf("recreated index must be rebuilt from the canonical messages table, got %d hits", hits)
	}
	if after := ftsMasterRows(t, path, "messages_fts_trigram"); strings.Join(after, "|") != strings.Join(trigramBefore, "|") {
		t.Fatalf("trigram family changed %v -> %v", trigramBefore, after)
	}
}

func TestAdmissionHolderHelperProcess(t *testing.T) {
	if os.Getenv(admissionHolderHelperEnv) == "" {
		return
	}
	zero := 0.0
	_ = FTSRebuildAdmission(os.Getenv(admissionHolderHelperEnv), &zero, func(admitted bool) error {
		if !admitted {
			os.Exit(2)
		}
		fmt.Println("locked")
		time.Sleep(30 * time.Second)
		return nil
	})
	os.Exit(0)
}

// Reports whether this SQLite build ships the trigram tokenizer
func trigramSupported(t *testing.T) bool {
	t.Helper()
	conn := openRaw(t, ":memory:")
	defer conn.Close()
	_, err := conn.ExecContext(context.Background(), "CREATE VIRTUAL TABLE trigram_probe USING fts5(content, tokenize='trigram')")
	return err == nil
}

func TestFreshFTSBootstrapDoesNotPublishSchemaWithoutAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestAdmissionHolderHelperProcess$")
	cmd.Env = append(os.Environ(), admissionHolderHelperEnv+"="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	stop := func() {
		if !killed {
			killed = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	defer stop()
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("holder never locked: %v", err)
		}
		if strings.TrimSpace(line) == "locked" {
			break
		}
	}
	saved := ftsRebuildLockTimeoutSeconds
	ftsRebuildLockTimeoutSeconds = 0.1
	deferred, err := NewSessionDB(context.Background(), path, false)
	ftsRebuildLockTimeoutSeconds = saved
	if err != nil {
		t.Fatal(err)
	}
	stale, enabled := deferred.ftsStale.Load(), deferred.ftsEnabled.Load()
	deferred.mu.Lock()
	published := rawCount(t, deferred.conn, "SELECT count(*) FROM sqlite_master WHERE name = 'messages_fts'")
	deferred.mu.Unlock()
	deferred.Close()
	if !stale || enabled || published != 0 {
		t.Fatalf("deferred opener stale %v enabled %v published %d", stale, enabled, published)
	}
	stop()
	recovered := mustSessionDB(t, path, false)
	defer recovered.Close()
	if recovered.ftsStale.Load() || !recovered.ftsEnabled.Load() {
		t.Fatalf("recovered stale %v enabled %v", recovered.ftsStale.Load(), recovered.ftsEnabled.Load())
	}
	if trigramSupported(t) {
		recovered.mu.Lock()
		trigram := rawCount(t, recovered.conn, "SELECT count(*) FROM sqlite_master WHERE name = 'messages_fts_trigram'")
		recovered.mu.Unlock()
		if !recovered.trigramAvailable.Load() || trigram != 1 {
			t.Fatalf("trigram available %v published %d", recovered.trigramAvailable.Load(), trigram)
		}
	}
	seedSession(t, recovered, "s1", "fresh bootstrap recovered")
	hits, err := recovered.sessionSearch.SearchMessages(context.Background(), "recovered", SearchMessagesOptions{Limit: 20})
	if err != nil || len(hits) == 0 {
		t.Fatalf("search hits %d err %v", len(hits), err)
	}
}

func TestConstructedSessionDBIsRegistered(t *testing.T) {
	t.Setenv(testIsolationMarkerEnv, "1")
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	found := false
	for _, live := range liveTestInstances() {
		found = found || live == db
	}
	db.Close()
	if !found {
		t.Fatal("constructed SessionDB missing from the test instance registry")
	}
	if !db.closedForTest() {
		t.Fatal("close kept the writer connection")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestLeakedInstanceIsClosedByTheSweep(t *testing.T) {
	t.Setenv(testIsolationMarkerEnv, "1")
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	if err := db.CreateSession(NewSession{ID: "leak-probe", Source: "cli", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if db.closedForTest() {
		t.Fatal("leaked instance closed within the test")
	}
	CloseLeakedTestInstances()
	if !db.closedForTest() {
		t.Fatal("the sweep left a leaked instance open")
	}
}

func TestSessionDBRepairsReadonlyDirBeforeFreshDBLock(t *testing.T) {
	h := repairHome(t)
	sub := filepath.Join(h, "profiles", "p1")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sub, 0o755)
	db := mustSessionDB(t, filepath.Join(sub, "state.db"), false)
	db.Close()
	if !osAccess(sub, 2) {
		t.Fatal("profile directory still read only")
	}
}

func TestSessionDBSelfhealsReadonlyDBInHome(t *testing.T) {
	path := filepath.Join(repairHome(t), "state.db")
	mustSessionDB(t, path, false).Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if st, err := os.Stat(path + suffix); err == nil && st.Mode().IsRegular() {
			os.Chmod(path+suffix, 0o444)
		}
	}
	db := mustSessionDB(t, path, false)
	defer db.Close()
	if !osAccess(path, 2) {
		t.Fatal("state.db still read only")
	}
}

func TestSessionDBActionableErrorOutsideHome(t *testing.T) {
	repairHome(t)
	outside := filepath.Join(t.TempDir(), "custom-loc")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(outside, "state.db")
	mustSessionDB(t, path, false).Close()
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)
	setLastInitError("")
	defer setLastInitError("")
	db, err := NewSessionDB(context.Background(), path, false)
	if err == nil {
		db.Close()
		t.Fatal("read only store outside the home opened")
	}
	if !isOperationalError(err) || !strings.Contains(err.Error(), path) {
		t.Fatalf("refusal %s: %v", pyExceptionClassName(err), err)
	}
}

func TestSessionDBReadOnlyIsTracked(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	path := filepath.Join(dir, "state.db")
	seed := mustSessionDB(t, path, false)
	seedSession(t, seed, "s1")
	seed.Close()
	ro := mustSessionDB(t, path, true)
	live := HasLiveConnection(path)
	_, readable := ReadHeaderBytesPreopen(path, 16, false)
	ro.Close()
	if !live || readable {
		t.Fatalf("read only handle live %v header readable %v", live, readable)
	}
	if HasLiveConnection(path) {
		t.Fatal("closed read only handle still tracked")
	}
	if _, readable := ReadHeaderBytesPreopen(path, 16, false); !readable {
		t.Fatal("header unreadable after close")
	}
}

// Builds a store with a trigram family over sixty appended messages, skipping without the tokenizer
func buildDBWithTrigram(t *testing.T, path string) string {
	t.Helper()
	db := mustSessionDB(t, path, false)
	defer db.Close()
	if !db.trigramAvailable.Load() {
		t.Skip("trigram tokenizer unavailable in this SQLite build")
	}
	sid := NewUID()
	var contents []string
	for i := range 60 {
		contents = append(contents, fmt.Sprintf("quick brown fox %d lorem ipsum dolor %d", i, i*7))
	}
	seedSession(t, db, sid, contents...)
	return sid
}

// Leaves an index row at the next free segid, the residue of an aborted segment write
func plantStaleTrigramSegment(t *testing.T, path string) {
	t.Helper()
	conn := openRaw(t, path)
	defer conn.Close()
	ctx := context.Background()
	rows, err := conn.QueryContext(ctx, "SELECT segid FROM messages_fts_trigram_idx")
	if err != nil {
		t.Fatal(err)
	}
	used := map[int64]bool{}
	for rows.Next() {
		var segid int64
		if err := rows.Scan(&segid); err != nil {
			t.Fatal(err)
		}
		used[segid] = true
	}
	rows.Close()
	stale := int64(1)
	for used[stale] {
		stale++
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO messages_fts_trigram_idx(segid, term, pgno) VALUES (?, X'', 2)", stale); err != nil {
		t.Fatal(err)
	}
}

// Reports whether a raw append fails with the integrity error the collision raises
func realAppendFails(t *testing.T, path, sid string) bool {
	t.Helper()
	conn := openRaw(t, path)
	defer conn.Close()
	LoadFTS5CJKExtension(context.Background(), conn)
	_, err := conn.ExecContext(context.Background(), "INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, ?, ?, ?)",
		sid, "user", "zebra yak xylophone wombat", now())
	if err == nil {
		return false
	}
	if class, ok := sqliteClassOf(err); ok && class == SQLiteIntegrityError {
		return true
	}
	t.Fatal(err)
	return false
}

func TestWriteProbeReportsSegmentCollisionThatIntegrityCheckMisses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	sid := buildDBWithTrigram(t, path)
	plantStaleTrigramSegment(t, path)
	raw := openRaw(t, path)
	var integrity string
	if err := raw.QueryRowContext(context.Background(), "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity %q %v", integrity, err)
	}
	raw.Close()
	if !realAppendFails(t, path, sid) {
		t.Fatal("fixture must break real appends")
	}
	reason, sick, err := dbOpensCleanly(context.Background(), path)
	if err != nil || !sick || !strings.Contains(reason, "constraint failed") {
		t.Fatalf("probe %q sick %v err %v", reason, sick, err)
	}
	raw = openRaw(t, path)
	defer raw.Close()
	if n := rawCount(t, raw, "SELECT COUNT(*) FROM sessions"); n != 1 {
		t.Fatalf("sessions %d", n)
	}
}

func TestRepairHealsSegmentCollisionAndRestoresAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	sid := buildDBWithTrigram(t, path)
	plantStaleTrigramSegment(t, path)
	report, err := RepairStateDBSchema(context.Background(), path, false)
	if err != nil || !report.Repaired {
		t.Fatalf("repair %+v %v", report, err)
	}
	if reason, sick, err := dbOpensCleanly(context.Background(), path); sick || err != nil {
		t.Fatalf("still sick %q %v", reason, err)
	}
	if realAppendFails(t, path, sid) {
		t.Fatal("appends still fail after repair")
	}
	db := mustSessionDB(t, path, false)
	defer db.Close()
	db.mu.Lock()
	n := rawCount(t, db.conn, "SELECT COUNT(*) FROM messages WHERE session_id = ?", sid)
	db.mu.Unlock()
	if n != 61 {
		t.Fatalf("messages %d", n)
	}
}

func TestForeignStateDBHoldersMethodSeesNoForeignHolderOfAPrivateStore(t *testing.T) {
	db := usageDB(t)
	holders, err := db.ForeignStateDBHolders()
	if err != nil || len(holders) != 0 {
		t.Fatalf("holders = %v, %v", holders, err)
	}
}
