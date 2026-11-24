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
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	userutil "github.com/nickheyer/icebath/pkg/utils/user"
)

// The gateway.py names the installer and its linger and legacy unit helpers call, a table tests replace like the patched module
var installHooks = struct {
	unitPath           func(ctx context.Context, system bool) (string, error)
	generateUnit       func(ctx context.Context, system bool, runAsUser string) (string, error)
	requireRoot        func(action string) error
	conflictWarning    func(ctx context.Context, w io.Writer) error
	prepareLauncher    func(ctx context.Context, system bool, runAsUser string) error
	ensureLinger       func(ctx context.Context, w io.Writer, username string, system bool) (bool, error)
	ensureSystemLinger func(ctx context.Context, w io.Writer, username string) error
	syncHome           func(ctx context.Context, system bool) error
	unitIsCurrent      func(ctx context.Context, system bool) (bool, error)
	refreshUnit        func(ctx context.Context, system bool) (bool, error)
	readUser           func(unitPath string) (string, error)
	getpwnam           func(name string) (userutil.Account, error)
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
	conflictWarning: printSystemdScopeConflictWarning, prepareLauncher: prepareServiceLauncher,
	syncHome: SyncIcebathHomeFromSystemdUnit, unitIsCurrent: SystemdUnitIsCurrent, refreshUnit: refreshSystemdUnitIfNeeded,
	readUser: readSystemdUserFromUnit, getpwnam: userutil.Lookup, waitUserBus: waitForTargetUserBus, unitIsActive: SystemdUnitIsActive,
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
func findLegacyIcebathUnits(ctx context.Context) []legacyUnit {
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
			text := strings.ToValidUTF8(string(textutil.TrimBOM(raw)), "")
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
		return false, printLingerEnableWarning(ctx, w, username, err.Error(), system)
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
			fmt.Fprintf(w, "✓ %s service definition updated\n", textutil.UpperFirst(strings.ToLower(scopeLabel)))
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
	fmt.Fprintf(w, "✓ %s service %s!\n", textutil.UpperFirst(strings.ToLower(scopeLabel)), done)
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
	return installHooks.conflictWarning(ctx, w)
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
	if _, _, _, err := removeRetired(ctx, w, retiredLaunchdServices, removeRetiredLaunchdService); err != nil {
		return err
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
