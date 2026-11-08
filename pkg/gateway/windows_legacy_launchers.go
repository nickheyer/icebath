package gateway

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Pre suffix launcher of this home, a file or a task, with its label
type LegacyLauncherArtifact struct {
	Kind   string
	Label  string
	Target string
}

// Whether a bare named launcher's action points into this home's gateway-service directory
func legacyTargetsThisHome(ctx context.Context, text string) bool {
	marker := normalizeWindowsPath(filepath.Join(winHome(ctx), "gateway-service"))
	return strings.Contains(normalizeWindowsPath(text), marker)
}

// Every pre suffix launcher of this home still present, empty when this home owns the bare name
func LegacyLauncherArtifacts(ctx context.Context) ([]LegacyLauncherArtifact, error) {
	bare := windowsTaskNameDefault
	name, err := winTaskName(ctx)
	if err != nil {
		return nil, err
	}
	if name == bare {
		return nil, nil
	}
	startup, err := winStartupDir()
	if err != nil {
		return nil, err
	}
	serviceDir := filepath.Join(winHome(ctx), "gateway-service")
	var found []LegacyLauncherArtifact
	for _, ext := range []string{".vbs", ".cmd"} {
		p := filepath.Join(startup, bare+ext)
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		text := strings.TrimPrefix(strings.ToValidUTF8(string(raw), "\ufffd"), "\ufeff")
		if legacyTargetsThisHome(ctx, text) {
			found = append(found, LegacyLauncherArtifact{"file", "legacy pre-suffix Windows login item", p})
		}
	}
	for _, f := range [][2]string{{".vbs", "legacy pre-suffix task launcher"}, {".cmd", "legacy pre-suffix task script"}} {
		if p := filepath.Join(serviceDir, bare+f[0]); winExists(p) {
			found = append(found, LegacyLauncherArtifact{"file", f[1], p})
		}
	}
	code, out, _, err := winExecSchtasks([]string{"/Query", "/TN", bare, "/XML"})
	if err != nil {
		return nil, err
	}
	if code == 0 && legacyTargetsThisHome(ctx, out) {
		found = append(found, LegacyLauncherArtifact{"task", "legacy pre-suffix Scheduled Task", bare})
	}
	return found, nil
}

// Names every stray pre suffix launcher with the uninstall remedy, true when any exist
func WarnLegacyLaunchers(ctx context.Context) (bool, error) {
	artifacts, err := LegacyLauncherArtifacts(ctx)
	if err != nil {
		return false, err
	}
	for _, a := range artifacts {
		winPrintln("⚠ " + a.Label + " still installed: " + a.Target)
	}
	if len(artifacts) > 0 {
		winPrintln("  These predate per-profile launcher names and also start the gateway at logon.")
		winPrintln("  Remove them with: icebath gateway uninstall   (then: icebath gateway install)")
	}
	return len(artifacts) > 0, nil
}

// Deletes the stray launchers, naming the exact elevated command for a task that needs elevation
func RemoveLegacyLaunchers(ctx context.Context) error {
	artifacts, err := LegacyLauncherArtifacts(ctx)
	if err != nil {
		return err
	}
	for _, a := range artifacts {
		if a.Kind == "file" {
			if err := os.Remove(a.Target); err != nil {
				winPrintln(fmt.Sprintf("⚠ Could not remove %s %s: %s", a.Label, a.Target, pypath.OSErrorText(err)))
			} else {
				winPrintln(fmt.Sprintf("✓ Removed %s: %s", a.Label, a.Target))
			}
			continue
		}
		code, _, errText, err := winExecSchtasks([]string{"/Delete", "/F", "/TN", a.Target})
		if err != nil {
			return err
		}
		switch {
		case code == 0:
			winPrintln(fmt.Sprintf("✓ Removed %s %s", a.Label, pyrepr.Repr(a.Target)))
		case windowsIsAccessDenied(pystr.Strip(errText)):
			winPrintln(fmt.Sprintf("⚠ %s %s needs an elevated shell to remove; run as administrator:", a.Label, pyrepr.Repr(a.Target)))
			winPrintln("    schtasks /Delete /F /TN " + a.Target)
		default:
			winPrintln(fmt.Sprintf("⚠ schtasks /Delete %s returned code %d: %s", pyrepr.Repr(a.Target), code, pystr.Strip(errText)))
		}
	}
	return nil
}
