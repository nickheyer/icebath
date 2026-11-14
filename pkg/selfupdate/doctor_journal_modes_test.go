package selfupdate

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const journalExposedText = "exposed to the WAL-reset bug"

var (
	journalVulnerable    = state.NewVersionTuple(3, 50, 4)
	journalFixedVersions = []state.VersionTuple{state.NewVersionTuple(3, 51, 3), state.NewVersionTuple(3, 52, 0), state.NewVersionTuple(3, 50, 7), state.NewVersionTuple(3, 44, 6)}
)

// Creates a one table database, in the given journal mode when one is named
func journalMakeDB(t *testing.T, path, journalMode string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	if journalMode != "" {
		rows, err := conn.QueryContext(ctx, "PRAGMA journal_mode="+journalMode)
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
}

// Names of the WAL and shared memory sidecars in a directory, sorted
func journalSidecars(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "-wal") || strings.HasSuffix(entry.Name(), "-shm") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Journal mode report of a home for a SQLite version, as printed
func journalReport(t *testing.T, icebathHome string, version state.VersionTuple) string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", icebathHome)
	var out bytes.Buffer
	d := &Doctor{W: &out, Home: icebathHome, ProjectRoot: t.TempDir(), DisplayHome: icebathHome}
	if err := doctorReportDatabaseJournalModes(context.Background(), d, icebathHome, &version); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Asserts the journal mode probe answers a mode and no error
func journalExpectMode(t *testing.T, path, want string) {
	t.Helper()
	if mode, reason := doctorReadJournalMode(path); mode != want || reason != "" {
		t.Fatalf("mode = %q, reason = %q", mode, reason)
	}
}

func TestReadJournalModeReadsWal(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	journalMakeDB(t, db, "WAL")
	journalExpectMode(t, db, "wal")
}

func TestReadJournalModeReadsRollback(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	journalMakeDB(t, db, "")
	journalExpectMode(t, db, "rollback")
}

func TestReadJournalModeProbeCreatesNoWalSidecars(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	journalMakeDB(t, db, "WAL")
	if got := journalSidecars(t, dir); len(got) != 0 {
		t.Fatalf("sidecars before = %q", got)
	}
	journalExpectMode(t, db, "wal")
	if got := journalSidecars(t, dir); len(got) != 0 {
		t.Fatalf("sidecars after = %q", got)
	}
}

func TestReadJournalModeMissingFileReportsErrorAndDoesNotCreateIt(t *testing.T) {
	db := filepath.Join(t.TempDir(), "missing.db")
	mode, reason := doctorReadJournalMode(db)
	if mode != "" || reason == "" {
		t.Fatalf("mode = %q, reason = %q", mode, reason)
	}
	if _, err := os.Lstat(db); err == nil {
		t.Fatal("probe created the file")
	}
}

func TestReadJournalModeEmptyFileReportsError(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	writeTestFile(t, db, "", 0o644)
	if mode, reason := doctorReadJournalMode(db); mode != "" || reason != "file is empty" {
		t.Fatalf("mode = %q, reason = %q", mode, reason)
	}
}

func TestReadJournalModeCorruptFileReportsError(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	writeTestFile(t, db, strings.Repeat("this is not a sqlite database", 4), 0o644)
	if mode, reason := doctorReadJournalMode(db); mode != "" || !strings.Contains(reason, "not a database") {
		t.Fatalf("mode = %q, reason = %q", mode, reason)
	}
}

func TestReadJournalModeReadOnlyDirectoryIsStillReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	journalMakeDB(t, db, "WAL")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	mode, reason := doctorReadJournalMode(db)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" || reason != "" {
		t.Fatalf("mode = %q, reason = %q", mode, reason)
	}
	if got := journalSidecars(t, dir); len(got) != 0 {
		t.Fatalf("sidecars = %q", got)
	}
}

func TestReadJournalModeDoesNotMutateDatabaseFiles(t *testing.T) {
	dir := t.TempDir()
	walDB, rollbackDB := filepath.Join(dir, "wal.db"), filepath.Join(dir, "plain.db")
	journalMakeDB(t, walDB, "WAL")
	journalMakeDB(t, rollbackDB, "")
	walBytes, _ := os.ReadFile(walDB)
	rollbackBytes, _ := os.ReadFile(rollbackDB)
	journalExpectMode(t, walDB, "wal")
	journalExpectMode(t, rollbackDB, "rollback")
	afterWal, _ := os.ReadFile(walDB)
	afterRollback, _ := os.ReadFile(rollbackDB)
	if !bytes.Equal(walBytes, afterWal) || !bytes.Equal(rollbackBytes, afterRollback) {
		t.Fatal("probe changed a database file")
	}
	if got := journalSidecars(t, dir); len(got) != 0 {
		t.Fatalf("sidecars = %q", got)
	}
}

