package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Writes a unit pinning home with a gateway run ExecStart
func pinnedUnit(t *testing.T, path, h string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeBytes(t, path, "[Service]\nEnvironment=\"ICEBATH_HOME="+h+"\"\nExecStart=/usr/bin/icebath gateway run\n")
}

// Records every service manager, subprocess and kill call instead of running it
func recordedLifecycle(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	swapSetup(t, &lifecycleRunSystemctl, func(_ context.Context, argv []string, _ bool, _ time.Duration) (CommandResult, error) {
		calls = append(calls, "systemctl "+strings.Join(argv, " "))
		return CommandResult{}, nil
	})
	swapSetup(t, &RunServiceCommand, func(_ context.Context, _ time.Duration, argv ...string) (CommandResult, error) {
		calls = append(calls, "run "+strings.Join(argv, " "))
		return CommandResult{}, nil
	})
	swapSetup(t, &lifecycleTerminatePid, func(pid int, _ bool, _ any) error {
		calls = append(calls, "kill")
		return nil
	})
	swapSetup(t, &setupSupportsSystemd, func(context.Context) bool { return true })
	swapSetup(t, &lifecyclePreflightUser, func(context.Context, bool) error { return nil })
	swapSetup(t, &lifecycleRefreshSystemdUnit, func(context.Context, bool) (bool, error) { return false, nil })
	return &calls
}

func TestLifecycleVerbRefusesAUserUnitPinningAnotherHome(t *testing.T) {
	for _, verb := range []string{"stop", "restart", "start", "uninstall"} {
		t.Run(verb, func(t *testing.T) {
			tmp := t.TempDir()
			account := filepath.Join(tmp, "account")
			custom := filepath.Join(tmp, "custom", "profiles", "coder")
			if err := os.MkdirAll(custom, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", custom)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(account, ".config"))
			swapSetup(t, &serviceGOOS, "linux")
			swapSetup(t, &lifecycleGeteuid, func() int { return 1000 })
			swapSetup(t, &serviceGeteuid, func() int { return 1000 })
			calls := recordedLifecycle(t)
			var out bytes.Buffer
			swapSetup(t, &lifecycleOut, io.Writer(&out))
			ctx := context.Background()
			unit, err := GetSystemdUnitPath(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(unit) != "icebath-gateway-coder.service" {
				t.Fatalf("unit %s", unit)
			}
			pinnedUnit(t, unit, filepath.Join(account, ".icebath", "profiles", "coder"))
			err = serviceCall(ctx, "systemd", verb, false)
			var exit *SystemExitError
			if verb == "uninstall" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.As(err, &exit) || exit.Code != 1 {
				t.Fatalf("err %v", err)
			}
			if len(*calls) != 0 {
				t.Fatalf("calls %v", *calls)
			}
			if !strings.Contains(out.String(), "Refusing to") {
				t.Fatalf("output %q", out.String())
			}
			if _, err := os.Stat(unit); err != nil {
				t.Fatal(err)
			}
			if got := os.Getenv("ICEBATH_HOME"); got != custom {
				t.Fatalf("home adopted %s", got)
			}
		})
	}
}
