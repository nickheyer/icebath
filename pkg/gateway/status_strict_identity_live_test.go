package gateway

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const lockHolderEnv = "ICEBATH_GATEWAY_TEST_LOCK_HOLDER"

func TestMain(m *testing.M) {
	if os.Getenv(lockHolderEnv) == "1" {
		os.Exit(runGatewayLockHolder())
	}
	if spec := os.Getenv(mallocArenaChildEnv); spec != "" && mallocArenaChild != nil {
		os.Exit(mallocArenaChild(spec))
	}
	os.Exit(m.Run())
}

// Takes the runtime lock, writes the pid file, prints the pid and holds both until stdin closes
func runGatewayLockHolder() int {
	if !AcquireGatewayRuntimeLock() {
		fmt.Fprintln(os.Stderr, "could not acquire gateway.lock")
		return 1
	}
	if err := WritePidFile(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(os.Getpid())
	io.Copy(io.Discard, os.Stdin)
	return 0
}

type lockHolder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	pid   int
}

// Kills the holder, closes its stdin and reaps it
func (h *lockHolder) stop() {
	h.stdin.Close()
	h.cmd.Process.Kill()
	h.cmd.Wait()
}

// Real child process holding the home gateway lock, launched as icebath gateway run or as a plain holder
func spawnLockHolder(t *testing.T, binDir, home string, asGateway bool) *lockHolder {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name, args := "holder", []string{}
	if asGateway {
		name, args = "icebath", []string{"gateway", "run"}
	}
	script := filepath.Join(binDir, name)
	if err := os.Symlink(exe, script); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, args...)
	cmd.Env = append(os.Environ(), "ICEBATH_HOME="+home, lockHolderEnv+"=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
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
	holder := &lockHolder{cmd: cmd, stdin: stdin}
	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		got <- strings.TrimSpace(line)
	}()
	var line string
	select {
	case line = <-got:
	case <-time.After(20 * time.Second):
	}
	pid, err := strconv.Atoi(line)
	if err != nil {
		holder.stop()
		t.Fatalf("lock holder failed to start: %q %s", line, stderr.String())
	}
	holder.pid = pid
	return holder
}

func makeIdentityHome(t *testing.T, root string) string {
	t.Helper()
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(home, "config.yaml"), "{}\n")
	return home
}

// Pid of a process that already exited, a leftover pid file rather than a rival gateway
func exitedPid(t *testing.T) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	pid := child.ProcessState.Pid()
	deadline := time.Now().Add(10 * time.Second)
	for statusPidExists(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("exited child %d never left the process table", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return pid
}

// Rewrites the pid file as a replaced gateway left it, its own pid and an earlier start
func writePreviousGatewayPidFile(t *testing.T, home string, pid int) {
	t.Helper()
	path := filepath.Join(home, "gateway.pid")
	record := readRecord(t, path)
	record.Set("pid", int64(pid))
	raw, _ := record.Get("start_time")
	var start int64
	switch v := raw.(type) {
	case int64:
		start = v
	case float64:
		start = int64(v)
	default:
		t.Fatalf("start_time %T", raw)
	}
	record.Set("start_time", max(int64(1), start-500))
	writeRecord(t, path, record)
}

func strictIdentity(t *testing.T, home string) (int, float64, bool, error) {
	t.Helper()
	clearRunningPidCache()
	return GetRunningPidIdentityStrict(filepath.Join(home, "gateway.pid"))
}

func mustStrictError(t *testing.T, home, fragment string) {
	t.Helper()
	_, _, ok, err := strictIdentity(t, home)
	if err == nil || ok || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("want error matching %q, got ok=%v err=%v", fragment, ok, err)
	}
}

func TestStrictIdentityWithLiveLock(t *testing.T) {
	root := t.TempDir()
	home := makeIdentityHome(t, root)
	holder := spawnLockHolder(t, filepath.Join(root, "bin"), home, true)
	defer holder.stop()
	pidPath, lockPath := filepath.Join(home, "gateway.pid"), filepath.Join(home, "gateway.lock")
	run := func(name string, body func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", home)
			pidBytes, err := os.ReadFile(pidPath)
			if err != nil {
				t.Fatal(err)
			}
			lockBytes, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				os.WriteFile(pidPath, pidBytes, 0o644)
				f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				f.Write(lockBytes)
				f.Close()
				clearRunningPidCache()
			}()
			body(t)
		})
	}

	run("missing_pid_file_resolves_from_the_lock_record", func(t *testing.T) {
		wpid, wstart, wok, werr := strictIdentity(t, home)
		os.Remove(pidPath)
		pid, start, ok, err := strictIdentity(t, home)
		if err != nil || !ok || pid != holder.pid {
			t.Fatalf("identity %d %v %v, holder %d", pid, ok, err, holder.pid)
		}
		if werr != nil || !wok || wpid != pid || wstart != start {
			t.Fatalf("with pid file %d %v %v %v, without %d %v", wpid, wstart, wok, werr, pid, start)
		}
	})

	run("pid_file_that_disagrees_with_the_lock_still_aborts", func(t *testing.T) {
		record := readRecord(t, pidPath)
		record.Set("pid", int64(holder.pid+1))
		writeRecord(t, pidPath, record)
		mustStrictError(t, home, "disagree")
	})

	run("stale_pid_file_beside_a_held_lock_resolves_from_the_lock_record", func(t *testing.T) {
		writePreviousGatewayPidFile(t, home, exitedPid(t))
		pid, _, ok, err := strictIdentity(t, home)
		if err != nil || !ok || pid != holder.pid {
			t.Fatalf("identity %d %v %v, holder %d", pid, ok, err, holder.pid)
		}
	})

	run("previous_gateway_pid_recycled_by_a_live_process_resolves_from_the_lock", func(t *testing.T) {
		writePreviousGatewayPidFile(t, home, os.Getpid())
		pid, _, ok, err := strictIdentity(t, home)
		if err != nil || !ok || pid != holder.pid {
			t.Fatalf("identity %d %v %v, holder %d", pid, ok, err, holder.pid)
		}
	})

	run("malformed_pid_file_beside_a_held_lock_still_aborts", func(t *testing.T) {
		writeBytes(t, pidPath, "not a record")
		mustStrictError(t, home, "malformed")
	})

	run("garbled_lock_record_without_pid_file_still_aborts", func(t *testing.T) {
		os.Remove(pidPath)
		f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte("not a record" + strings.Repeat(" ", 100)))
		f.Close()
		mustStrictError(t, home, "malformed")
	})
}

func TestLockHolderThatIsNotAGatewayStillAborts(t *testing.T) {
	root := t.TempDir()
	home := makeIdentityHome(t, root)
	t.Setenv("ICEBATH_HOME", home)
	holder := spawnLockHolder(t, filepath.Join(root, "bin"), home, false)
	defer holder.stop()
	os.Remove(filepath.Join(home, "gateway.pid"))
	mustStrictError(t, home, "does not identify a live gateway")
}