func TestLiveConnectionSafetyProbeIsRefusedForARealTrackedConnection(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	journalMakeDB(t, db, "WAL")
	conn, err := state.ConnectTracked(db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !state.HasLiveConnection(db) {
		t.Fatal("connection is not tracked")
	}
	if mode, reason := doctorReadJournalMode(db); mode != "" || reason != "database is open in this process" {
		t.Fatalf("mode = %q, reason = %q", mode, reason)
	}
}

func TestLiveConnectionSafetyProbeResumesOnceTheConnectionCloses(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	journalMakeDB(t, db, "WAL")
	conn, err := state.ConnectTracked(db)
	if err != nil {
		t.Fatal(err)
	}
	if mode, _ := doctorReadJournalMode(db); mode != "" {
		t.Fatalf("mode = %q while tracked", mode)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if state.HasLiveConnection(db) {
		t.Fatal("connection is still tracked")
	}
	journalExpectMode(t, db, "wal")
}

func TestLiveConnectionSafetyRefusalCreatesNoNewSidecars(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	journalMakeDB(t, db, "WAL")
	conn, err := state.ConnectTracked(db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	before := journalSidecars(t, dir)
	doctorReadJournalMode(db)
	if after := journalSidecars(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("sidecars %q became %q", before, after)
	}
}

func TestLiveConnectionSafetyReportDegradesInsteadOfProbingALiveDatabase(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	journalMakeDB(t, db, "WAL")
	conn, err := state.ConnectTracked(db)
	if err != nil {
		t.Fatal(err)
	}
	out := journalReport(t, dir, journalVulnerable)
	conn.Close()
	for _, want := range []string{"state.db: journal mode could not be read", "database is open in this process", "cannot rule out WAL exposure"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestLiveConnectionSafetyAnUntrackedLockHolderDoesNotBlockTheProbe(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	journalMakeDB(t, db, "")
	holder, err := state.ConnectSQLite(db, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	journalExpectMode(t, db, "rollback")
}

func TestUnreadableReasonMissingFileKeepsTheOsErrorText(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux error text")
	}
	if reason := doctorUnreadableReason(filepath.Join(t.TempDir(), "gone.db")); !strings.Contains(reason, "No such file or directory") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestUnreadableReasonUnreadableFileIsReportedAsPermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	db := filepath.Join(t.TempDir(), "state.db")
	journalMakeDB(t, db, "")
	if err := os.Chmod(db, 0o000); err != nil {
		t.Fatal(err)
	}
	mode, reason := doctorReadJournalMode(db)
	if err := os.Chmod(db, 0o644); err != nil {
		t.Fatal(err)
	}
	if mode != "" || !strings.Contains(strings.ToLower(reason), "permission denied") {
		t.Fatalf("mode = %q, reason = %q", mode, reason)
	}
}

func TestUnreadableReasonReasonDoesNotOpenTheFile(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	journalMakeDB(t, db, "")
	before, _ := os.Stat(db)
	if reason := doctorUnreadableReason(db); reason != "file could not be read" {
		t.Fatalf("reason = %q", reason)
	}
	if after, _ := os.Stat(db); !after.ModTime().Equal(before.ModTime()) || len(journalSidecars(t, dir)) != 0 {
		t.Fatal("reason probe touched the database")
	}
}

func TestReportDatabaseJournalModesWalDbOnCrossVmFsIsFlaggedWithOfflineRemedy(t *testing.T) {
	dir := t.TempDir()
	journalMakeDB(t, filepath.Join(dir, "state.db"), "WAL")
	toolsSeam(t, &doctorPathOnCrossVMFS, func(string) (bool, error) { return true, nil })
	out := journalReport(t, dir, state.NewVersionTuple(3, 51, 3))
	if !strings.Contains(out, "state.db is in WAL mode on a cross-VM filesystem") || !strings.Contains(out, "icebath sessions set-journal-mode delete") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestReportDatabaseJournalModesVulnerableRuntimeWalDbIsExposed(t *testing.T) {
	dir := t.TempDir()
	journalMakeDB(t, filepath.Join(dir, "state.db"), "WAL")
	out := journalReport(t, dir, journalVulnerable)
	if !strings.Contains(out, "state.db is in WAL mode") || !strings.Contains(out, journalExposedText) {
		t.Fatalf("output:\n%s", out)
	}
}

func TestReportDatabaseJournalModesVulnerableRuntimeRollbackDbIsListedNotExposed(t *testing.T) {
	dir := t.TempDir()
	journalMakeDB(t, filepath.Join(dir, "state.db"), "")
	out := journalReport(t, dir, journalVulnerable)
	if !strings.Contains(out, "state.db: rollback journal mode") || strings.Contains(out, journalExposedText) {
		t.Fatalf("output:\n%s", out)
	}
}

func TestReportDatabaseJournalModesFixedRuntimeWalDbIsNotExposed(t *testing.T) {
	for _, version := range journalFixedVersions {
		t.Run(fmt.Sprint(version[0], ".", version[1], ".", version[2]), func(t *testing.T) {
			dir := t.TempDir()
			journalMakeDB(t, filepath.Join(dir, "state.db"), "WAL")
			out := journalReport(t, dir, version)
			if !strings.Contains(out, "state.db: WAL journal mode") || strings.Contains(out, journalExposedText) || strings.Contains(out, "⚠") {
				t.Fatalf("output:\n%s", out)
			}
		})
	}
}

func TestReportDatabaseJournalModesListsEveryManagedDatabase(t *testing.T) {
	dir := t.TempDir()
	journalMakeDB(t, filepath.Join(dir, "state.db"), "WAL")
	journalMakeDB(t, filepath.Join(dir, "projects.db"), "")
	journalMakeDB(t, filepath.Join(dir, "kanban.db"), "")
	boardRel := filepath.Join("kanban", "boards", "myboard", "kanban.db")
	journalMakeDB(t, filepath.Join(dir, boardRel), "WAL")
	out := journalReport(t, dir, journalVulnerable)
	for _, want := range []string{"state.db is in WAL mode", "projects.db: rollback journal mode", "kanban.db: rollback journal mode", boardRel + " is in WAL mode"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestReportDatabaseJournalModesMissingDatabasesAreSkipped(t *testing.T) {
	out := journalReport(t, t.TempDir(), journalVulnerable)
	if strings.Contains(out, "state.db") || strings.Contains(out, journalExposedText) {
		t.Fatalf("output:\n%s", out)
	}
}

func TestReportDatabaseJournalModesLockedDatabaseDoesNotCrashOrBlock(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	journalMakeDB(t, db, "")
	holder, err := state.ConnectSQLite(db, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	if out := journalReport(t, dir, journalVulnerable); !strings.Contains(out, "state.db: rollback journal mode") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestReportDatabaseJournalModesUnreadableDatabaseDoesNotCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	journalMakeDB(t, db, "")
	if err := os.Chmod(db, 0o000); err != nil {
		t.Fatal(err)
	}
	out := journalReport(t, dir, journalVulnerable)
	if err := os.Chmod(db, 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "state.db: journal mode could not be read") || !strings.Contains(out, "cannot rule out WAL exposure") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestReportDatabaseJournalModesCorruptDatabaseDoesNotCrash(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "state.db"), strings.Repeat("garbage bytes, not sqlite", 8), 0o644)
	if out := journalReport(t, dir, journalVulnerable); !strings.Contains(out, "state.db: journal mode could not be read") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestReportDatabaseJournalModesReadErrorIsInformationalOnFixedRuntime(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "state.db"), strings.Repeat("garbage bytes, not sqlite", 8), 0o644)
	out := journalReport(t, dir, state.NewVersionTuple(3, 51, 3))
	if !strings.Contains(out, "state.db: journal mode could not be read") || strings.Contains(out, "cannot rule out WAL exposure") || strings.Contains(out, "⚠") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestReportDatabaseJournalModesReportCreatesNoWalSidecars(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	journalMakeDB(t, db, "WAL")
	before, _ := os.ReadFile(db)
	journalReport(t, dir, journalVulnerable)
	after, _ := os.ReadFile(db)
	if got := journalSidecars(t, dir); len(got) != 0 || !bytes.Equal(before, after) {
		t.Fatalf("sidecars = %q, changed = %v", got, !bytes.Equal(before, after))
	}
}

func TestConfiguredDeleteNeverAppliedWalDbUnderConfiguredDeleteWarns(t *testing.T) {
	for _, tc := range []struct {
		version state.VersionTuple
		exposed bool
	}{{state.NewVersionTuple(3, 51, 3), false}, {journalVulnerable, true}} {
		t.Run(fmt.Sprint(tc.exposed), func(t *testing.T) {
			dir := t.TempDir()
			journalMakeDB(t, filepath.Join(dir, "state.db"), "WAL")
			toolsSeam(t, &doctorResolveJournalMode, func(context.Context) string { return "delete" })
			out := journalReport(t, dir, tc.version)
			for _, want := range []string{"state.db is in WAL mode", "despite database.journal_mode=delete", "never live-downgraded", "icebath sessions set-journal-mode delete"} {
				if !strings.Contains(out, want) {
					t.Fatalf("output lacks %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "state.db: WAL journal mode") || strings.Contains(out, "To clear the exposure:") != tc.exposed {
				t.Fatalf("output:\n%s", out)
			}
		})
	}
}

const journalHolderEnv = "ICEBATH_DOCTOR_JOURNAL_HOLDER_DB"

// Child stand in that holds a database open, reports its pid and waits for a line on stdin
func TestJournalHolderChild(t *testing.T) {
	db := os.Getenv(journalHolderEnv)
	if db == "" {
		return
	}
	conn, err := state.ConnectSQLite(db, state.SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var count int64
	if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("held:%d\n", os.Getpid())
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestConfiguredDeleteNeverAppliedWalDbUnderConfiguredDeleteNamesItsHolders(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	journalMakeDB(t, db, "WAL")
	toolsSeam(t, &doctorResolveJournalMode, func(context.Context) string { return "delete" })
	holder := exec.Command(os.Args[0], "-test.run=^TestJournalHolderChild$", "-test.v=false")
	holder.Env = append(os.Environ(), journalHolderEnv+"="+db)
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	marker, sqlitePID, _ := strings.Cut(strings.TrimSpace(line), ":")
	var out string
	if marker == "held" {
		out = journalReport(t, dir, state.NewVersionTuple(3, 51, 3))
	}
	fmt.Fprintln(stdin)
	stdin.Close()
	holder.Wait()
	if marker != "held" {
		t.Fatalf("holder said %q", line)
	}
	_, afterHeld, found := strings.Cut(out, "held by PID")
	if !strings.Contains(out, "state.db is held by PID "+sqlitePID) || !found || !strings.Contains(afterHeld, "state.db") {
		t.Fatalf("output:\n%s", out)
	}
	if strings.Contains(out, "no other process holds it") || strings.Contains(out, "cannot prove") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestConfiguredDeleteNeverAppliedPartialHolderScanIsNeverAnAllClear(t *testing.T) {
	dir := t.TempDir()
	journalMakeDB(t, filepath.Join(dir, "state.db"), "WAL")
	toolsSeam(t, &doctorResolveJournalMode, func(context.Context) string { return "delete" })
	toolsSeam(t, &doctorForeignHolders, func(string) ([]state.Holder, error) {
		return []state.Holder{{PID: -1, Target: "open-file scan unavailable"}}, nil
	})
	out := journalReport(t, dir, state.NewVersionTuple(3, 51, 3))
	if !strings.Contains(out, "cannot prove the database is quiet") || !strings.Contains(out, "open-file scan unavailable") ||
		strings.Contains(out, "no other process holds it") || strings.Contains(out, "held by PID") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestConfiguredDeleteNeverAppliedConfiguredWalKeepsTheInformationalLine(t *testing.T) {
	dir := t.TempDir()
	journalMakeDB(t, filepath.Join(dir, "state.db"), "WAL")
	toolsSeam(t, &doctorResolveJournalMode, func(context.Context) string { return "wal" })
	out := journalReport(t, dir, state.NewVersionTuple(3, 51, 3))
	if !strings.Contains(out, "state.db: WAL journal mode") || strings.Contains(out, "despite") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSizeAndRepairHintExposedDatabasesReportSizeAndRepairHint(t *testing.T) {
	dir := t.TempDir()
	journalMakeDB(t, filepath.Join(dir, "state.db"), "WAL")
	out := journalReport(t, dir, journalVulnerable)
	if !regexp.MustCompile(`\(\d[\d.]* [KMGT]?B\)`).MatchString(out) || !strings.Contains(out, "To clear the exposure:") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSizeAndRepairHintNoRepairHintWhenNothingIsExposed(t *testing.T) {
	dir := t.TempDir()
	journalMakeDB(t, filepath.Join(dir, "state.db"), "DELETE")
	if out := journalReport(t, dir, journalVulnerable); strings.Contains(out, "To clear the exposure:") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSizeAndRepairHintNoRepairHintOnAFixedRuntime(t *testing.T) {
	dir := t.TempDir()
	journalMakeDB(t, filepath.Join(dir, "state.db"), "WAL")
	if out := journalReport(t, dir, journalFixedVersions[0]); strings.Contains(out, "To clear the exposure:") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSizeAndRepairHintSizeFailureDoesNotCrash(t *testing.T) {
	if size := doctorFormatDBSize(filepath.Join(t.TempDir(), "gone.db")); size != "size unknown" {
		t.Fatalf("size = %q", size)
	}
}
