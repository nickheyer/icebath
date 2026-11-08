package state

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Environment variable naming the helper role a re-executed test binary plays
const stateTestHelperEnv = "ICEBATH_STATE_TEST_HELPER"

// Runs a helper role when the test binary was re-executed as a child process, true when it did
func runStateTestHelper() bool {
	role := os.Getenv(stateTestHelperEnv)
	if role == "" {
		return false
	}
	args := strings.Split(os.Getenv(stateTestHelperEnv+"_ARGS"), "\x1f")
	ctx := context.Background()
	switch role {
	case "hold-repair-lock":
		holdFlock(args[0])
	case "repair":
		report, err := RepairStateDBSchema(ctx, args[0], true)
		if err != nil {
			fmt.Println("ERROR", err)
			os.Exit(1)
		}
		out, _ := json.Marshal(map[string]any{"repaired": report.Repaired, "strategy": report.Strategy, "backup_path": report.BackupPath})
		fmt.Println(string(out))
	case "writer-after-stage":
		writerAfterStage(args[0], args[1], args[2], args[3])
	case "hot-wal-row":
		conn, err := ConnectSQLite(args[0], SQLiteConfig{Timeout: 5})
		if err != nil {
			fmt.Println("ERROR", err)
			os.Exit(1)
		}
		for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "INSERT INTO messages (body) VALUES ('committed-wal-row')"} {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				fmt.Println("ERROR", err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	case "probe-repair-lock":
		repairLockTimeoutSeconds = 0.5
		holding := false
		if err := crossProcessRepairLock(args[0], func(h bool) error { holding = h; return nil }); err != nil {
			fmt.Println("ERROR", err)
			os.Exit(1)
		}
		fmt.Println(holding)
	case "peer-write":
		conn, err := ConnectSQLite(args[0], SQLiteConfig{Timeout: 0.3})
		if err != nil {
			fmt.Println("ERROR", err)
			os.Exit(1)
		}
		err = func() error {
			for _, stmt := range []string{"BEGIN IMMEDIATE", "INSERT INTO sessions VALUES('peer')", "COMMIT"} {
				if _, err := conn.ExecContext(ctx, stmt); err != nil {
					return err
				}
			}
			return nil
		}()
		conn.Close()
		if err != nil {
			fmt.Println("BLOCKED")
		} else {
			fmt.Println("WROTE")
		}
	default:
		fmt.Println("ERROR unknown helper role", role)
		os.Exit(2)
	}
	return true
}

// Child writer that signals readiness, waits for the staging signal and attempts one real commit
func writerAfterStage(dbPath, readyFile, startFile, resultFile string) {
	os.WriteFile(readyFile, []byte("ready"), 0o644)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(startFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.WriteFile(resultFile, []byte("not-started\x00repair did not reach staging"), 0o644)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	conn, err := ConnectSQLite(dbPath, SQLiteConfig{Timeout: 0.75})
	if err != nil {
		os.WriteFile(resultFile, []byte("failed\x00"+err.Error()), 0o644)
		return
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "INSERT INTO messages (body) VALUES ('committed-after-stage')"); err != nil {
		os.WriteFile(resultFile, []byte("failed\x00"+err.Error()), 0o644)
		return
	}
	os.WriteFile(resultFile, []byte("committed\x00"), 0o644)
}

// Child process running one helper role of this test binary
func spawnHelper(t *testing.T, role string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), stateTestHelperEnv+"="+role, stateTestHelperEnv+"_ARGS="+strings.Join(args, "\x1f"))
	return cmd
}

// Starts a helper and returns its first stdout line, failing when the child dies before printing one
func startHelper(t *testing.T, cmd *exec.Cmd) (string, func()) {
	t.Helper()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("helper printed nothing: %v", err)
	}
	stop := func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
	return strings.TrimSpace(line), stop
}

