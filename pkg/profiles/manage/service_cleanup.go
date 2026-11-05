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

// False with a notice when a service definition pins another home than the profile being removed
func ownedByProfile(definition, profileDir string) bool {
	raw, pinned := gatewayServicePinnedHome(definition)
	if !pinned || gatewayServiceResolve(raw) == gatewayServiceResolve(profileDir) {
		return true
	}
	fmt.Fprintf(stdout, "↷ %s runs ICEBATH_HOME=%s, not %s: another install's service, left alone\n", definition, raw, profileDir)
	return false
}

// Removes the system wide systemd unit of the profile when root, else names the leftover and its removal command
func removeSystemSystemdUnit(ctx context.Context, profileDir string) (bool, error) {
	svc := gatewayServiceName(ctx)
	unit := filepath.Join(gatewaySystemUnitDir(), svc+".service")
	if !pathExists(unit) || !ownedByProfile(unit, profileDir) {
		return false, nil
	}
	if os.Geteuid() != 0 {
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
	task := gatewayWindowsTaskName(ctx)
	removed := false
	if gatewayWindowsTaskRegistered(ctx) {
		code, _, errText := gatewayExecSchtasks([]string{"/Delete", "/F", "/TN", task})
		if code == 0 {
			removed = true
			fmt.Fprintf(stdout, "✓ Removed Scheduled Task %s\n", pyrepr.Repr(task))
		} else {
			fmt.Fprintf(stdout, "⚠ Scheduled Task %s remains (schtasks code %d): %s\n", pyrepr.Repr(task), code, pystr.Strip(errText))
		}
	}
	for _, path := range []string{gatewayWindowsStartupEntryPath(ctx), gatewayWindowsLegacyStartupEntryPath(ctx)} {
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
