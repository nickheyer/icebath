package gateway

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Live child standing in for the old daemon, reaped as soon as it exits
func oldOwner(t *testing.T, h string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	proc := exec.Command("sleep", "60")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { proc.Wait(); close(done) }()
	t.Cleanup(func() { proc.Process.Kill(); <-done })
	swapSeam(t, &replaceIdentifyOf, func(string, time.Duration) (string, *pyjson.Object, error) {
		return h, ownerPayload(t, h, proc.Process.Pid, GetProcessStartTime(proc.Process.Pid)), nil
	})
	return proc, done
}

// Whether the owner is still running
func ownerAlive(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	default:
		return true
	}
}

func TestReplacementStopsTheVerifiedOldOwnerWithAPlannedStop(t *testing.T) {
	h := replaceHome(t)
	_, done := oldOwner(t, h)
	swapSeam(t, &replaceInstallRootOf, func() string { return h })
	if refusal := ReplaceIncompatibleOwner(h, pythread.Monotonic()+15); refusal != "" {
		t.Fatal(refusal)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old owner survived")
	}
}

func TestReplacementRefusesARecycledPid(t *testing.T) {
	h := replaceHome(t)
	proc, done := oldOwner(t, h)
	swapSeam(t, &replaceInstallRootOf, func() string { return h })
	swapSeam(t, &replaceIdentifyOf, func(string, time.Duration) (string, *pyjson.Object, error) {
		return h, ownerPayload(t, h, proc.Process.Pid, 1), nil
	})
	if refusal := ReplaceIncompatibleOwner(h, pythread.Monotonic()+5); refusal != "owner_unverified" || !ownerAlive(done) {
		t.Fatal(refusal)
	}
}

func TestReplacementRefusesAnotherInstallsOwner(t *testing.T) {
	h := replaceHome(t)
	_, done := oldOwner(t, h)
	otherRoot, _ := pypath.Resolve(filepath.Join(t.TempDir(), "other"))
	os.Mkdir(otherRoot, 0o700)
	swapSeam(t, &replaceInstallRootOf, func() string { return otherRoot })
	if refusal := ReplaceIncompatibleOwner(h, pythread.Monotonic()+5); refusal != "foreign_install" || !ownerAlive(done) {
		t.Fatal(refusal)
	}
}

func TestReplacementRefusesAnOwnerServingAnotherHome(t *testing.T) {
	h := replaceHome(t)
	proc, done := oldOwner(t, h)
	elsewhere, _ := pypath.Resolve(filepath.Join(t.TempDir(), "elsewhere"))
	swapSeam(t, &replaceInstallRootOf, func() string { return h })
	swapSeam(t, &replaceIdentifyOf, func(string, time.Duration) (string, *pyjson.Object, error) {
		return h, ownerPayload(t, elsewhere, proc.Process.Pid, GetProcessStartTime(proc.Process.Pid)), nil
	})
	if refusal := ReplaceIncompatibleOwner(h, pythread.Monotonic()+5); refusal != "foreign_install" || !ownerAlive(done) {
		t.Fatal(refusal)
	}
}
