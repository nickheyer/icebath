package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// The gateway.py names the installer and its linger and legacy unit helpers call, a table tests replace like the patched module
var installHooks = struct {
	unitPath           func(ctx context.Context, system bool) (string, error)
	generateUnit       func(ctx context.Context, system bool, runAsUser string) (string, error)
	requireRoot        func(action string) error
	hasLegacy          func(ctx context.Context) bool
	removeLegacy       func(ctx context.Context, w io.Writer, interactive, dryRun bool) (int, []string, error)
	legacyWarning      func(ctx context.Context, w io.Writer)
	conflictWarning    func(ctx context.Context, w io.Writer) error
	prepareLauncher    func(ctx context.Context, system bool, runAsUser string) error
	ensureLinger       func(ctx context.Context, w io.Writer, username string, system bool) (bool, error)
	ensureSystemLinger func(ctx context.Context, w io.Writer, username string) error
	syncHome           func(ctx context.Context, system bool) error
	unitIsCurrent      func(ctx context.Context, system bool) (bool, error)
	refreshUnit        func(ctx context.Context, system bool) (bool, error)
	readUser           func(unitPath string) (string, error)
	getpwnam           func(name string) (pypwd.Passwd, error)
	waitUserBus        func(uid int, timeout time.Duration) bool
	unitIsActive       func(ctx context.Context, system bool) (bool, error)
	enableLinger       func(ctx context.Context, username string) (CommandResult, error)
	unitBelongs        func(ctx context.Context, unitPath string, system bool, action, runAsUser string) (bool, error)
	definitionBelongs  func(definitionPath, h, action string) (bool, error)
	supervising        func(ctx context.Context, label string) bool
	supervisedPID      func(ctx context.Context, label string) (int, bool)
	refuseTemp         func(definition, kind string) bool
	clearMarker        func(ctx context.Context)
	reloadBudget       func(ctx context.Context) (float64, error)
	appendReloadLog    func(ctx context.Context, message string)
	bootstrap          func(ctx context.Context, domain, plistPath, label string, timeout time.Duration) error
	refreshPlist       func(ctx context.Context) (bool, error)
}{
	unitPath: GetSystemdUnitPath, generateUnit: GenerateSystemdUnit, requireRoot: requireRootForSystemService,
	hasLegacy: hasLegacyHermesUnits, removeLegacy: removeLegacyHermesUnits, legacyWarning: printLegacyUnitWarning,
	conflictWarning: printSystemdScopeConflictWarning, prepareLauncher: prepareServiceLauncher,
	syncHome: SyncHermesHomeFromSystemdUnit, unitIsCurrent: SystemdUnitIsCurrent, refreshUnit: refreshSystemdUnitIfNeeded,
	readUser: readSystemdUserFromUnit, getpwnam: pypwd.Getpwnam, waitUserBus: waitForTargetUserBus, unitIsActive: SystemdUnitIsActive,
	enableLinger: loginctlEnableLinger, unitBelongs: unitBelongsToCaller,
	definitionBelongs: definitionBelongsToHome, supervising: LaunchctlLabelSupervisingProcess, supervisedPID: LaunchctlSupervisedPID,
	refuseTemp: refuseTempHomeServiceWrite, clearMarker: clearLaunchdUnsupportedMarker, reloadBudget: launchdReloadBudget,
	appendReloadLog: appendLaunchdReloadLog, bootstrap: launchctlBootstrap, refreshPlist: RefreshLaunchdPlistIfNeeded,
}

// Installs the linger helpers into the seam table, their bodies reading the table themselves
func init() {
	installHooks.ensureLinger, installHooks.ensureSystemLinger = ensureLingerEnabled, ensureSystemServiceLinger
}

// True when the wizard would trigger a system scope operation as non root, detected before prompting
func systemScopeWizardWouldNeedRoot(ctx context.Context, system bool) (bool, error) {
	if serviceGeteuid() == 0 {
		return false, nil
	}
	return SelectSystemdScope(ctx, system)
}

