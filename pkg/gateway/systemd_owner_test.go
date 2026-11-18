package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Points the proc tree at a temp dir and answers systemctl show with the given main pids per scope
func pinUnitProbes(t *testing.T, mainPIDs map[string]string) *[][]string {
	t.Helper()
	savedRoot, savedProbe := systemdProcRoot, systemdRunProbe
	t.Cleanup(func() { systemdProcRoot, systemdRunProbe = savedRoot, savedProbe })
	systemdProcRoot = t.TempDir()
	var calls [][]string
	systemdRunProbe = func(ctx context.Context, argv []string, timeout time.Duration) (*pysubprocess.Completed, error) {
		calls = append(calls, argv)
		scope := "system"
		if argv[1] == "--user" {
			scope = "user"
		}
		out, known := mainPIDs[scope]
		if !known {
			return nil, errors.New("systemctl not found")
		}
		return &pysubprocess.Completed{Returncode: 0, Stdout: []byte(out + "\n")}, nil
	}
	return &calls
}

// Writes the cgroup file of a pid under the pinned proc tree
func writeCgroup(t *testing.T, pid, text string) {
	t.Helper()
	dir := filepath.Join(systemdProcRoot, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSystemdOwnerForPidRequiresTheUnitMainPid(t *testing.T) {
	ctx := context.Background()
	calls := pinUnitProbes(t, map[string]string{"user": "4242", "system": "1"})
	writeCgroup(t, "4242", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/icebath-gateway.service\n")
	unit, scope, ok := SystemdOwnerForPid(ctx, 4242)
	if !ok || unit != "icebath-gateway.service" || scope != "user" {
		t.Fatalf("owner %q %q %v", unit, scope, ok)
	}
	if want := []string{"systemctl", "--user", "show", "icebath-gateway.service", "--property=MainPID", "--value"}; len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], want) {
		t.Fatalf("probes %v", *calls)
	}
	writeCgroup(t, "77", "0::/system.slice/cron.service\n")
	if _, _, ok := SystemdOwnerForPid(ctx, 77); ok {
		t.Fatal("a process inside another unit's cgroup read as owned by it")
	}
	writeCgroup(t, "88", "12:pids:/legacy\n0::/custom/odd.service\n")
	*calls = nil
	systemdRunProbe = func(ctx context.Context, argv []string, timeout time.Duration) (*pysubprocess.Completed, error) {
		*calls = append(*calls, argv)
		if argv[1] == "--user" {
			return &pysubprocess.Completed{Returncode: 0, Stdout: []byte("88")}, nil
		}
		return &pysubprocess.Completed{Returncode: 1}, nil
	}
	unit, scope, ok = SystemdOwnerForPid(ctx, 88)
	if !ok || unit != "odd.service" || scope != "" || len(*calls) != 2 || (*calls)[0][1] != "show" {
		t.Fatalf("unknown scope owner %q %q %v after %v", unit, scope, ok, *calls)
	}
	if _, _, ok := SystemdOwnerForPid(ctx, 999); ok {
		t.Fatal("a process with no cgroup file read as owned")
	}
	writeCgroup(t, "55", "0::/user.slice/session-3.scope\n")
	if _, _, ok := SystemdOwnerForPid(ctx, 55); ok {
		t.Fatal("a scope cgroup read as a service")
	}
	if got, ok := getPidCgroupPath(55); !ok || got != "/user.slice/session-3.scope" || extractScopeFromCgroup("/elsewhere") != "" {
		t.Fatalf("cgroup path %q %v", got, ok)
	}
}
