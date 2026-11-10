package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Prints a yellow warning line on the lifecycle output like the setup print_warning
func printLifecycleWarning(text string) { (&cliui.Console{Out: lifecycleOut}).PrintWarning(text) }

// False with a warning when the installed unit pins another home, so removing it would take down another install
func systemdUnitBelongsToCurrentHome(ctx context.Context, system bool) (bool, error) {
	foreign, err := ForeignSystemdUnitHome(ctx, system)
	if err != nil {
		return false, err
	}
	if foreign != "" {
		unitPath, err := GetSystemdUnitPath(ctx, system)
		if err != nil {
			return false, err
		}
		printLifecycleWarning(fmt.Sprintf("Refusing to remove %s: it runs ICEBATH_HOME=%s, but this process has ICEBATH_HOME=%s",
			unitPath, foreign, home.Dir(ctx)))
		return false, nil
	}
	if err := lifecycleSyncHome(ctx, system); err != nil {
		return false, err
	}
	unitHome, err := hermesHomeFromSystemdUnitFile(ctx, system)
	if err != nil || unitHome == "" {
		return err == nil, err
	}
	expanded, err := pypath.PathExpanduser(unitHome)
	if err != nil {
		return false, err
	}
	resolvedUnit, err := pypath.Resolve(expanded)
	if err != nil {
		return false, err
	}
	resolvedHome, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return false, err
	}
	if resolvedUnit == resolvedHome {
		return true, nil
	}
	unitPath, err := GetSystemdUnitPath(ctx, system)
	if err != nil {
		return false, err
	}
	printLifecycleWarning(fmt.Sprintf("Refusing to remove %s: it runs ICEBATH_HOME=%s, but this process has ICEBATH_HOME=%s",
		unitPath, unitHome, home.Dir(ctx)))
	return false, nil
}

// Stops, disables and removes the systemd gateway unit of the scope unless it belongs to another home
func SystemdUninstall(ctx context.Context, system bool) error {
	system, err := systemdScopePreamble(ctx, "uninstall", system, false, false)
	if err != nil {
		return err
	}
	if belongs, err := systemdUnitBelongsToCurrentHome(ctx, system); err != nil || !belongs {
		return err
	}
	for _, step := range []struct {
		verb    string
		timeout int
	}{{"stop", 90}, {"disable", 30}} {
		name, err := lifecycleServiceName(ctx)
		if err != nil {
			return err
		}
		if _, err := lifecycleRunSystemctl(ctx, []string{step.verb, name}, system, time.Duration(step.timeout)*time.Second); err != nil {
			return err
		}
	}
	unitPath, err := GetSystemdUnitPath(ctx, system)
	if err != nil {
		return err
	}
	if pathExistsSafe(unitPath) {
		if err := os.Remove(unitPath); err != nil {
			return err
		}
		lifecyclePrintf("✓ Removed %s\n", unitPath)
	}
	if _, err := runSystemctlChecked(ctx, []string{"daemon-reload"}, system, 30*time.Second); err != nil {
		return err
	}
	lifecyclePrintf("✓ %s service uninstalled\n", pystr.Capitalize(ServiceScopeLabel(system)))
	return nil
}

// Boots out the launchd job and removes its plist unless the plist belongs to another home
func LaunchdUninstall(ctx context.Context) error {
	if err := RefuseForeignLaunchdPlist(ctx, "uninstall"); err != nil {
		return err
	}
	plistPath, err := launchdPlistPathOf(ctx)
	if err != nil {
		return err
	}
	domain, err := launchdDomainOf(ctx)
	if err != nil {
		return err
	}
	label, err := launchdLabelOf(ctx)
	if err != nil {
		return err
	}
	if _, err := launchctlRun(ctx, launchdLongTimeout, true, "launchctl", "bootout", domain+"/"+label); err != nil {
		return err
	}
	if pathExistsSafe(plistPath) {
		if err := os.Remove(plistPath); err != nil {
			return err
		}
		lifecyclePrintf("✓ Removed %s\n", plistPath)
	}
	lifecyclePrintf("✓ Service uninstalled\n")
	return nil
}

// Removes the Scheduled Task, the Startup folder entries, the task scripts and the legacy launchers
func WindowsUninstall(ctx context.Context) error {
	if err := assertWindows(); err != nil {
		return err
	}
	taskName, err := winTaskName(ctx)
	if err != nil {
		return err
	}
	scriptPath, err := winTaskScriptPath(ctx)
	if err != nil {
		return err
	}
	removed := false
	registered, err := winIsTaskRegistered(ctx)
	if err != nil {
		return err
	}
	if registered {
		code, _, errOut, err := winExecSchtasks([]string{"/Delete", "/F", "/TN", taskName})
		if err != nil {
			return err
		}
		detail := pystr.Strip(errOut)
		admin := false
		if code != 0 && windowsIsAccessDenied(detail) {
			if admin, err = winIsRunningAsAdmin(); err != nil {
				return err
			}
		}
		switch {
		case code == 0:
			removed = true
			winPrintln("✓ Removed Scheduled Task " + pyrepr.Repr(taskName))
		case windowsIsAccessDenied(detail) && !admin:
			reason := detail
			if reason == "" {
				reason = "access denied"
			}
			winPrintln(fmt.Sprintf("↻ Scheduled Task uninstall needs administrator approval (%s)", reason))
			winPrintln("  UAC is Windows' admin approval prompt; it is needed to remove the Scheduled Task.")
			if WizardSetup.PromptYesNo("  Open the UAC prompt now?", false) {
				launched, err := launchElevatedGatewayCommand(ctx, "uninstall", nil)
				if err != nil {
					return err
				}
				if launched {
					winPrintln("✓ Launched elevated Icebath gateway uninstall prompt.")
					winPrintln("  Approve the Windows UAC prompt, then run: icebath gateway status")
					return nil
				}
				winPrintln("⚠ Elevated uninstall prompt was unavailable or cancelled.")
			} else {
				winPrintln("  Skipped elevation. Scheduled Task was not removed.")
			}
		default:
			winPrintln(fmt.Sprintf("⚠ schtasks /Delete returned code %d: %s", code, detail))
		}
	}
	entry, err := winStartupEntryPath(ctx)
	if err != nil {
		return err
	}
	legacy, err := winLegacyStartupEntryPath(ctx)
	if err != nil {
		return err
	}
	staging, err := windowsStartupStagingPath(ctx)
	if err != nil {
		return err
	}
	for _, item := range []struct{ path, label string }{
		{entry, "Windows login item"}, {legacy, "legacy Windows login item"},
		{staging, "Windows login item staging file"},
		{scriptPath, "Task script"}, {winWithSuffix(scriptPath, ".vbs"), "Task launcher"},
	} {
		if err := winRemove(item.path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		winPrintln(fmt.Sprintf("✓ Removed %s: %s", item.label, item.path))
	}
	if err := winRemoveLegacyLaunchers(ctx); err != nil {
		return err
	}
	if registered, err = winIsTaskRegistered(ctx); err != nil {
		return err
	}
	if registered && !removed {
		winPrintln("⚠ Scheduled Task still registered: " + taskName)
	}
	return nil
}
