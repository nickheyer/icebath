package state

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	darwinScanBudget    = 2.0
	darwinScanHangGuard = 30 * time.Second
)

// One pid with one fd whose vnode lookup never returns
type hungLibproc struct{ release chan struct{} }

func (hungLibproc) ProcListPIDs(uint32, uint32, []byte) int { return 0 }

func (hungLibproc) ProcPIDInfo(_, _ int, _ uint64, buf []byte) int {
	binary.LittleEndian.PutUint32(buf[0:], 7)
	binary.LittleEndian.PutUint32(buf[4:], 1)
	return 8
}

func (l hungLibproc) ProcPIDFDInfo(int, int, int, []byte) int {
	<-l.release
	return 0
}

// One pid with one vnode fd naming target with the given identity
type healthyLibproc struct {
	target   string
	identity FileIdentity
}

func (healthyLibproc) ProcListPIDs(uint32, uint32, []byte) int { return 0 }

func (healthyLibproc) ProcPIDInfo(_, _ int, _ uint64, buf []byte) int {
	binary.LittleEndian.PutUint32(buf[0:], 7)
	binary.LittleEndian.PutUint32(buf[4:], 1)
	return 8
}

func (l healthyLibproc) ProcPIDFDInfo(_, _, _ int, record []byte) int {
	binary.LittleEndian.PutUint32(record[darwinFDDevOffset:], uint32(l.identity.Dev))
	binary.LittleEndian.PutUint64(record[darwinFDInoOffset:], l.identity.Ino)
	copy(record[darwinFDPathOffset:], l.target)
	return 1
}

// Runs the darwin sidecar scan so a hang fails the test instead of the suite
func runDarwinScanGuarded(t *testing.T, dbPath string) ([]Holder, time.Duration) {
	t.Helper()
	type outcome struct {
		holders []Holder
		err     error
	}
	done := make(chan outcome, 1)
	started := time.Now()
	go func() {
		holders, err := iterDarwinSidecarHolders(dbPath)
		done <- outcome{holders, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatal(o.err)
		}
		return o.holders, time.Since(started)
	case <-time.After(darwinScanHangGuard):
		t.Fatal("scan blocked past the hang guard: no deadline")
	}
	return nil, 0
}

func pinDarwinLibproc(t *testing.T, lib darwinLibprocAPI) {
	pinVar(t, &darwinLibproc, func() (darwinLibprocAPI, error) { return lib, nil })
	pinVar(t, &darwinAllPIDs, func(darwinLibprocAPI) []int { return []int{4242} })
	pinVar(t, &darwinFDScanTimeout, darwinScanBudget)
}

func TestHungVnodeScanFailsOpenWithinBudget(t *testing.T) {
	lib := hungLibproc{release: make(chan struct{})}
	t.Cleanup(func() { close(lib.release) })
	pinDarwinLibproc(t, lib)
	logs := captureSlog(t)
	holders, elapsed := runDarwinScanGuarded(t, filepath.Join(t.TempDir(), "state.db"))
	if elapsed >= darwinScanHangGuard {
		t.Fatalf("elapsed %v", elapsed)
	}
	if holders == nil || len(holders) != 0 {
		t.Fatalf("a timed-out scan must fail open (no holders, no refusal): %v", holders)
	}
	if len(logLinesWith(logs, "WARN", "timed out")) == 0 {
		t.Fatal("deadline expiry must log a visible warning")
	}
}

func TestHealthyScanStillReportsHolder(t *testing.T) {
	dir := t.TempDir()
	watched := realpathLax(filepath.Join(dir, "state.db-wal"))
	pinDarwinLibproc(t, healthyLibproc{watched, FileIdentity{Dev: 0xDEAD, Ino: 0xBEEF}})
	logs := captureSlog(t)
	holders, elapsed := runDarwinScanGuarded(t, filepath.Join(dir, "state.db"))
	if elapsed >= time.Duration(darwinScanBudget*float64(time.Second)) {
		t.Fatal("a fast scan must not wait out the budget")
	}
	if !slices.Equal(holders, []Holder{{PID: 4242, Target: watched}}) {
		t.Fatalf("holders = %v", holders)
	}
	if len(logLinesWith(logs, "WARN", "timed out")) != 0 {
		t.Fatal("a successful scan must not log a timeout warning")
	}
}