// Unit names older installs used for the gateway
var legacyServiceNames = []string{"icebath.service"}

// ExecStart markers identifying a unit as running the gateway, a legacy unit flagged only when one matches
var legacyUnitExecstartMarkers = []string{"icebath_cli.main gateway", "icebath_cli/main.py gateway", "gateway/run.py",
	" icebath gateway ", "/icebath gateway "}

// One directory scanned for legacy units and whether it is the system scope
type legacySearchPath struct {
	system bool
	base   string
}

// Directories scanned for legacy units, a seam tests replace
var legacyUnitSearchPaths = func(ctx context.Context) []legacySearchPath {
	return []legacySearchPath{{false, UserSystemdUnitDir(ctx)}, {true, systemUnitDir}}
}

// One legacy gateway unit found on disk
type legacyUnit struct {
	name, path string
	system     bool
}

// Legacy gateway units on disk whose ExecStart runs the gateway, unreadable files skipped
func findLegacyHermesUnits(ctx context.Context) []legacyUnit {
	var out []legacyUnit
	for _, dir := range legacyUnitSearchPaths(ctx) {
		for _, name := range legacyServiceNames {
			unitPath := filepath.Join(dir.base, name)
			if _, err := os.Stat(unitPath); err != nil {
				continue
			}
			raw, err := os.ReadFile(unitPath)
			if err != nil {
				continue
			}
			text := pystr.DecodeUTF8SigIgnore(raw)
			for _, marker := range legacyUnitExecstartMarkers {
				if strings.Contains(text, marker) {
					out = append(out, legacyUnit{name, unitPath, dir.system})
					break
				}
			}
		}
	}
	return out
}

// Whether any legacy gateway unit file exists
func hasLegacyHermesUnits(ctx context.Context) bool { return len(findLegacyHermesUnits(ctx)) > 0 }

// Warns about installed legacy gateway units, printing nothing when there are none
func printLegacyUnitWarning(ctx context.Context, w io.Writer) {
	legacy := findLegacyHermesUnits(ctx)
	if len(legacy) == 0 {
		return
	}
	con := serviceConsole(w)
	con.PrintWarning("Legacy Icebath gateway unit(s) detected from an older install:")
	for _, u := range legacy {
		con.PrintInfo(fmt.Sprintf("    %s  (%s scope)", u.path, ServiceScopeLabel(u.system)))
	}
	con.PrintInfo("  These run alongside the current icebath-gateway service and")
	con.PrintInfo("  cause SIGTERM flap loops — both try to use the same bot token.")
	con.PrintInfo("  Remove them with:")
	con.PrintInfo("    icebath gateway migrate-legacy")
}

