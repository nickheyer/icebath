package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Scope picker of the setup install, a seam tests replace like the patched gateway module
var setupPromptInstallScope = promptLinuxGatewayInstallScope

// Prints the container guidance and reports true when a fresh user scope unit was asked for inside a container
func refusesContainerUserScopeInstall(w io.Writer, system bool) bool {
	if system || !setupIsContainer() {
		return false
	}
	con := serviceConsole(w)
	con.PrintError("Refusing to install a user-scope systemd gateway service inside a container.")
	for _, line := range []string{
		"The unit file and its enable symlink would be written to the home directory, which is",
		"commonly the host's own home bind-mounted in — the host's user manager then enables and",
		"starts the same unit, so a second gateway polls the same bot token outside the container",
		"(Telegram: 'Conflict: terminated by other getUpdates request').",
		"",
		"  icebath gateway run                                # run as the container's main process",
		"  docker run --restart unless-stopped ...           # container restart policy",
		"",
		"If systemd manages this container (systemd as PID 1), install an isolated system service instead:",
		"  sudo icebath gateway install --system --run-as-user <user>",
	} {
		con.PrintInfo(line)
	}
	return true
}

// First of SUDO_USER, USER and LOGNAME naming an account other than root, empty when none does
func defaultSystemServiceUser() string {
	for _, key := range []string{"SUDO_USER", "USER", "LOGNAME"} {
		if candidate := pystr.Strip(os.Getenv(key)); candidate != "" && candidate != "root" {
			return candidate
		}
	}
	return ""
}

// Background scope the user picks, system offered only to root, empty when the install is skipped
func promptLinuxGatewayInstallScope(w io.Writer) string {
	isRoot := serviceGeteuid() == 0
	options := []string{"User service (no sudo; best for laptops/dev boxes; may need linger after logout)"}
	values := []string{"user"}
	if isRoot {
		options = append(options, "System service (starts on boot; runs as your chosen user)")
		values = append(values, "system")
	}
	options = append(options, "Skip service install for now")
	values = append(values, "")
	choice := gatewayFacade.promptChoice("  Choose how the gateway should run in the background:", options, 0)
	if !isRoot && choice == 0 {
		serviceConsole(w).PrintInfo("  Tip: for a boot-time system service, re-run setup as root (e.g. from a root shell or `sudo -i`).")
	}
	return values[choice]
}

// Installs the systemd service at the picked scope, returning the scope and whether an install ran
func installLinuxGatewayFromSetup(ctx context.Context, w io.Writer, force, enableOnStartup bool) (string, bool, error) {
	scope := setupPromptInstallScope(w)
	if scope == "" {
		return "", false, nil
	}
	con := serviceConsole(w)
	if scope == "system" {
		runAsUser := defaultSystemServiceUser()
		if serviceGeteuid() != 0 {
			con.PrintWarning("  System service install requires root. Re-run setup from a " +
				"root shell, or install a user service instead: icebath gateway install")
			return scope, false, nil
		}
		for runAsUser == "" {
			runAsUser = pystr.Strip(gatewayFacade.prompt("  Run the system gateway service as which user?", "", false))
			if runAsUser == "" {
				con.PrintError("  Enter a username.")
			}
		}
		if err := setupSystemdInstall(ctx, w, force, true, runAsUser, enableOnStartup, false, false); err != nil {
			return scope, false, err
		}
		return scope, true, nil
	}
	if refusesContainerUserScopeInstall(w, false) {
		return scope, false, nil
	}
	if err := setupSystemdInstall(ctx, w, force, false, "", enableOnStartup, false, false); err != nil {
		return scope, false, err
	}
	return scope, true, nil
}

// Host service manager in the predicate order every subcommand routes on, empty when none applies
func serviceBackendName(ctx context.Context, windows bool) string {
	switch {
	case setupSupportsSystemd(ctx):
		return "systemd"
	case setupIsMacOS():
		return "launchd"
	case windows && setupIsWindows():
		return "windows"
	}
	return ""
}

// Runs start, stop, restart or uninstall on the backend, systemd alone taking the scope
func serviceCall(ctx context.Context, backend, verb string, system bool) error {
	switch backend {
	case "windows":
		switch verb {
		case "start":
			return WindowsStart(ctx)
		case "stop":
			return WindowsStop(ctx)
		case "restart":
			return WindowsRestart(ctx)
		case "uninstall":
			return WindowsUninstall(ctx)
		}
		return fmt.Errorf("AttributeError: module 'icebath_cli.gateway_windows' has no attribute '%s'", verb)
	case "launchd":
		switch verb {
		case "start":
			return LaunchdStart(ctx)
		case "stop":
			return LaunchdStop(ctx)
		case "restart":
			return LaunchdRestart(ctx)
		case "uninstall":
			return LaunchdUninstall(ctx)
		}
		return fmt.Errorf("KeyError: 'launchd_%s'", verb)
	}
	switch verb {
	case "start":
		return SystemdStart(ctx, system)
	case "stop":
		return SystemdStop(ctx, system)
	case "restart":
		return SystemdRestart(ctx, system)
	case "uninstall":
		return SystemdUninstall(ctx, system)
	}
	return fmt.Errorf("KeyError: 'systemd_%s'", verb)
}

// Runs a wizard service start or restart, printing remediation instead of failing on the scope errors
func setupServiceAction(ctx context.Context, w io.Writer, action, failedLabel string, windows, system bool) error {
	con := serviceConsole(w)
	err := func() error {
		if backend := serviceBackendName(ctx, windows); backend != "" {
			return serviceCall(ctx, backend, action, system && action != "restart")
		}
		if action == "restart" && windows {
			if _, err := StopProfileGateway(ctx); err != nil {
				return err
			}
			con.PrintInfo("Start manually: icebath gateway")
		}
		return nil
	}()
	var unavailable *UserSystemdUnavailableError
	var needsRoot *SystemScopeRequiresRootError
	var called *CalledProcessError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &unavailable):
		con.PrintError(fmt.Sprintf("  %s — user systemd not reachable:", failedLabel))
		printIndented(unavailable.Error(), func(line string) { fmt.Fprintln(w, line) })
		return nil
	case errors.As(err, &needsRoot):
		con.PrintError(fmt.Sprintf("  %s: %s", failedLabel, needsRoot.Error()))
		return printSystemScopeRemediation(ctx, con, action)
	case errors.As(err, &called):
		con.PrintError(fmt.Sprintf("  %s: %s", failedLabel, called.Error()))
		return nil
	}
	return err
}
