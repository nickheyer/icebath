package tuiserver

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"testing"
)

// Worker argv captured from a spawn that never starts a child
func spawnWorkerArgv(t *testing.T, providerName string) []string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var argv []string
	saved := spawnOwned
	spawnOwned = func(_ context.Context, cmd *exec.Cmd, purpose string) error {
		argv = slices.Clone(cmd.Args)
		return errors.New("captured")
	}
	t.Cleanup(func() { spawnOwned = saved })
	_, _ = newSlashWorker(context.Background(), "sess", "default", "", providerName)
	if argv == nil {
		t.Fatal("worker never spawned")
	}
	return argv
}

func TestSlashWorkerArgvCarriesProvider(t *testing.T) {
	argv := spawnWorkerArgv(t, "moa")
	i := slices.Index(argv, "--provider")
	if i < 0 || argv[i+1] != "moa" {
		t.Fatalf("argv %v", argv)
	}
	if j := slices.Index(argv, "--model"); j < 0 || argv[j+1] != "default" {
		t.Fatalf("argv %v", argv)
	}
}

func TestSlashWorkerArgvOmitsEmptyProvider(t *testing.T) {
	if slices.Contains(spawnWorkerArgv(t, ""), "--provider") {
		t.Fatal("provider flag present")
	}
}
