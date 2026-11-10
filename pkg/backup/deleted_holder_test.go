package backup

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyzip"
)

const dbHolderChildEnv = "ICEBATH_BACKUP_TEST_DB_HOLDER"

// Child mode holding a SQLite connection to db open after one read, announcing it and sleeping
func holdDatabase(db string) {
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		os.Exit(2)
	}
	conn.SetMaxOpenConns(1)
	var n int
	if err := conn.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString("held\n")
	time.Sleep(300 * time.Second)
	os.Exit(0)
}

func makeHolderDB(t *testing.T, path, marker string) {
	t.Helper()
	sqliteExec(t, path, "CREATE TABLE t (v TEXT)", "INSERT INTO t VALUES ('"+marker+"')")
}

func TestImportRefusesToPublishOverADeletedButHeldDatabase(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the /proc holder scan is Linux-only")
	}
	tmp := t.TempDir()
	target := filepath.Join(tmp, "state.db")
	makeHolderDB(t, target, "live")
	archive := filepath.Join(tmp, "backup.zip")
	payload := filepath.Join(tmp, "archived.db")
	makeHolderDB(t, payload, "from-archive")
	writeZipFiles(t, archive, [2]string{payload, "hermes/state.db"})

	holder := exec.Command(os.Args[0], "-test.run=^$")
	holder.Env = append(os.Environ(), dbHolderChildEnv+"="+target)
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	func() {
		defer func() {
			holder.Process.Kill()
			holder.Wait()
		}()
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		if strings.TrimSpace(line) != "held" {
			t.Fatalf("holder said %q", line)
		}
		os.Remove(target)
		if pids, ok := foreignDBHolderPIDs(target); !ok || len(pids) != 1 || pids[0] != holder.Process.Pid {
			t.Fatalf("holders %v %v, want [%d]", pids, ok, holder.Process.Pid)
		}
		zr, err := pyzip.OpenReader(archive)
		if err != nil {
			t.Fatal(err)
		}
		err = importDBMember(ctx, zr, "hermes/state.db", target, 0, false)
		zr.Close()
		var oe *osError
		if !errors.As(err, &oe) || !strings.Contains(err.Error(), strconv.Itoa(holder.Process.Pid)) {
			t.Fatalf("import error %v", err)
		}
		if exists(target) {
			t.Fatal("a new inode was published under a live holder")
		}
	}()
	zr, err := pyzip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if err := importDBMember(ctx, zr, "hermes/state.db", target, 0, false); err != nil {
		t.Fatal(err)
	}
	if got := tableRows(t, target, "SELECT v FROM t"); len(got) != 1 || got[0] != "from-archive" {
		t.Fatalf("rows %v", got)
	}
}
