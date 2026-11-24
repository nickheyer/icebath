//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	plistutil "github.com/nickheyer/icebath/pkg/utils/plist"
	shellutil "github.com/nickheyer/icebath/pkg/utils/shell"
)

var (
	fleetHomeForPID = func(ctx context.Context, pid int) (string, bool, error) {
		return gateway.IcebathHomeForPID(ctx, pid, false)
	}
	fleetPartitionPidsByScope = gateway.PartitionGatewayPidsByScope
	fleetLaunchdPlistPath     = gateway.GetLaunchdPlistPath
)

// Home a systemd unit runs on from its live main pid, its declared environment, then the user default
func systemdUnitIcebathHome(ctx context.Context, scopeCmd []string, svcName string) (string, bool) {
	pid, err := fleetUnitMainPid(ctx, scopeCmd, svcName)
	if err == nil && pid > 0 {
		if unitHome, known, err := fleetHomeForPID(ctx, pid); err == nil && known {
			return unitHome, true
		}
	}
	shown, err := systemctl(ctx, fleetArgv(scopeCmd, "show", svcName, "--property=Environment", "--value"), 10)
	if err != nil || shown == nil || shown.ExitCode != 0 {
		return "", false
	}
	envLine := strings.TrimSpace(completedStdout(shown))
	tokens, err := shellutil.Split(envLine)
	if err != nil {
		tokens = strings.Fields(envLine)
	}
	for _, token := range tokens {
		key, value, found := strings.Cut(token, "=")
		if found && key == "ICEBATH_HOME" && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), true
		}
	}
	if slices.Contains(scopeCmd, "--user") {
		userHome, err := pathutil.Home()
		if err != nil {
			return "", false
		}
		return pathutil.Join(userHome, ".icebath"), true
	}
	return "", false
}

// Home a label's installed plist pins when this update does not own it, false for ours or an unreadable plist
func launchdLabelForeignHome(ctx context.Context, label string) (string, bool) {
	return launchdLabelForeignHomeIn(ctx, label, nil)
}

// Foreign home of a label's plist judged against a given scope, the live scope when nil
func launchdLabelForeignHomeIn(ctx context.Context, label string, scope map[string]bool) (string, bool) {
	own, err := fleetLaunchdPlistPath(ctx)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(own), label+".plist"))
	if err != nil {
		return "", false
	}
	loaded, err := plistutil.Decode(data)
	if err != nil {
		return "", false
	}
	root, ok := loaded.(map[string]any)
	if !ok {
		return "", false
	}
	variables, ok := root["EnvironmentVariables"].(map[string]any)
	if !ok {
		return "", false
	}
	value, ok := variables["ICEBATH_HOME"]
	if !ok {
		return "", false
	}
	pinned := anyutil.String(value)
	if gateway.HomeInUpdateScope(ctx, pinned, scope) {
		return "", false
	}
	return pinned, true
}

// One notice line for a runtime the update leaves alone, a foreign home or unreadable ownership
func describeSkippedRuntime(kind, name, runtimeHome string, known bool) string {
	if !known {
		return fmt.Sprintf("  ↷ %s: %s whose Icebath home could not be read — left alone (not restarted)", name, kind)
	}
	return fmt.Sprintf("  ↷ %s: %s of another Icebath home (%s) — left alone (not restarted)", name, kind, runtimeHome)
}

// Prints the left alone notice once per update run and names the runtime on the open receipt
func announceSkippedRuntime(ctx context.Context, w io.Writer, kind, name, runtimeHome string, known bool) {
	entry := []any{kind, name, nil}
	if known {
		entry[2] = runtimeHome
	}
	announced := []any{}
	if current := updaterecord.CurrentReceipt(ctx); current != nil && current.Data != nil {
		if recorded, ok := current.Data.Get("runtimes_left_alone"); ok {
			if rows, isList := recorded.([]any); isList {
				announced = append(announced, rows...)
			}
		}
	}
	for _, row := range announced {
		if anyutil.Equal(row, entry) {
			return
		}
	}
	fmt.Fprintln(w, describeSkippedRuntime(kind, name, runtimeHome, known))
	updaterecord.RecordFact(ctx, "runtimes_left_alone", append(announced, entry))
}

// Launchd labels of this install the profile layout cannot derive, the excluded ones left out
func legacyLaunchdLabelsForInstall(ctx context.Context, exclude map[string]bool) ([]string, error) {
	return gateway.LegacyLaunchdLabelsForInstall(ctx, exclude), nil
}
