//go:build linux

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Pids of the process and every descendant it has now
func processFamily(pid int) map[int]bool {
	family := map[int]bool{pid: true}
	out, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=").Output()
	if err != nil {
		return family
	}
	parents := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			child, _ := strconv.Atoi(fields[0])
			parents[child], _ = strconv.Atoi(fields[1])
		}
	}
	for grew := true; grew; {
		grew = false
		for child, parent := range parents {
			if family[parent] && !family[child] {
				family[child], grew = true, true
			}
		}
	}
	return family
}

// Ledger row of the serve process or one of its descendants once it records a port, failing at the deadline
func awaitServeLedgerRow(t *testing.T, ledger string, process *exec.Cmd, exited <-chan struct{}, log string) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		select {
		case <-exited:
			deadline = time.Now()
			continue
		default:
		}
		pids := processFamily(process.Process.Pid)
		var rows []map[string]any
		if raw, err := os.ReadFile(ledger); err == nil {
			_ = json.Unmarshal(raw, &rows)
		}
		for _, row := range rows {
			pid, _ := row["pid"].(float64)
			if row["purpose"] == "serve" && pids[int(pid)] && row["port"] != nil && row["port"] != float64(0) {
				return row
			}
		}
	}
	text, _ := os.ReadFile(log)
	t.Fatalf("serve never registered (exit=%v): %s", process.ProcessState, text)
	return nil
}

// Terminates the serve process and its descendants, killing whatever is alive five seconds later
func stopServeFamily(process *exec.Cmd, exited <-chan struct{}) {
	select {
	case <-exited:
		return
	default:
	}
	family := processFamily(process.Process.Pid)
	for pid := range family {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		for pid := range family {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
	}
}

func TestIsolatedServeLedgerRowIsMarkedAndOrdinaryServeIsNot(t *testing.T) {
	tmp := t.TempDir()
	h, user := filepath.Join(tmp, "home"), filepath.Join(tmp, "user")
	for _, dir := range []string{h, user} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "ICEBATH_HOME", "HOME", "ICEBATH_DESKTOP", "ICEBATH_PARENT_PID", "ICEBATH_PARENT_START_MARKER",
			"ICEBATH_DASHBOARD_SESSION_TOKEN", "ICEBATH_SPAWN", "ICEBATH_GATEWAY_LOCK_DIR":
		default:
			env = append(env, kv)
		}
	}
	env = append(env, "ICEBATH_HOME="+h, "HOME="+user, "ICEBATH_GATEWAY_LOCK_DIR="+filepath.Join(tmp, "locks"), runMainEnv+"=1")
	ledger := filepath.Join(h, "spawn-ledger.json")
	rows := map[string]map[string]any{}
	for _, launch := range []struct {
		name  string
		extra []string
	}{{"ordinary", nil}, {"isolated", []string{"--isolated"}}} {
		log := filepath.Join(tmp, launch.name+".log")
		out, err := os.Create(log)
		if err != nil {
			t.Fatal(err)
		}
		args := append(append([]string{"serve"}, launch.extra...), "--host", "127.0.0.1", "--port", strconv.Itoa(freeLoopbackPort(t)))
		process := exec.Command(os.Args[0], args...)
		process.Env, process.Stdout, process.Stderr = env, out, out
		if err := process.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() {
			_ = process.Wait()
			out.Close()
			close(exited)
		}()
		t.Cleanup(func() { stopServeFamily(process, exited) })
		rows[launch.name] = awaitServeLedgerRow(t, ledger, process, exited, log)
	}
	if rows["isolated"]["isolated"] != true {
		t.Fatalf("isolated row = %v", rows["isolated"])
	}
	if isolated, _ := rows["ordinary"]["isolated"].(bool); isolated {
		t.Fatalf("ordinary row = %v", rows["ordinary"])
	}
}