func TestDarwinPathMatchesCaseFoldedButNeedsIdentityMismatch(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	if err := os.WriteFile(db+"-wal", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	live, _ := statFileIdentity(db + "-wal")
	watched := realpathLax(db + "-wal")
	pinDarwinLibproc(t, healthyLibproc{strings.ToUpper(watched), live})
	if holders, _ := runDarwinScanGuarded(t, db); len(holders) != 0 {
		t.Fatalf("the live sidecar itself is no holder: %v", holders)
	}
	pinDarwinLibproc(t, healthyLibproc{strings.ToUpper(watched), FileIdentity{Dev: live.Dev, Ino: live.Ino + 1}})
	holders, _ := runDarwinScanGuarded(t, db)
	if len(holders) != 1 || holders[0].PID != 4242 {
		t.Fatalf("case folded retired generation = %v", holders)
	}
	pinVar(t, &dbfilePlatform, "darwin")
	if n, ok := CountDBHolders(watched); !ok || n != 0 {
		t.Fatalf("darwin count on a mismatched identity = %d %v", n, ok)
	}
	pinDarwinLibproc(t, healthyLibproc{watched, live})
	if n, ok := CountDBHolders(watched); !ok || n != 1 {
		t.Fatalf("darwin count = %d %v", n, ok)
	}
}

func TestDarwinAllPIDsGrowsTheBufferAndDropsNonPositive(t *testing.T) {
	calls := 0
	lib := &listingLibproc{fill: func(buf []byte) int {
		calls++
		if len(buf) == 4096*8 {
			return len(buf)
		}
		for i, pid := range []int32{3, 0, -1, 9} {
			binary.LittleEndian.PutUint32(buf[i*4:], uint32(pid))
		}
		return 16
	}}
	if pids := darwinAllPIDs(lib); !slices.Equal(pids, []int{3, 9}) || calls != 2 {
		t.Fatalf("pids = %v after %d calls", pids, calls)
	}
	if _, err := loadDarwinLibproc(); runtime.GOOS != "darwin" && err == nil {
		t.Fatal("libproc resolved off macOS")
	}
}

type listingLibproc struct{ fill func([]byte) int }

func (l *listingLibproc) ProcListPIDs(_, _ uint32, buf []byte) int { return l.fill(buf) }
func (l *listingLibproc) ProcPIDInfo(int, int, uint64, []byte) int { return 0 }
func (l *listingLibproc) ProcPIDFDInfo(int, int, int, []byte) int  { return 0 }

func TestDeletedSidecarHolderScanFindsOwnRetiredGeneration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs descriptor scan")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	if holders := IterDeletedSQLiteSidecarHolders(db); len(holders) != 0 {
		t.Fatalf("fresh dir holders = %v", holders)
	}
	held, err := os.Create(db + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := RefuseDeletedWALGeneration(db); err != nil {
		t.Fatalf("a still linked sidecar is no retired generation: %v", err)
	}
	if err := os.Remove(db + "-wal"); err != nil {
		t.Fatal(err)
	}
	holders := IterDeletedSQLiteSidecarHolders(db)
	if len(holders) != 1 || holders[0].PID != os.Getpid() || !strings.HasSuffix(holders[0].Target, " (deleted)") {
		t.Fatalf("holders = %v", holders)
	}
	logs := captureSlog(t)
	err = RefuseDeletedWALGeneration(db)
	var deleted *DeletedWalGenerationError
	var replaced *StateDBReplacedError
	if !errors.As(err, &deleted) || !errors.As(err, &replaced) || err.Error() != DeletedWalGenerationMsg {
		t.Fatalf("refusal = %v", err)
	}
	if len(logLinesWith(logs, "ERROR", "FATAL")) == 0 {
		t.Fatal("refusal must log the message at error level")
	}
	if err := os.WriteFile(db+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if holders := IterDeletedSQLiteSidecarHolders(db); len(holders) != 1 {
		t.Fatalf("a newer sidecar at the path must not hide the retired one: %v", holders)
	}
	pinVar(t, &dbfilePlatform, "windows")
	if holders := IterDeletedSQLiteSidecarHolders(db); holders == nil || len(holders) != 0 {
		t.Fatalf("windows holders = %v", holders)
	}
	pinVar(t, &dbfilePlatform, "plan9")
	if holders := IterDeletedSQLiteSidecarHolders(db); len(holders) != 0 {
		t.Fatalf("other host holders = %v", holders)
	}
}

func TestFDTrulyUnlinkedJudgesIdentity(t *testing.T) {
	dir := t.TempDir()
	watched := filepath.Join(dir, "state.db-wal")
	if err := os.WriteFile(watched, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "same")
	if err := os.Symlink(watched, link); err != nil {
		t.Fatal(err)
	}
	if fdIsTrulyUnlinked(link, watched) {
		t.Fatal("the live sidecar is not unlinked")
	}
	if fdIsTrulyUnlinked(filepath.Join(dir, "gone"), watched) {
		t.Fatal("a closed descriptor never counts")
	}
	other := filepath.Join(dir, "backup")
	if err := os.WriteFile(other, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !fdIsTrulyUnlinked(other, watched) {
		t.Fatal("a different inode is a retired generation")
	}
	id, _ := statFileIdentity(other)
	if !identityIsTrulyUnlinked(id, filepath.Join(dir, "missing")) {
		t.Fatal("an unstatable watched path fails closed")
	}
	if runtime.GOOS != "windows" {
		real := filepath.Join(dir, "real")
		os.Mkdir(real, 0o700)
		os.Symlink(real, filepath.Join(dir, "via"))
		watchedMap, err := watchedSQLiteSidecarPaths(filepath.Join(dir, "via", "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		canonical, _ := CanonicalSQLitePath(filepath.Join(realpathLax(real), "state.db-wal"))
		if _, ok := watchedMap[canonical]; !ok || len(watchedMap) != 4 {
			t.Fatalf("resolved spelling missing from %v", watchedMap)
		}
	}
}

// Writes a minimal SQLite style header carrying an application id
func writeSQLiteHeader(t *testing.T, path string, appID uint32) {
	t.Helper()
	header := make([]byte, 100)
	copy(header, "SQLite format 3\x00")
	binary.BigEndian.PutUint16(header[16:], 1)
	binary.BigEndian.PutUint32(header[28:], 7)
	binary.BigEndian.PutUint32(header[stateDBApplicationIDOffset:], appID)
	if err := os.WriteFile(path, header, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPreadDBRangeRetiresTheStaleDescriptor(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	writeSQLiteHeader(t, db, 0x1CEBA7)
	if id, ok := readSQLiteApplicationID(db); !ok || id != 0x1CEBA7 {
		t.Fatalf("application id = %x %v", id, ok)
	}
	retired := len(retiredHeaderProbeFDs)
	next := filepath.Join(dir, "next")
	writeSQLiteHeader(t, next, 42)
	if err := os.Rename(next, db); err != nil {
		t.Fatal(err)
	}
	if id, ok := readSQLiteApplicationID(db); !ok || id != 42 {
		t.Fatalf("replaced file application id = %d %v", id, ok)
	}
	if !isWindows && len(retiredHeaderProbeFDs) != retired+1 {
		t.Fatal("the stale descriptor must be retired, never closed")
	}
	if tail, ok := preadDBRange(db, 1000, 10); !ok || tail == nil || len(tail) != 0 {
		t.Fatalf("read past the end = %v %v", tail, ok)
	}
	os.WriteFile(next, []byte("not sqlite"), 0o600)
	if _, ok := readSQLiteApplicationID(next); ok {
		t.Fatal("non SQLite header has no application id")
	}
	if _, ok := preadDBRange(filepath.Join(dir, "missing"), 0, 4); ok {
		t.Fatal("missing file read")
	}
	pinVar(t, &isWindows, true)
	if head, ok := preadDBHeader(db, 16); !ok || string(head) != "SQLite format 3\x00" {
		t.Fatalf("plain read = %q %v", head, ok)
	}
	parsed := parseSQLiteHeader(append(make([]byte, 0, 100), mustRead(t, db)...))
	if parsed["valid"] != true || parsed["page_size"] != int64(65536) || parsed["page_count"] != int64(7) || parsed["application_id"] != int64(42) {
		t.Fatalf("parsed header = %v", parsed)
	}
	if parseSQLiteHeader([]byte("SQLite format 3\x00"))["valid"] != false {
		t.Fatal("short header parsed as valid")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// SQLite database at path with one table and committed rows
func seedPlainDB(t *testing.T, path string) {
	t.Helper()
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mustExecConn(t, conn, "CREATE TABLE t (x)")
	mustExecConn(t, conn, "INSERT INTO t VALUES (1)")
}

func TestCaptureRetiredWALGenerationCopiesTheHeldInode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("descriptor tables are POSIX")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	seedPlainDB(t, db)
	walBytes := []byte(strings.Repeat("retired-frames", 1000))
	if err := os.WriteFile(db+"-wal", walBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(db + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	walID, _ := StatDBFileIdentity(db + "-wal")
	os.Remove(db + "-wal")
	os.WriteFile(db+"-wal", []byte("newer"), 0o600)
	newer, _ := StatDBFileIdentity(db + "-wal")
	final, err := CaptureRetiredWALGeneration(db, map[string]FileIdentity{"-wal": walID, "-shm": {Dev: 1, Ino: 1}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(final), "state.db.retired-wal-") || !strings.HasSuffix(final, "-"+strconv.Itoa(os.Getpid())) {
		t.Fatalf("artifact dir = %s", final)
	}
	if string(mustRead(t, filepath.Join(final, "state.db-wal"))) != string(walBytes) {
		t.Fatal("captured WAL differs from the retired generation")
	}
	if string(mustRead(t, filepath.Join(final, "state.db"))) != string(mustRead(t, db)) {
		t.Fatal("main image not copied")
	}
	text := string(mustRead(t, filepath.Join(final, RetiredGenerationManifest)))
	for _, want := range []string{`"version": 1`, `"trigger": "test"`, `"shm": null`, `"mode": "copied"`, `"valid": true`,
		`"bytes": ` + strconv.Itoa(len(walBytes)), `"-wal": [`, strconv.Itoa(int(newer.Ino)), "icebath sessions recover"} {
		if !strings.Contains(text, want) {
			t.Fatalf("manifest lacks %s:\n%s", want, text)
		}
	}
	if strings.Index(text, `"captured_at"`) > strings.Index(text, `"database"`) {
		t.Fatal("manifest keys are not sorted")
	}
	if _, err := os.Stat(final + ".partial"); !os.IsNotExist(err) {
		t.Fatal("staging dir left behind")
	}
	second, err := CaptureRetiredWALGeneration(db, map[string]FileIdentity{"-wal": walID}, "again")
	if err != nil || second == final {
		t.Fatalf("second capture = %s %v", second, err)
	}
	pinVar(t, &RetiredGenerationMainImageMaxBytes, 10)
	third, err := CaptureRetiredWALGeneration(db, map[string]FileIdentity{"-wal": walID}, "big")
	if err != nil {
		t.Fatal(err)
	}
	if text := string(mustRead(t, filepath.Join(third, RetiredGenerationManifest))); !strings.Contains(text, `"mode": "header_only"`) ||
		len(mustRead(t, filepath.Join(third, "state.db.header"))) != 100 {
		t.Fatalf("header only capture:\n%s", text)
	}
}

func TestCaptureRetiredWALGenerationRefusesWithoutTheExactGeneration(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	var capture *RetiredGenerationCaptureError
	_, err := CaptureRetiredWALGeneration(db, map[string]FileIdentity{}, "t")
	if !errors.As(err, &capture) || !strings.Contains(err.Error(), "refusing to guess it by pathname") {
		t.Fatalf("no identity = %v", err)
	}
	_, err = CaptureRetiredWALGeneration(db, map[string]FileIdentity{"-wal": {Dev: 1, Ino: 2}}, "t")
	if !errors.As(err, &capture) || !strings.Contains(err.Error(), "no longer holds the retired WAL inode (1, 2) of") {
		t.Fatalf("not held = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	os.WriteFile(db+"-wal", []byte("frames"), 0o600)
	held, _ := os.Open(db + "-wal")
	defer held.Close()
	walID, _ := StatDBFileIdentity(db + "-wal")
	_, err = CaptureRetiredWALGeneration(db, map[string]FileIdentity{"-wal": walID}, "t")
	if !errors.As(err, &capture) || !strings.Contains(err.Error(), "cannot read the main image header of") {
		t.Fatalf("missing main = %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".retired-wal-") {
			t.Fatalf("staging left behind: %s", e.Name())
		}
	}
	short, err := copyRange(func(int64, int) ([]byte, error) { return nil, nil }, filepath.Join(dir, "short"), 5)
	if !errors.As(err, &capture) || short != nil || !strings.Contains(err.Error(), "0 of 5 bytes") {
		t.Fatalf("short read = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "short.part")); !os.IsNotExist(err) {
		t.Fatal("part file left behind")
	}
}

func TestPreopenHeaderPredicatesAndQuarantine(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		body            []byte
		zeroed, invalid bool
	}{
		"empty":   {nil, true, true},
		"nul":     {make([]byte, 100), true, true},
		"garbage": {[]byte("clobbered page zero"), false, true},
	}
	for name, c := range cases {
		path := filepath.Join(dir, name+".db")
		os.WriteFile(path, c.body, 0o600)
		if IsZeroedStateDB(path, 100, false) != c.zeroed || HasInvalidSQLiteHeaderPreopen(path, 100, false) != c.invalid {
			t.Fatalf("%s predicates wrong", name)
		}
	}
	sqliteDB := filepath.Join(dir, "ok.db")
	writeSQLiteHeader(t, sqliteDB, 1)
	if IsZeroedStateDB(sqliteDB, 100, false) || HasInvalidSQLiteHeaderPreopen(sqliteDB, 100, false) || HasInvalidSQLiteHeaderPreopen(dir, 100, false) {
		t.Fatal("a SQLite file or a directory is not invalid")
	}
	live := filepath.Join(dir, "nul.db")
	TrackConnection(live)
	if IsZeroedStateDB(live, 100, false) || !IsZeroedStateDB(live, 100, true) {
		t.Fatal("a live connection blocks the probe unless forced")
	}
	UntrackConnection(live)
	os.WriteFile(live+"-wal", []byte("w"), 0o600)
	dest, err := QuarantineInvalidStateDB(live, false)
	if err != nil || !strings.Contains(filepath.Base(dest), "nul.db.zeroed-") || !strings.HasSuffix(dest, ".bak") {
		t.Fatalf("quarantine = %q %v", dest, err)
	}
	if _, err := os.Stat(dest + "-wal"); err != nil {
		t.Fatal("sidecar not moved with the database")
	}
	if again, err := QuarantineInvalidStateDB(live, true); again != "" || err != nil {
		t.Fatalf("already moved = %q %v", again, err)
	}
	garbage := filepath.Join(dir, "garbage.db")
	if dest, _ := QuarantineInvalidStateDB(garbage, true); !strings.Contains(dest, ".notadb-") {
		t.Fatalf("notadb quarantine = %q", dest)
	}
	if dest, _ := QuarantineInvalidStateDB(sqliteDB, false); dest != "" {
		t.Fatal("a valid database was quarantined")
	}
	acquired, release, err := QuarantineCrossProcessLock(sqliteDB, 1)
	if err != nil || !acquired {
		t.Fatalf("first lock = %v %v", acquired, err)
	}
	contended, release2, err := QuarantineCrossProcessLock(sqliteDB, 0.1)
	if err != nil || contended {
		t.Fatalf("second lock = %v %v", contended, err)
	}
	release2()
	os.WriteFile(garbage, []byte("again"), 0o600)
	logs := captureSlog(t)
	if dest, err := QuarantineInvalidStateDB(garbage, false); !strings.Contains(dest, ".notadb-") || err != nil {
		t.Fatalf("garbage.db has its own lock file: %q %v", dest, err)
	}
	locked := filepath.Join(dir, "ok.db")
	os.WriteFile(locked, nil, 0o600)
	if dest, err := QuarantineInvalidStateDB(locked, false); dest != "" || err != nil {
		t.Fatalf("contended quarantine = %q %v", dest, err)
	}
	if len(logLinesWith(logs, "ERROR", "not acquired within 5s")) != 1 {
		t.Fatal("contended quarantine must log the refusal")
	}
	release()
}

func TestCollectStateDBStatsReadsEveryField(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	missing, err := CollectStateDBStats(db)
	if err != nil || len(missing.Keys) != 16 || missing.Keys[0] != "page_count" || missing.Vals["wal_size_bytes"] != int64(0) || missing.Vals["page_count"] != nil {
		t.Fatalf("missing db stats = %v %v", missing, err)
	}
	conn, err := ConnectSQLite(db, SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{SchemaSQL, FTSSQL, FTSTrigramSQL} {
		mustExecConn(t, conn, script)
	}
	for _, kv := range [][2]string{{"fts_storage_version", "3"}, {"fts_rebuild_high_water", "10"}, {"fts_rebuild_progress", "4"},
		{FTSRebuildDeferralKey, `{"b": 1, "a": 2}`}} {
		if _, err := conn.ExecContext(context.Background(), "INSERT OR REPLACE INTO state_meta (key, value) VALUES (?, ?)", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	stats, err := CollectStateDBStats(db)
	if err != nil {
		t.Fatal(err)
	}
	v := stats.Vals
	if v["page_count"].(int64)*v["page_size"].(int64) != v["logical_size_bytes"] || v["journal_mode"] != "delete" ||
		v["messages"] != int64(0) || v["sessions"] != int64(0) || v["fts_storage_version"] != int64(3) ||
		v["fts_storage_upgrade_needed"] != false || v["fts_teardown_pending"] != false || v["fts_rebuild_pending"] != true ||
		v["fts_rebuild_high_water"] != int64(10) || v["fts_rebuild_progress"] != int64(4) || v["freelist_count"] == nil {
		t.Fatalf("stats = %v", v)
	}
	tables, _ := v["fts_tables"].(interface{ Get(string) (any, bool) })
	if got, _ := tables.Get("messages_fts"); got != true {
		t.Fatalf("fts tables = %v", v["fts_tables"])
	}
	if got, _ := tables.Get("messages_fts_cjk"); got != false {
		t.Fatalf("fts tables = %v", v["fts_tables"])
	}
	if deferral, ok := v["fts_rebuild_deferral"].(interface{ Get(string) (any, bool) }); !ok {
		t.Fatalf("deferral = %v", v["fts_rebuild_deferral"])
	} else if b, _ := deferral.Get("b"); b == nil {
		t.Fatalf("deferral = %v", v["fts_rebuild_deferral"])
	}
	if HasLiveConnection(db) {
		t.Fatal("stats connection still tracked after close")
	}
}

func TestFTSLayoutRegistryAndDrift(t *testing.T) {
	for table, ddl := range map[string]string{"messages_fts": FTSSQL, "messages_fts_trigram": FTSTrigramSQL} {
		want, err := expectedVTableSQL(ddl, table)
		layouts := FTSVTableLayouts[table]
		if err != nil || layouts[len(layouts)-1] != want {
			t.Fatalf("%s registry ends with %q, shipping DDL stores %q (%v)", table, layouts[len(layouts)-1], want, err)
		}
	}
	if got := storedForm("CREATE VIRTUAL TABLE \"messages_fts\" USING fts5( content ,\n tool_name )"); got != "CREATE VIRTUAL TABLE messages_fts USING fts5(content,tool_name)" {
		t.Fatalf("stored form = %q", got)
	}
	ctx := context.Background()
	conn, err := ConnectSQLite(":memory:", SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mustExecConn(t, conn, SchemaSQL)
	mustExecConn(t, conn, FTSSQL)
	if drift, err := FTSLayoutDrift(ctx, conn); err != nil || len(drift) != 0 {
		t.Fatalf("current layout drift = %v %v", drift, err)
	}
	if current, err := FTSLayoutCurrent(ctx, conn); !current || err != nil {
		t.Fatalf("current = %v %v", current, err)
	}
	if stale, err := BaseSourceViewIsStale(ctx, conn); stale || err != nil {
		t.Fatalf("stale = %v %v", stale, err)
	}
	mustExecConn(t, conn, "CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content, tool_name, tool_calls, tokenize='trigram')")
	if drift, _ := FTSLayoutDrift(ctx, conn); !slices.Equal(drift, []string{"messages_fts_trigram"}) {
		t.Fatalf("trigram with tool_calls drift = %v", drift)
	}
	if current, _ := FTSLayoutCurrent(ctx, conn); current {
		t.Fatal("an older trigram is not current")
	}
	if recognised, _ := BaseIndexRecognised(ctx, conn); !recognised {
		t.Fatal("shipping base index not recognised")
	}
	if !BaseIndexRealignedOnOpen("fts5(content,tool_name,content='messages')") || BaseIndexRealignedOnOpen(FTSVTableLayouts["messages_fts"][3]) {
		t.Fatal("realign predicate wrong")
	}
	if !isOlderLayout("messages_fts", "CREATE VIRTUAL TABLE messages_fts USING fts5(content)") || isOlderLayout("messages_fts", "x tool_name y") {
		t.Fatal("older layout predicate wrong")
	}
}

func TestHolderPIDHelpers(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	pids, err := concreteStateDBHolderPIDs(db, []Holder{{PID: 5, Target: db + "-wal (deleted)"}, {PID: 0, Target: db}, {PID: 6, Target: db + ".bak"},
		{PID: 5, Target: db}, {PID: 7, Target: db + "-shm"}})
	if err != nil || !slices.Equal(pids, []int{5, 7}) {
		t.Fatalf("pids = %v %v", pids, err)
	}
	if !isInactiveOrphanDesktopHolder(1, 60, 30, true, []string{"LISTEN"}) || isInactiveOrphanDesktopHolder(2, 60, 30, true, nil) ||
		isInactiveOrphanDesktopHolder(0, 10, 30, true, nil) || isInactiveOrphanDesktopHolder(0, 60, 30, false, nil) ||
		isInactiveOrphanDesktopHolder(0, 60, 30, true, []string{"ESTABLISHED"}) {
		t.Fatal("orphan predicate wrong")
	}
	retain := prepareConnectionRetirement()
	before := len(retainedConnections)
	retain("conn")
	if len(retainedConnections) != before+1 {
		t.Fatal("connection not retained")
	}
	pinVar(t, &dbfilePlatform, "plan9")
	if _, ok := CountDBHolders(db); ok {
		t.Fatal("other hosts have no holder count")
	}
	if runtime.GOOS != "linux" {
		return
	}
	pinVar(t, &dbfilePlatform, "linux")
	seedPlainDB(t, db)
	f, err := os.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if n, ok := CountDBHolders(db); !ok || n != 1 {
		t.Fatalf("holder count = %d %v", n, ok)
	}
}
