package relayhost_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/relayhost/relaytest"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Relay host children of this process by their command line
func relayHostChildren(t *testing.T) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
		if len(fields) < 2 || fields[1] != strconv.Itoa(os.Getpid()) {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if strings.Contains(string(cmdline), "relay-host") {
			pids = append(pids, pid)
		}
	}
	return pids
}

func TestRelayHostChildDoesNotHoldTheParentsStderr(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	relaytest.Use(t)
	if _, err := tools.LoadNemoRelay(); err != nil {
		t.Fatal(err)
	}
	own, err := os.Readlink("/proc/self/fd/2")
	if err != nil {
		t.Fatal(err)
	}
	children := relayHostChildren(t)
	if len(children) == 0 {
		t.Fatal("no relay host child runs")
	}
	for _, pid := range children {
		target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", "2"))
		if err != nil {
			t.Fatal(err)
		}
		if target == own || !strings.HasPrefix(target, "pipe:") {
			t.Fatalf("relay host %d stderr is %s, this process's is %s", pid, target, own)
		}
	}
}
