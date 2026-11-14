//go:build unix

package dashboard

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
)

func init() {
	if os.Getenv("ICEBATH_FAKE_GATEWAY_LOCK_HOLDER") != "1" {
		return
	}
	handle, err := os.OpenFile(os.Args[1], os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		os.Exit(2)
	}
	if err := unix.Flock(int(handle.Fd()), unix.LOCK_EX); err != nil {
		os.Exit(3)
	}
	os.Stdout.WriteString("locked\n")
	time.Sleep(300 * time.Second)
	os.Exit(0)
}

// The test binary linked into dir under the icebath name so its command line reads as the CLI
func linkTestBinaryAsIcebath(t *testing.T, dir string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "icebath")
	if err := os.Link(self, target); err == nil {
		return target
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestServeAutoArchiveDefersToALiveGatewayForTheProfile(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	resetAutoArchiveThrottle(t)
	dbPath := filepath.Join(tmp, "state.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "sessions:\n  auto_archive: true\n  min_interval_hours: 0\n"
	if err := os.WriteFile(filepath.Join(tmp, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	opened := func() bool {
		info, err := os.Stat(dbPath)
		return err == nil && info.Size() > 0
	}
	entrypoint := linkTestBinaryAsIcebath(t, tmp)
	holder := exec.Command(entrypoint, filepath.Join(tmp, "gateway.lock"), "gateway", "run")
	holder.Env = append(os.Environ(), "ICEBATH_FAKE_GATEWAY_LOCK_HOLDER=1")
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			_ = holder.Process.Signal(unix.SIGTERM)
			_ = holder.Wait()
		}()
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil || line != "locked\n" {
			t.Fatalf("holder said %q (%v)", line, err)
		}
		if err := os.WriteFile(filepath.Join(tmp, "gateway.pid"), []byte(strconv.Itoa(holder.Process.Pid)), 0o644); err != nil {
			t.Fatal(err)
		}
		if !manage.CheckGatewayRunning(tmp) {
			t.Fatal("probe did not look like a live gateway")
		}
		maybeAutoArchiveForProfile(ctx, nil)
		if opened() {
			t.Fatal("serve opened the store while the gateway owned it")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for manage.CheckGatewayRunning(tmp) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	resetAutoArchiveThrottle(t)

	maybeAutoArchiveForProfile(ctx, nil)

	if !opened() {
		t.Fatal("serve must still archive when no gateway owns the profile")
	}
	db, err := state.NewSessionDB(ctx, dbPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, ok, err := db.GetMeta(ctx, "last_auto_archive"); err != nil || !ok {
		t.Fatalf("archive sweep did not run once (%v)", err)
	}
}
