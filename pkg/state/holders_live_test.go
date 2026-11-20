package state

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	holderHelperEnv     = "ICEBATH_HOLDER_HELPER"
	holderHelperPathEnv = "ICEBATH_HOLDER_HELPER_PATH"
)

// Child half of the live holder tests, holding a SQLite connection or a plain handle until stdin delivers a line
func TestHolderHelperProcess(t *testing.T) {
	mode := os.Getenv(holderHelperEnv)
	if mode == "" {
		return
	}
	path := os.Getenv(holderHelperPathEnv)
	switch mode {
	case "sqlite":
		db, err := sql.Open("sqlite", path)
		if err != nil {
			os.Exit(2)
		}
		var n int
		if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
			os.Exit(3)
		}
		defer db.Close()
	case "file":
		f, err := os.Open(path)
		if err != nil {
			os.Exit(4)
		}
		defer f.Close()
	}
	fmt.Printf("held:%d\n", os.Getpid())
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}

// Starts a helper holding path in mode and returns its pid with the release function that ends it
func startHolder(t *testing.T, mode, path string) (int, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHolderHelperProcess$")
	cmd.Env = append(os.Environ(), holderHelperEnv+"="+mode, holderHelperPathEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_, _ = io.WriteString(stdin, "\n")
		stdin.Close()
		_ = cmd.Wait()
	}
	t.Cleanup(release)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	marker, pidText, _ := strings.Cut(strings.TrimSpace(line), ":")
	if err != nil || marker != "held" {
		t.Fatalf("helper line %q err %v", line, err)
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatal(err)
	}
	return pid, release
}

func holdsPID(holders []Holder, pid int) bool {
	for _, h := range holders {
		if h.PID == pid {
			return true
		}
	}
	return false
}

func TestScanFindsRealForeignStateDBHolder(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, "CREATE TABLE t(x)")
	conn.Close()

	pid, release := startHolder(t, "sqlite", db)
	holders, err := ForeignStateDBHolders(db)
	if err != nil || !holdsPID(holders, pid) {
		t.Fatalf("holders = %v err = %v, want pid %d", holders, err, pid)
	}
	refusal, held, err := HeldStoreRefusal(context.Background(), db, "optimize-storage", "--force")
	if err != nil || !held || !strings.Contains(refusal, fmt.Sprintf("PID %d", pid)) {
		t.Fatalf("refusal = %q held = %v err = %v", refusal, held, err)
	}
	release()
	if holders, err := ForeignStateDBHolders(db); err != nil || len(holders) != 0 {
		t.Fatalf("after release holders = %v err = %v", holders, err)
	}
}

func TestScanFindsForeignSQLiteSidecarHolder(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "state.db")
			if err := os.WriteFile(db, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			sidecar := db + suffix
			if err := os.WriteFile(sidecar, []byte("sidecar sentinel"), 0o644); err != nil {
				t.Fatal(err)
			}
			pid, release := startHolder(t, "file", sidecar)
			holders, err := ForeignStateDBHolders(db)
			if err != nil || !holdsPID(holders, pid) {
				t.Fatalf("holders = %v err = %v, want pid %d", holders, err, pid)
			}
			release()
			if holders, err := ForeignStateDBHolders(db); err != nil || len(holders) != 0 {
				t.Fatalf("after release holders = %v err = %v", holders, err)
			}
		})
	}
}
