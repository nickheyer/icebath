package gateway

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type killCall struct {
	pid   int
	force bool
}

// Terminate double recording each pid and force flag in call order
func recordKills(t *testing.T) *[]killCall {
	t.Helper()
	calls := &[]killCall{}
	swapSetup(t, &statusTerminatePid, func(pid int, force bool, _ any) error {
		*calls = append(*calls, killCall{pid, force})
		return nil
	})
	return calls
}

func discoveredPids(t *testing.T, pids ...int) {
	t.Helper()
	swapSetup(t, &killFindGatewayPIDs, func(context.Context, map[int]bool, bool) ([]int, error) { return pids, nil })
}

func TestWaitForGatewayExitKillGatewayProcessesForceUsesHelper(t *testing.T) {
	discoveredPids(t, 11, 22)
	swapSetup(t, &killCaptureArgv, func(int) []string { return []string{"python", "-m", "icebath_cli.main", "gateway", "run"} })
	calls := recordKills(t)
	killed, err := KillGatewayProcesses(context.Background(), io.Discard, true, nil, false)
	if err != nil || killed != 2 {
		t.Fatalf("killed = %d, %v", killed, err)
	}
	if want := []killCall{{11, true}, {22, true}}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestWaitForGatewayExitKillGatewayProcessesForceRefusesRecycledPid(t *testing.T) {
	discoveredPids(t, 11, 22)
	swapSetup(t, &killCaptureArgv, func(pid int) []string {
		if pid == 11 {
			return nil
		}
		return []string{"python", "-m", "icebath_cli.main", "gateway", "run"}
	})
	calls := recordKills(t)
	killed, err := KillGatewayProcesses(context.Background(), io.Discard, true, nil, false)
	if err != nil || killed != 1 {
		t.Fatalf("killed = %d, %v", killed, err)
	}
	if want := []killCall{{22, true}}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestStopAllLeavesAnotherHomesGatewayProcessAlone(t *testing.T) {
	tmp := t.TempDir()
	own := filepath.Join(tmp, "own", ".icebath")
	other := filepath.Join(tmp, "account", ".icebath")
	t.Setenv("ICEBATH_HOME", own)
	discoveredPids(t, 111, 222)
	homes := map[int]string{111: own, 222: other}
	processHomes(t, func(pid int) (string, bool, error) {
		h, ok := homes[pid]
		return h, ok, nil
	})
	calls := recordKills(t)
	var out bytes.Buffer
	killed, err := KillGatewayProcesses(context.Background(), &out, false, nil, true)
	if err != nil || killed != 1 {
		t.Fatalf("killed = %d, %v", killed, err)
	}
	if want := []killCall{{111, false}}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %v", *calls)
	}
	if want := "↷ PID 222: gateway of another Icebath home (" + other + ") — left alone\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("output %q lacks %q", out.String(), want)
	}
}
