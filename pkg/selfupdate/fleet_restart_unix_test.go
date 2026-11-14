//go:build !windows

package selfupdate

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestUpdateSignalsProvenStaleGatewaySurvivor(t *testing.T) {
	h := newFleetHarness(t)
	marker := filepath.Join(t.TempDir(), "got_sigusr1")
	child := exec.Command("sh", "-c", `trap 'echo x > "$1"; kill "$s"; exit 75' USR1; sleep 120 & s=$!; echo ready; wait "$s"`, "sh", marker)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			child.Process.Kill()
			<-exited
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("child said %q, %v", line, err)
	}
	go func() { exited <- child.Wait() }()

	restart := newGatewayRestartOutcome()
	restart.Incomplete = true
	restart.PreRestartGatewayPIDs = []int{child.Process.Pid}
	fleet := []any{
		pyjson.ObjectOf("profile", "default", "pid", int64(child.Process.Pid), "state", "stale", "code_sha", strings.Repeat("a", 40)),
		pyjson.ObjectOf("profile", "fresh", "pid", int64(os.Getpid()), "state", "current", "code_sha", strings.Repeat("b", 40)),
	}
	signalled := signalStaleFleetSurvivors(h.ctx, io.Discard, fleet, restart, 10.0)
	select {
	case waitErr := <-exited:
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) || exit.ExitCode() != 75 {
			t.Fatalf("child exit = %v", waitErr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the stale gateway never exited")
	}
	exited <- nil

	if !reflect.DeepEqual(signalled, []int{child.Process.Pid}) {
		t.Fatalf("signalled = %v", signalled)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the survivor never saw SIGUSR1: %v", err)
	}
	if !reflect.DeepEqual(restart.KilledPIDs, map[int]bool{child.Process.Pid: true}) {
		t.Fatalf("killed pids = %v", restart.KilledPIDs)
	}
}
