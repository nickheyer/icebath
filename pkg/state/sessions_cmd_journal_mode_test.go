package state

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const journalHolderHelperEnv = "ICEBATH_JOURNAL_HOLDER_HELPER_DB"

// Child half of the holder test, keeping a connection on the store until a line arrives on stdin
func TestJournalModeHolderHelperProcess(t *testing.T) {
	path := os.Getenv(journalHolderHelperEnv)
	if path == "" {
		return
	}
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
	if err != nil {
		os.Exit(2)
	}
	var n int64
	if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		os.Exit(3)
	}
	fmt.Printf("held:%d\n", os.Getpid())
	bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}

// Creates a three row store in WAL mode and checks its header says so
func walStore(t *testing.T, path string) {
	t.Helper()
	conn := openRaw(t, path)
	execAll(t, conn, "PRAGMA journal_mode=WAL", "CREATE TABLE t(x)", "INSERT INTO t VALUES (1), (2), (3)")
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := headerVersionBytes(t, path); got != [2]byte{2, 2} {
		t.Fatalf("fixture header %v", got)
	}
}

// Header bytes 18 and 19 of a database file, the write and read format versions
func headerVersionBytes(t *testing.T, path string) [2]byte {
	t.Helper()
	data := fileBytes(t, path)
	return [2]byte{data[18], data[19]}
}

// Runs set-journal-mode and returns its exit code with the printed output
func runSetJournalMode(t *testing.T, mode string, db any, force bool) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code, err := CmdSetJournalMode(context.Background(), map[string]any{"sessions_action": "set-journal-mode", "mode": mode, "db": db, "force": force}, &out)
	if err != nil {
		t.Fatalf("set-journal-mode failed %v, output %s", err, out.String())
	}
	return code, out.String()
}

// Repoints the default store at path under a fresh home for the test
func journalDefaultStore(t *testing.T, path string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := DefaultDBPath
	DefaultDBPath = path
	t.Cleanup(func() { DefaultDBPath = saved })
}

func TestSetJournalModeConvertsWALStoreOffline(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	walStore(t, db)
	journalDefaultStore(t, db)
	code, out := runSetJournalMode(t, "delete", nil, true)
	if code != 0 {
		t.Fatalf("code %d output %s", code, out)
	}
	if got := headerVersionBytes(t, db); got != [2]byte{1, 1} {
		t.Fatalf("header must report rollback-journal mode, got %v", got)
	}
	if _, err := os.Lstat(db + "-wal"); err == nil {
		t.Fatal("the wal sidecar survived the switch")
	}
	conn := openRaw(t, db)
	if mode := scalarOf(t, conn, "PRAGMA journal_mode"); strings.ToLower(fmt.Sprint(mode)) != "delete" {
		t.Fatalf("journal mode %v", mode)
	}
	if n := scalarCount(t, conn, "SELECT count(*) FROM t"); n != 3 {
		t.Fatalf("rows %d", n)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wal → delete") {
		t.Fatalf("output %s", out)
	}
}

func TestSetJournalModeRefusesWhileAnotherProcessHoldsTheStore(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "normal"
		if force {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "state.db")
			walStore(t, db)
			journalDefaultStore(t, db)
			holder := exec.Command(os.Args[0], "-test.run=^TestJournalModeHolderHelperProcess$")
			holder.Env = append(os.Environ(), journalHolderHelperEnv+"="+db)
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
			line, readErr := bufio.NewReader(stdout).ReadString('\n')
			marker, pidText, _ := strings.Cut(strings.TrimSpace(line), ":")
			code, out := 0, ""
			if readErr == nil && marker == "held" {
				code, out = runSetJournalMode(t, "delete", nil, force)
			}
			fmt.Fprintln(stdin)
			stdin.Close()
			holder.Wait()
			if readErr != nil || marker != "held" {
				t.Fatalf("holder said %q %v", line, readErr)
			}
			sqlitePID, err := strconv.Atoi(pidText)
			if err != nil {
				t.Fatal(err)
			}
			if code != 1 {
				t.Fatalf("a found holder must refuse, code %d output %s", code, out)
			}
			if !strings.Contains(out, fmt.Sprintf("pid %d", sqlitePID)) {
				t.Fatalf("output does not name the holder %d: %s", sqlitePID, out)
			}
			if got := headerVersionBytes(t, db); got != [2]byte{2, 2} {
				t.Fatalf("a refused switch must leave the file untouched, header %v", got)
			}
			saved := foreignStateDBHoldersOf
			foreignStateDBHoldersOf = func(string) ([]Holder, error) { return []Holder{{PID: -1, Target: "scan failed"}}, nil }
			t.Cleanup(func() { foreignStateDBHoldersOf = saved })
			wantCode, wantHeader := 1, [2]byte{2, 2}
			if force {
				wantCode, wantHeader = 0, [2]byte{1, 1}
			}
			code, out = runSetJournalMode(t, "delete", nil, force)
			if code != wantCode {
				t.Fatalf("failed scan code %d, want %d, output %s", code, wantCode, out)
			}
			if got := headerVersionBytes(t, db); got != wantHeader {
				t.Fatalf("header %v, want %v", got, wantHeader)
			}
			if strings.Contains(out, "scan: scan failed") == force {
				t.Fatalf("scan refusal line presence wrong for force=%v: %s", force, out)
			}
		})
	}
}

func TestRefusalAdmissionInvariants(t *testing.T) {
	cases := []struct {
		target, current string
		crossVM         bool
		expected        string
	}{
		{"delete", "wal", false, ""},
		{"wal", "delete", true, "cross-VM filesystems"},
		{"delete", "wal", true, ""},
		{"delete", "not-a-database", false, "not an Icebath SQLite store"},
		{"delete", "unknown(3/3)", false, "not an Icebath SQLite store"},
	}
	for _, c := range cases {
		reason := journalModeRefusal(c.target, c.current, c.crossVM)
		if c.expected == "" && reason != "" {
			t.Fatalf("%+v refused with %q", c, reason)
		}
		if c.expected != "" && !strings.Contains(reason, c.expected) {
			t.Fatalf("%+v reason %q", c, reason)
		}
	}
}

func TestSetJournalModeReportsUnusableDBPathsWithoutATraceback(t *testing.T) {
	for _, kind := range []string{"garbage-file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ICEBATH_HOME", dir)
			bad := filepath.Join(dir, "a-directory")
			if kind == "garbage-file" {
				bad = filepath.Join(dir, "notes.txt")
				if err := os.WriteFile(bad, []byte("this is not a database, not even close\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(bad, 0o755); err != nil {
				t.Fatal(err)
			}
			code, out := runSetJournalMode(t, "delete", bad, true)
			if code != 1 || !strings.Contains(out, "✗") {
				t.Fatalf("code %d output %s", code, out)
			}
		})
	}
}