// Stops, disables and removes legacy units, returning the removed count and the paths left behind
func removeLegacyHermesUnits(ctx context.Context, w io.Writer, interactive, dryRun bool) (int, []string, error) {
	legacy := findLegacyHermesUnits(ctx)
	if len(legacy) == 0 {
		fmt.Fprintln(w, "No legacy Icebath gateway units found.")
		return 0, nil, nil
	}
	paths := func() []string {
		out := make([]string, 0, len(legacy))
		for _, u := range legacy {
			out = append(out, u.path)
		}
		return out
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Legacy Icebath gateway unit(s) found:")
	for _, u := range legacy {
		fmt.Fprintf(w, "  %s  (%s scope)\n", u.path, ServiceScopeLabel(u.system))
	}
	fmt.Fprintln(w)
	if dryRun {
		fmt.Fprintln(w, "(dry-run — nothing removed)")
		return 0, paths(), nil
	}
	if interactive && !gatewayFacade.promptYesNo("Remove these legacy units?", true) {
		fmt.Fprintln(w, "Skipped. Run again with: icebath gateway migrate-legacy")
		return 0, paths(), nil
	}
	removed := 0
	var remaining []string
	removeUnits := func(units []legacyUnit, system bool) error {
		for _, u := range units {
			err := func() error {
				if _, err := lifecycleRunSystemctl(ctx, []string{"stop", u.name}, system, 90*time.Second); err != nil {
					return err
				}
				if _, err := lifecycleRunSystemctl(ctx, []string{"disable", u.name}, system, 30*time.Second); err != nil {
					return err
				}
				if err := os.Remove(u.path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				return nil
			}()
			if err != nil {
				if !legacyRemovalHandled(err) {
					return err
				}
				fmt.Fprintf(w, "  ⚠ Could not remove %s: %s\n", u.path, legacyRemovalText(err))
				remaining = append(remaining, u.path)
				continue
			}
			fmt.Fprintf(w, "  ✓ Removed %s\n", u.path)
			removed++
		}
		if _, err := lifecycleRunSystemctl(ctx, []string{"daemon-reload"}, system, 30*time.Second); err != nil && !runtimeErrorClass(err) {
			return err
		}
		return nil
	}
	var userUnits, systemUnits []legacyUnit
	for _, u := range legacy {
		if u.system {
			systemUnits = append(systemUnits, u)
		} else {
			userUnits = append(userUnits, u)
		}
	}
	if len(userUnits) > 0 {
		if err := removeUnits(userUnits, false); err != nil {
			return removed, remaining, err
		}
	}
	con := serviceConsole(w)
	if len(systemUnits) > 0 {
		if serviceGeteuid() != 0 {
			fmt.Fprintln(w)
			con.PrintWarning("System-scope legacy units require root to remove.")
			con.PrintInfo("  Re-run with: sudo icebath gateway migrate-legacy")
			for _, u := range systemUnits {
				remaining = append(remaining, u.path)
			}
		} else if err := removeUnits(systemUnits, true); err != nil {
			return removed, remaining, err
		}
	}
	fmt.Fprintln(w)
	if len(remaining) > 0 {
		con.PrintWarning(fmt.Sprintf("%d legacy unit(s) still present — see messages above.", len(remaining)))
	} else {
		con.PrintSuccess(fmt.Sprintf("Removed %d legacy unit(s).", removed))
	}
	return removed, remaining, nil
}

// Whether a unit removal failure is the OSError or RuntimeError class the removal reports and survives
func legacyRemovalHandled(err error) bool {
	var pathErr *os.PathError
	return errors.As(err, &pathErr) || runtimeErrorClass(err)
}

// Python str of a handled removal failure, the OSError text of a file error
func legacyRemovalText(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// Whether an error is a RuntimeError subclass of the service helpers, the missing systemctl or a timeout
func runtimeErrorClass(err error) bool {
	var unavailable SystemctlUnavailableError
	return errors.As(err, &unavailable)
}

// Prints the manual linger guidance after an automatic enable could not run or failed
func printLingerEnableWarning(ctx context.Context, w io.Writer, username, detail string, system bool) error {
	fmt.Fprintln(w)
	if system {
		fmt.Fprintf(w, "⚠ Linger not enabled for %s — cron and Kanban workers cannot start (no user D-Bus).\n", username)
	} else {
		fmt.Fprintln(w, "⚠ Linger not enabled — gateway may stop when you close this terminal.")
	}
	if detail != "" {
		fmt.Fprintf(w, "  Auto-enable failed: %s\n", detail)
	}
	fmt.Fprintln(w)
	if system {
		fmt.Fprintln(w, "  Enable it manually:")
	} else {
		fmt.Fprintln(w, "  On headless servers (VPS, cloud instances) run:")
	}
	fmt.Fprintf(w, "    sudo loginctl enable-linger %s\n", username)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Then restart the gateway:")
	sudo, _, userFlag := systemdCLIBits(system)
	name, err := GetServiceName(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "    %ssystemctl %srestart %s.service\n", sudo, userFlag, name)
	fmt.Fprintln(w)
	return nil
}

// Enables linger for the user when possible, true only when this call enabled it
func ensureLingerEnabled(ctx context.Context, w io.Writer, username string, system bool) (bool, error) {
	if hostIsTermux() || !IsLinux() {
		return false, nil
	}
	if username == "" {
		var err error
		if username, err = lifecycleGetuser(); err != nil {
			return false, err
		}
	}
	enabledMsg := "✓ Systemd linger is enabled (service survives logout)"
	if system {
		enabledMsg = fmt.Sprintf("✓ Systemd linger is enabled for %s (worker D-Bus available)", username)
	}
	if lifecyclePathExists("/var/lib/systemd/linger/" + username) {
		fmt.Fprintln(w, enabledMsg)
		return false, nil
	}
	enabled, detail := lifecycleLingerStatus(ctx, username)
	if enabled != nil && *enabled {
		fmt.Fprintln(w, enabledMsg)
		return false, nil
	}
	if _, err := serviceLookPath("loginctl"); err != nil {
		if detail == "" {
			detail = "loginctl not found"
		}
		return false, printLingerEnableWarning(ctx, w, username, detail, system)
	}
	if system {
		fmt.Fprintf(w, "Enabling linger for %s so cron and Kanban workers can reach systemd-run --user...\n", username)
	} else {
		fmt.Fprintln(w, "Enabling linger so the gateway survives SSH logout...")
	}
	res, err := installHooks.enableLinger(ctx, username)
	if err != nil {
		return false, printLingerEnableWarning(ctx, w, username, pythonExceptionText(err), system)
	}
	if res.ReturnCode != 0 {
		failure := completedProcessDetail(res)
		if failure == "" {
			failure = detail
		}
		return false, printLingerEnableWarning(ctx, w, username, failure, system)
	}
	if system {
		fmt.Fprintf(w, "✓ Enabled linger for %s\n", username)
	} else {
		fmt.Fprintln(w, "✓ Linger enabled — gateway will persist after logout")
	}
	return true, nil
}

// Enables linger for a system unit's account, waits for its user bus and asks to restart a bus less gateway
func ensureSystemServiceLinger(ctx context.Context, w io.Writer, username string) error {
	enabled, err := installHooks.ensureLinger(ctx, w, username, true)
	if err != nil || !enabled {
		return err
	}
	pw, err := installHooks.getpwnam(username)
	if err != nil {
		return err
	}
	if installHooks.waitUserBus(pw.UID, 5*time.Second) {
		fmt.Fprintf(w, "✓ /run/user/%d/bus is up — cron and Kanban workers can use systemd-run --user\n", pw.UID)
	} else {
		fmt.Fprintf(w, "⚠ /run/user/%d/bus did not appear within 5s.\n", pw.UID)
		fmt.Fprintf(w, "  Start the user manager: sudo systemctl start user@%d.service\n", pw.UID)
	}
	active, err := installHooks.unitIsActive(ctx, true)
	if err != nil || !active {
		return err
	}
	name, err := GetServiceName(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "  The running gateway was started without a user D-Bus; restart it to pick one up:")
	fmt.Fprintf(w, "    sudo systemctl restart %s.service\n", name)
	return nil
}

// Installs or repairs the systemd gateway unit of a scope, enabling it on startup and ensuring linger
func systemdInstall(ctx context.Context, w io.Writer, force, system bool, runAsUser string, enableOnStartup, nonInteractive, forceUnitPath bool) error {
	if system {
		if err := installHooks.requireRoot("install"); err != nil {
			return err
		}
	}
	unitPath, err := installHooks.unitPath(ctx, system)
	if err != nil {
		return err
	}
	if !forceUnitPath {
		owns, err := installHooks.unitBelongs(ctx, unitPath, system, "overwrite", runAsUser)
		if err != nil {
			return err
		}
		if !owns {
			return &SystemExitError{Code: 1}
		}
	}
	if installHooks.hasLegacy(ctx) {
		fmt.Fprintln(w)
		installHooks.legacyWarning(ctx, w)
		fmt.Fprintln(w)
		if nonInteractive || gatewayFacade.promptYesNo("Remove the legacy unit(s) before installing?", true) {
			if _, _, err := installHooks.removeLegacy(ctx, w, false, false); err != nil {
				return err
			}
			fmt.Fprintln(w)
		}
	}
	scopeLabel := ServiceScopeLabel(system)
	sudo, scopeFlag, userFlag := systemdCLIBits(system)
	exists := pathExistsSafe(unitPath)
	if exists && !forceUnitPath {
		if err := installHooks.syncHome(ctx, system); err != nil {
			return err
		}
	}
	name, err := GetServiceName(ctx)
	if err != nil {
		return err
	}
	if exists && !(force || forceUnitPath) {
		current, err := installHooks.unitIsCurrent(ctx, system)
		if err != nil {
			return err
		}
		if !current {
			fmt.Fprintf(w, "↻ Repairing outdated %s systemd service at: %s\n", scopeLabel, unitPath)
			if _, err := installHooks.refreshUnit(ctx, system); err != nil {
				return err
			}
			if enableOnStartup {
				if _, err := runSystemctlChecked(ctx, []string{"enable", name}, system, 30*time.Second); err != nil {
					return err
				}
			}
			fmt.Fprintf(w, "✓ %s service definition updated\n", pystr.Capitalize(scopeLabel))
		} else {
			fmt.Fprintf(w, "Service already installed at: %s\n", unitPath)
			fmt.Fprintln(w, "Use --force to reinstall")
			if enableOnStartup {
				res, err := lifecycleRunSystemctl(ctx, []string{"enable", name}, system, 30*time.Second)
				if err != nil {
					return err
				}
				if res.ReturnCode != 0 {
					serviceConsole(w).PrintWarning(fmt.Sprintf("could not enable %s at boot (systemctl enable exited %d); a reboot may come up with no gateway",
						name, res.ReturnCode))
				}
			}
		}
		configuredUser := ""
		if system {
			if configuredUser, err = installHooks.readUser(unitPath); err != nil {
				return err
			}
		}
		if configuredUser != "" {
			return installHooks.ensureSystemLinger(ctx, w, configuredUser)
		}
		if !system {
			_, err := installHooks.ensureLinger(ctx, w, "", false)
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o777); err != nil {
		return err
	}
	newUnit, err := installHooks.generateUnit(ctx, system, runAsUser)
	if err != nil {
		return err
	}
	if installHooks.refuseTemp(newUnit, "systemd unit") {
		return nil
	}
	fmt.Fprintf(w, "Installing %s systemd service to: %s\n", scopeLabel, unitPath)
	if err := installHooks.prepareLauncher(ctx, system, runAsUser); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(newUnit), 0o666); err != nil {
		return err
	}
	if _, err := runSystemctlChecked(ctx, []string{"daemon-reload"}, system, 30*time.Second); err != nil {
		return err
	}
	if enableOnStartup {
		if _, err := runSystemctlChecked(ctx, []string{"enable", name}, system, 30*time.Second); err != nil {
			return err
		}
	}
	done := "installed"
	if enableOnStartup {
		done = "installed and enabled"
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "✓ %s service %s!\n", pystr.Capitalize(scopeLabel), done)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next steps:")
	fmt.Fprintf(w, "  %sicebath gateway start%s              # Start the service\n", sudo, scopeFlag)
	fmt.Fprintf(w, "  %sicebath gateway status%s             # Check status\n", sudo, scopeFlag)
	fmt.Fprintf(w, "  journalctl %s-u %s -f  # View logs\n", userFlag, name)
	fmt.Fprintln(w)
	if system {
		configuredUser, err := installHooks.readUser(unitPath)
		if err != nil {
			return err
		}
		if configuredUser != "" {
			fmt.Fprintf(w, "Configured to run as: %s\n", configuredUser)
			if err := installHooks.ensureSystemLinger(ctx, w, configuredUser); err != nil {
				return err
			}
		}
	} else if _, err := installHooks.ensureLinger(ctx, w, "", false); err != nil {
		return err
	}
	if err := installHooks.conflictWarning(ctx, w); err != nil {
		return err
	}
	installHooks.legacyWarning(ctx, w)
	return nil
}

// Installs or repairs the launchd LaunchAgent, loading it unless the install was asked not to start the gateway
func launchdInstall(ctx context.Context, w io.Writer, force, startNow, forceUnitPath bool) error {
	plistPath, err := launchdPlistPathOf(ctx)
	if err != nil {
		return err
	}
	label, err := launchdLabelOf(ctx)
	if err != nil {
		return err
	}
	if !forceUnitPath {
		owns, err := installHooks.definitionBelongs(plistPath, home.Dir(ctx), "overwrite")
		if err != nil {
			return err
		}
		if !owns {
			return &SystemExitError{Code: 1}
		}
	}
	load := startNow || installHooks.supervising(ctx, label)
	forceOldPid, haveOld := 0, false
	if force && load {
		forceOldPid, haveOld = installHooks.supervisedPID(ctx, label)
	}
	if pathExistsSafe(plistPath) && !(force || forceUnitPath) {
		current, err := launchdPlistIsCurrentFn(ctx)
		if err != nil {
			return err
		}
		if current {
			fmt.Fprintf(w, "Service already installed at: %s\n", plistPath)
			fmt.Fprintln(w, "Use --force to reinstall")
			return nil
		}
		if load {
			fmt.Fprintf(w, "↻ Repairing outdated launchd service at: %s\n", plistPath)
			reloaded, err := installHooks.refreshPlist(ctx)
			if err != nil {
				return err
			}
			if reloaded {
				fmt.Fprintln(w, "✓ Service definition updated")
			} else {
				fmt.Fprintf(w, "⚠ Service definition could not be reloaded with launchd. "+
					"Run 'icebath gateway install --force' or check "+
					"%s/logs/launchd-reload.log for details.\n", home.Display(home.Dir(ctx)))
			}
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o777); err != nil {
		return err
	}
	newPlist, err := launchdGeneratePlist(ctx)
	if err != nil {
		return err
	}
	if installHooks.refuseTemp(newPlist, "launchd plist") {
		return nil
	}
	fmt.Fprintf(w, "Installing launchd service to: %s\n", plistPath)
	if err := installHooks.prepareLauncher(ctx, false, ""); err != nil {
		return err
	}
	if err := os.WriteFile(plistPath, []byte(newPlist), 0o666); err != nil {
		return err
	}
	domain, err := launchdDomainOf(ctx)
	if err != nil {
		return err
	}
	if !load {
		if _, err := launchctlRun(ctx, 90*time.Second, true, "launchctl", "bootout", domain+"/"+label); err != nil {
			return err
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "✓ Service installed, not started (launchd starts it at your next login)")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Next steps:")
		fmt.Fprintln(w, "  icebath gateway start              # Start it now")
		fmt.Fprintln(w, "  icebath gateway status             # Check status")
		return nil
	}
	if haveOld {
		target := domain + "/" + label
		budget, err := installHooks.reloadBudget(ctx)
		if err != nil {
			return err
		}
		registered, err := bootoutDrainAndRebootstrap(ctx, domain, plistPath, label, forceOldPid, true, budget)
		if err != nil {
			return err
		}
		if !registered {
			installHooks.appendReloadLog(ctx, fmt.Sprintf("FAILED forced launchd install of %s — service NOT registered after "+
				"retrying for %ds", target, int(budget)))
			serviceConsole(w).PrintError(fmt.Sprintf("launchctl bootstrap did not register %s within %ds; "+
				"the gateway service is NOT loaded.", target, int(budget)))
			return &SystemExitError{Code: 1}
		}
	} else if err := installHooks.bootstrap(ctx, domain, plistPath, label, 30*time.Second); err != nil {
		var cpe *CalledProcessError
		if errors.As(err, &cpe) {
			return launchdDegradeOrRaise(ctx, cpe, "launchctl bootstrap")
		}
		return err
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "✓ Service installed and loaded!")
	installHooks.clearMarker(ctx)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next steps:")
	fmt.Fprintln(w, "  icebath gateway status             # Check status")
	fmt.Fprintf(w, "  tail -f %s/logs/gateway.log  # View logs\n", home.Display(home.Dir(ctx)))
	return nil
}
