package manage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Runs a supervisor command with output discarded, returning its exit code, errors for a timeout or a missing binary
var runQuiet = func(timeout time.Duration, args ...string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, fmt.Errorf("Command '%s' timed out after %d seconds", pyrepr.Repr(args), int(timeout.Seconds()))
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("[Errno 2] No such file or directory: %s", pyrepr.Repr(args[0]))
	}
	return 0, nil
}

// Replaces the supervisor command runner for callers outside the package until the returned restore runs
func SwapRunQuiet(f func(timeout time.Duration, args ...string) (int, error)) (restore func()) {
	old := runQuiet
	runQuiet = f
	return func() { runQuiet = old }
}

// Gateway service manager facts the cleanup reads, the gateway contract functions unless a test swaps one
var serviceFacts = struct {
	ServiceName                   func(ctx context.Context) string
	UserSystemdUnitDir            func(ctx context.Context) string
	SystemUnitDir                 func() string
	LaunchdPlistPath              func(ctx context.Context) string
	PinnedHome                    func(definition string) (string, bool)
	Resolve                       func(p string) string
	WindowsTaskName               func(ctx context.Context) string
	WindowsTaskRegistered         func(ctx context.Context) bool
	ExecSchtasks                  func(args []string) (int, string, string)
	WindowsStartupEntryPath       func(ctx context.Context) string
	WindowsLegacyStartupEntryPath func(ctx context.Context) string
	Geteuid                       func() int
}{
	ServiceName: gatewayServiceName, UserSystemdUnitDir: gatewayUserSystemdUnitDir, SystemUnitDir: gatewaySystemUnitDir,
	LaunchdPlistPath: gatewayLaunchdPlistPath, PinnedHome: gatewayServicePinnedHome, Resolve: gatewayServiceResolve,
	WindowsTaskName: gatewayWindowsTaskName, WindowsTaskRegistered: gatewayWindowsTaskRegistered,
	ExecSchtasks: gatewayExecSchtasks, WindowsStartupEntryPath: gatewayWindowsStartupEntryPath,
	WindowsLegacyStartupEntryPath: gatewayWindowsLegacyStartupEntryPath, Geteuid: os.Geteuid,
}

// False with a notice when a service definition pins another home than the profile being removed
func ownedByProfile(definition, profileDir string) bool {
	raw, pinned := serviceFacts.PinnedHome(definition)
	if !pinned || serviceFacts.Resolve(raw) == serviceFacts.Resolve(profileDir) {
		return true
	}
	fmt.Fprintf(stdout, "↷ %s runs ICEBATH_HOME=%s, not %s: another install's service, left alone\n", definition, raw, profileDir)
	return false
}

// Removes the system wide systemd unit of the profile when root, else names the leftover and its removal command
func removeSystemSystemdUnit(ctx context.Context, profileDir string) (bool, error) {
	svc := serviceFacts.ServiceName(ctx)
	unit := filepath.Join(serviceFacts.SystemUnitDir(), svc+".service")
	if !pathExists(unit) || !ownedByProfile(unit, profileDir) {
		return false, nil
	}
	if serviceFacts.Geteuid() != 0 {
		fmt.Fprintf(stdout, "⚠ System service %s remains at %s; it will restart the removed profile at boot.\n", svc, unit)
		fmt.Fprintf(stdout, "  Remove it with: sudo systemctl disable --now %s && sudo rm %s && sudo systemctl daemon-reload\n", svc, unit)
		return false, nil
	}
	if _, err := runQuiet(30*time.Second, "systemctl", "disable", svc); err != nil {
		return false, err
	}
	stopped, err := runQuiet(30*time.Second, "systemctl", "stop", svc)
	if err != nil {
		return false, err
	}
	if err := unlinkMissingOK(unit); err != nil {
		return false, err
	}
	if _, err := runQuiet(30*time.Second, "systemctl", "daemon-reload"); err != nil {
		return false, err
	}
	if stopped != 0 {
		fmt.Fprintf(stdout, "⚠ System service %s unit removed, but `systemctl stop` exited %d; its gateway may still be running. Stop it with: sudo systemctl stop %s\n", svc, stopped, svc)
		return true, nil
	}
	fmt.Fprintf(stdout, "✓ System service %s removed\n", svc)
	return true, nil
}

// Deletes the profile's Scheduled Task and Startup folder login items without a UAC prompt
func removeWindowsTask(ctx context.Context) (bool, error) {
	task := serviceFacts.WindowsTaskName(ctx)
	removed := false
	if serviceFacts.WindowsTaskRegistered(ctx) {
		code, _, errText := serviceFacts.ExecSchtasks([]string{"/Delete", "/F", "/TN", task})
		if code == 0 {
			removed = true
			fmt.Fprintf(stdout, "✓ Removed Scheduled Task %s\n", pyrepr.Repr(task))
		} else {
			fmt.Fprintf(stdout, "⚠ Scheduled Task %s remains (schtasks code %d): %s\n", pyrepr.Repr(task), code, pystr.Strip(errText))
		}
	}
	for _, path := range []string{serviceFacts.WindowsStartupEntryPath(ctx), serviceFacts.WindowsLegacyStartupEntryPath(ctx)} {
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, errors.New(pyfs.OSErrorText(err))
		}
		removed = true
		fmt.Fprintf(stdout, "✓ Removed Windows login item: %s\n", path)
	}
	return removed, nil
}