// Runs a helper to completion and returns its stdout, its logs flowing to this process's stderr
func runHelper(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// Store with one session of five user and five assistant messages, the hermes healthy repair fixture
func buildHealthyDB(t *testing.T, path string) string {
	t.Helper()
	db := mustSessionDB(t, path, false)
	raw := make([]byte, 16)
	rand.Read(raw)
	sid := hex.EncodeToString(raw)
	readSession(t, db, NewSession{ID: sid, Source: "cli"})
	for i := range 5 {
		readAppend(t, db, sid, map[string]any{"role": "user", "content": fmt.Sprintf("hello world %d", i)})
		readAppend(t, db, sid, map[string]any{"role": "assistant", "content": fmt.Sprintf("reply about pizza %d", i)})
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return sid
}

// Runs raw statements on a fresh connection to the file and closes it
func rawExecAll(t *testing.T, path string, stmts ...string) {
	t.Helper()
	conn := openRaw(t, path)
	execAll(t, conn, stmts...)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

// Injects a duplicate messages_fts row into sqlite_master, the malformed database schema class
func corruptDuplicateFTS(t *testing.T, path string) {
	t.Helper()
	rawExecAll(t, path, "PRAGMA writable_schema=ON",
		"INSERT INTO sqlite_master (type, name, tbl_name, rootpage, sql) SELECT type, name, tbl_name, rootpage, sql FROM sqlite_master WHERE name='messages_fts'")
}

// Overwrites the FTS5 shadow b-tree blocks of messages_fts so MATCH reads fail
func corruptFTSShadowSegments(t *testing.T, path string) {
	t.Helper()
	rawExecAll(t, path, "UPDATE messages_fts_data SET block = X'BADC0FFEE0DDF00D'")
}

// Overwrites the FTS5 shadow blocks so writes through the triggers fail while base reads still answer
func corruptFTSIndexData(t *testing.T, path string) {
	t.Helper()
	rawExecAll(t, path, "UPDATE messages_fts_data SET block = X'DEADBEEFDEADBEEF'")
}

// Makes a real b-tree index stale, an empty tree under a full definition
func corruptBtreeIndex(t *testing.T, path, index string) {
	t.Helper()
	ctx := context.Background()
	conn := openRaw(t, path)
	var original string
	if err := conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='index' AND name=?", index).Scan(&original); err != nil {
		t.Fatal(err)
	}
	setIndexSQL := func(conn SQLiteConn, sql string) {
		execAll(t, conn, "PRAGMA writable_schema=ON")
		if _, err := conn.ExecContext(ctx, "UPDATE sqlite_master SET sql=? WHERE type='index' AND name=?", sql, index); err != nil {
			t.Fatal(err)
		}
		version := rawCount(t, conn, "PRAGMA schema_version")
		execAll(t, conn, fmt.Sprintf("PRAGMA schema_version=%d", version+1), "PRAGMA writable_schema=OFF")
	}
	setIndexSQL(conn, original+" WHERE 0")
	conn.Close()
	conn = openRaw(t, path)
	execAll(t, conn, "REINDEX "+index)
	setIndexSQL(conn, original)
	conn.Close()
}

// Page size the populated repair fixtures use
const repairPageSize = 4096

// Multi page database with three sessions and twenty five messages in plain tables
func writePopulatedDB(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	conn := openRaw(t, path)
	execAll(t, conn, fmt.Sprintf("PRAGMA page_size=%d", repairPageSize),
		"CREATE TABLE sessions (id INTEGER PRIMARY KEY, name TEXT)", "CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT)")
	for i := range 3 {
		if _, err := conn.ExecContext(ctx, "INSERT INTO sessions (name) VALUES (?)", fmt.Sprintf("session-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 25 {
		if _, err := conn.ExecContext(ctx, "INSERT INTO messages (body) VALUES (?)", strings.Repeat(fmt.Sprintf("message body %d", i), 20)); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
}

// Aims page one's rightmost child at a data page, the malformed database schema with an empty name
func breakTheSchemaBtree(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pageCount := uint32(data[28])<<24 | uint32(data[29])<<16 | uint32(data[30])<<8 | uint32(data[31])
	if pageCount < 3 {
		t.Fatal("fixture needs a multi-page database")
	}
	put16 := func(off int, v uint16) { data[off], data[off+1] = byte(v>>8), byte(v) }
	put32 := func(off int, v uint32) {
		data[off], data[off+1], data[off+2], data[off+3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	}
	data[100] = 0x05
	put16(103, 1)
	put32(108, pageCount)
	put16(112, uint16(repairPageSize-6))
	put32(repairPageSize-6, pageCount)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Plain sessions and messages tables with seed rows in the requested journal mode
func makeRepairTestDB(t *testing.T, path, journalMode string) {
	t.Helper()
	conn := openRaw(t, path)
	execAll(t, conn, "CREATE TABLE sessions (id INTEGER PRIMARY KEY, name TEXT)", "CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT)",
		"INSERT INTO sessions (name) VALUES ('seed')", "INSERT INTO messages (body) VALUES ('seed')")
	var actual string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA journal_mode="+journalMode).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if journalMode == "wal" && actual != "wal" {
		t.Skip("SQLite build/filesystem does not support WAL")
	}
}

// Populated database whose schema b-tree points at data, refused by every read
func corruptDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	writePopulatedDB(t, path)
	breakTheSchemaBtree(t, path)
	conn := openRaw(t, path)
	_, err := conn.QueryContext(context.Background(), "SELECT * FROM sessions")
	conn.Close()
	if err == nil || !isDatabaseError(err) {
		t.Fatalf("fixture precondition: corrupt schema still reads (%v)", err)
	}
	return path
}

// Installs a disk usage answer for the repair guards
func fakeDiskUsage(t *testing.T, total, free uint64) {
	t.Helper()
	saved := diskUsageFn
	diskUsageFn = func(string) (uint64, uint64, uint64, error) { return total, total - min(total, free), free, nil }
	t.Cleanup(func() { diskUsageFn = saved })
}

// Hex digest of a file
func sha256Of(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Names in a directory matching a glob pattern
func globNames(t *testing.T, dir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// Swaps the repair probe for a fixed unhealthy answer
func forceUnhealthyProbe(t *testing.T) {
	t.Helper()
	saved := repairProbeFn
	repairProbeFn = func(context.Context, string) (string, bool, error) { return "forced-unhealthy", true, nil }
	t.Cleanup(func() { repairProbeFn = saved })
}

// Swaps the strategy runner for fn
func swapStrategies(t *testing.T, fn func(ctx context.Context, scratch string, report *RepairReport) error) {
	t.Helper()
	saved := repairStrategiesFn
	repairStrategiesFn = fn
	t.Cleanup(func() { repairStrategiesFn = saved })
}

// Repair report with a Fatal on a transport error
func repairReport(t *testing.T, path string, backup bool) *RepairReport {
	t.Helper()
	report, err := RepairStateDBSchema(context.Background(), path, backup)
	if err != nil {
		t.Fatal(err)
	}
	return report
}
