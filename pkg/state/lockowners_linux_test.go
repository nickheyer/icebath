package state

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const lockOwnerHelperEnv = "ICEBATH_LOCKOWNER_WRITER_HELPER"

// Child half of the live writer test, holding BEGIN IMMEDIATE with one pending insert until killed
func TestLockOwnerWriterHelperProcess(t *testing.T) {
	path := os.Getenv(lockOwnerHelperEnv)
	if path == "" {
		return
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		os.Exit(2)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		os.Exit(3)
	}
	for _, stmt := range []string{"BEGIN IMMEDIATE", "INSERT INTO t VALUES (1)"} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			os.Exit(4)
		}
	}
	fmt.Println("held")
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// Pins the /proc/locks reader to successive texts for the length of the test
func pinProcLocks(t *testing.T, passes ...string) {
	t.Helper()
	saved := readProcLocks
	t.Cleanup(func() { readProcLocks = saved })
	readProcLocks = func() (string, error) {
		text := passes[0]
		passes = passes[1:]
		return text, nil
	}
}

// A /proc/locks RESERVED line held by this process on the file at path
func reservedLockLine(t *testing.T, path string) string {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("1: POSIX  ADVISORY  WRITE %d %02x:%02x:%d 1073741825 1073741825\n",
		os.Getpid(), unix.Major(st.Dev), unix.Minor(st.Dev), st.Ino)
}

func TestHolderSkippedByOneProcLocksPassIsStillNamed(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pinProcLocks(t, "", reservedLockLine(t, db), "")
	lines, err := StateDBWriteLockHolders(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf("PID %d ", os.Getpid())) || !strings.Contains(lines[0], "RESERVED") {
		t.Fatalf("%q", lines)
	}
	if !strings.HasSuffix(lines[0], " holds RESERVED lock on state.db") {
		t.Fatalf("%q", lines[0])
	}
}

func TestHolderSeenInEveryPassIsNamedOnce(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	line := reservedLockLine(t, db)
	pinProcLocks(t, line+line, line, line)
	lines, err := StateDBWriteLockHolders(db)
	if err != nil || len(lines) != 1 {
		t.Fatalf("%q %v", lines, err)
	}
}

func TestUnreadableProcLocksNamesNoHolder(t *testing.T) {
	saved := readProcLocks
	t.Cleanup(func() { readProcLocks = saved })
	readProcLocks = func() (string, error) { return "", errors.New("unreadable") }
	lines, err := StateDBWriteLockHolders(filepath.Join(t.TempDir(), "state.db"))
	if err != nil || len(lines) != 0 {
		t.Fatalf("%q %v", lines, err)
	}
}

func TestLogWriteLockHoldersNamesHoldersOrTheRelease(t *testing.T) {
	logs := captureSlog(t)
	db := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	line := reservedLockLine(t, db)
	pinProcLocks(t, line, line, line, "", "", "")
	if err := LogWriteLockHolders(db, 60.4); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("state.db write lock unavailable for 60s (state.db): PID %d ", os.Getpid())
	if !strings.Contains(logs.String(), want) || !strings.Contains(logs.String(), "holds RESERVED lock on state.db") {
		t.Fatalf("missing %q in %s", want, logs.String())
	}
	if err := LogWriteLockHolders(db, 2.5); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "state.db write lock unavailable for 2s (state.db): no write-class lock held at the deadline (the holder released just before it)") {
		t.Fatal(logs.String())
	}
}

func TestReadProcLocksDecodesNonASCIIAsReplacement(t *testing.T) {
	text, err := readProcLocks()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range text {
		if r >= 0x80 && r != '�' {
			t.Fatalf("rune %U survived the ASCII decode", r)
		}
	}
}

func TestMakedevOfNODEVMatchesPython(t *testing.T) {
	makedev, err := osMakedev()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		major, minor int64
		want         uint64
	}{{-1, 0, 18446726481524555520}, {0xffffffff, 5, 18446726481524555525}, {0x103, 0x02, unix.Mkdev(0x103, 0x02)}} {
		got, err := makedev(big.NewInt(c.major), big.NewInt(c.minor))
		if err != nil || got != c.want {
			t.Fatalf("makedev(%d, %d) = %d %v want %d", c.major, c.minor, got, err, c.want)
		}
	}
}

func TestLiveWriterInAnotherProcessIsNamedByPID(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	setup, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"PRAGMA journal_mode=wal", "CREATE TABLE t(x)"} {
		if _, err := setup.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	setup.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockOwnerWriterHelperProcess$")
	cmd.Env = append(os.Environ(), lockOwnerHelperEnv+"="+db)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	kill := func() {
		if !killed {
			killed = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	defer kill()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("helper said %q %v", line, err)
	}
	var lines []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if lines, err = StateDBWriteLockHolders(db); err != nil {
			t.Fatal(err)
		}
		if len(lines) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	found := false
	for _, l := range lines {
		if strings.Contains(l, fmt.Sprintf("PID %d ", cmd.Process.Pid)) && strings.Contains(l, "WAL write") {
			found = true
		}
	}
	if !found {
		t.Fatalf("%q", lines)
	}
	kill()
	if lines, err := StateDBWriteLockHolders(db); err != nil || len(lines) != 0 {
		t.Fatalf("after the holder died %q %v", lines, err)
	}
}
