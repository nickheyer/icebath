package whatsapp

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestIdentityGuardKillsWhenStartTimeMatches(t *testing.T) {
	dir := t.TempDir()
	proc := startProc(t, spawnHelper(t, "sleep:0.2", ""))
	writeBridgePidfile(dir, proc.Pid(), nil)
	killStaleBridgeByPidfile(dir)
	if _, dead := proc.Wait(5 * time.Second); !dead {
		t.Fatal("the real bridge process should be killed")
	}
	if _, err := os.Stat(filepath.Join(dir, "bridge.pid")); err == nil {
		t.Fatal("bridge.pid survived the reap")
	}
}

func TestIdentityGuardLegacyPidfileRefusesCmdlineOnlyKill(t *testing.T) {
	dir := t.TempDir()
	proc := startProc(t, spawnHelper(t, "sleep:30", "", "node", dir))
	if err := os.WriteFile(filepath.Join(dir, "bridge.pid"), []byte(fmt.Sprint(proc.Pid())), 0o644); err != nil {
		t.Fatal(err)
	}
	killStaleBridgeByPidfile(dir)
	time.Sleep(500 * time.Millisecond)
	if _, dead := proc.Poll(); dead {
		t.Fatal("a cmdline-only match must not be killed (fail closed)")
	}
	if _, err := os.Stat(filepath.Join(dir, "bridge.pid")); err == nil {
		t.Fatal("legacy bridge.pid was not removed")
	}
}

func TestKillPortProcessListenerLookupExcludesClientProcess(t *testing.T) {
	srv, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	port := srv.Addr().(*net.TCPAddr).Port
	client := startProc(t, spawnHelper(t, fmt.Sprintf("client:%d", port), ""))
	conn, err := srv.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pids, _ := listenerPidsOnPort(port)
	if !slices.Contains(pids, os.Getpid()) {
		t.Skip("neither lsof nor ss detected the listener here")
	}
	if slices.Contains(pids, client.Pid()) {
		t.Fatalf("client pid %d listed as a listener: %v", client.Pid(), pids)
	}
}
