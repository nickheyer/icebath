package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/settings"

	"github.com/nickheyer/icebath/pkg/internal/textutil"
)

// Consent question before the setup flow installs the gateway service
const ServiceInstallQuestion = "Install the gateway service so Icebath starts automatically at login " +
	"and keeps scheduled jobs and messaging available?"

// Notice of the standalone opt out every refusal of a profile gateway prints
const standaloneDeprecationNotice = "gateway.standalone is a temporary compatibility shim while multiplexing gaps are fixed; " +
	"it will be removed once they are — plan to fold this profile with `icebath gateway migrate --multiplex`."

// Keyword arguments of the service setup, an empty Context meaning setup
type ServiceSetupOptions struct {
	Context     string
	Interactive bool
	Install     bool
}

// ValueError of a service choice that is neither install nor decline
type ServiceChoiceError struct{ Msg string }

func (e *ServiceChoiceError) Error() string      { return e.Msg }
func (e *ServiceChoiceError) PyTypeName() string { return "ValueError" }

// Console the setup helpers print through
func serviceConsole(w io.Writer) *cliui.Console {
	return &cliui.Console{In: cliui.Default().In, Out: w}
}

// Host and service manager probes of the setup flow, seams tests replace like the patched gateway module
var (
	setupIsContainer      = hostinfo.IsContainer
	setupSupportsSystemd  = SupportsSystemdServices
	setupIsMacOS          = IsMacOS
	setupIsWindows        = IsWindows
	setupServiceRunning   = IsServiceRunning
	setupServiceInstalled = isServiceInstalled
	setupNeedsNoService   = servedProfileNeedsNoService
	setupConflictingUnits = hasConflictingSystemdUnits
	setupConflictWarning  = printSystemdScopeConflictWarning
	setupSystemdInstall   = systemdInstall
	setupLaunchdInstall   = launchdInstall
	setupWindowsInstall   = WindowsInstall
	setupSystemdStart     = SystemdStart
	setupLaunchdStart     = LaunchdStart
	setupWindowsStart     = WindowsStart
	setupStdinIsTTY       = func() bool { return cliui.Default().StdinIsTTY() }
	setupPromptConsent    = func(_ context.Context, question string, def bool) bool {
		return gatewayFacade.promptYesNo(question, def)
	}
	setupInstallLinuxScope = installLinuxGatewayFromSetup
	setupServiceActionFn   = setupServiceAction
)

// Saves the durable service install choice of the profile ctx names, gateway.service_install_choice
func RecordServiceChoice(ctx context.Context, choice string) error {
	if choice != "install" && choice != "decline" {
		return &ServiceChoiceError{"service_install_choice must be install or decline"}
	}
	_, err := settings.Set(ctx, settings.GatewayServiceInstallChoice.Key, choice, settings.Global)
	return err
}

// Whether setup may install the service, asking once on an interactive terminal and recording a decline
func WantsServiceInstall(ctx context.Context, interactive, install bool) (bool, error) {
	if install {
		return true, nil
	}
	if !interactive || !setupStdinIsTTY() {
		return false, nil
	}
	choice := settings.GatewayServiceInstallChoice.Get(ctx)
	if choice == "" {
		if setupPromptConsent(ctx, ServiceInstallQuestion, false) {
			return true, nil
		}
		if err := RecordServiceChoice(ctx, "decline"); err != nil {
			return false, err
		}
	}
	return choice == "install", nil
}

// Starts an existing gateway service and installs one only with explicit setup consent, false leaving startup to the caller
func EnsureGatewayService(ctx context.Context, w io.Writer, o ServiceSetupOptions) (bool, error) {
	con := serviceConsole(w)
	if setupIsContainer() {
		con.PrintInfo("Run icebath gateway run as the container main process; use a Docker restart policy for persistence.")
		return false, nil
	}
	supportsSystemd := setupSupportsSystemd(ctx)
	if !(supportsSystemd || setupIsMacOS() || setupIsWindows()) {
		con.PrintInfo("No supported service manager found. Run: icebath gateway run")
		return false, nil
	}
	err := func() error {
		running, err := setupServiceRunning(ctx)
		if err != nil || running {
			return errOr(err, errServiceReady)
		}
		served, err := setupNeedsNoService(ctx, w)
		if err != nil || served {
			return errOr(err, errServiceReady)
		}
		installed, err := setupServiceInstalled(ctx)
		if err != nil {
			return err
		}
		if !installed {
			wants := false
			if o.Context != "import" {
				if wants, err = WantsServiceInstall(ctx, o.Interactive, o.Install); err != nil {
					return err
				}
			}
			if !wants {
				con.PrintInfo("No service installed. Run: icebath gateway run")
				con.PrintInfo(serviceAbsentNotice("Without a service, scheduled jobs and messaging stop at logout/reboot; jobs cannot run while the host is off."))
				return errServiceDeclined
			}
			if supportsSystemd {
				conflicting, err := setupConflictingUnits(ctx)
				if err != nil {
					return err
				}
				if conflicting {
					if err := setupConflictWarning(ctx, w); err != nil {
						return err
					}
					return errServiceDeclined
				}
			}
			switch {
			case supportsSystemd:
				err = setupSystemdInstall(ctx, w, false, false, "", true, true, false)
			case setupIsMacOS():
				err = setupLaunchdInstall(ctx, w, false, true, false)
			default:
				err = setupWindowsInstall(ctx, WindowsInstallOptions{Force: false, StartNow: optBool(true), StartOnLogin: optBool(true)})
			}
			if err != nil {
				return err
			}
			if installed, err = setupServiceInstalled(ctx); err != nil {
				return err
			}
			if !installed {
				con.PrintWarning("Gateway service install did not complete. Retry: icebath gateway install")
				return errServiceDeclined
			}
			if err := RecordServiceChoice(ctx, "install"); err != nil {
				return err
			}
		}
		switch {
		case supportsSystemd:
			err = setupSystemdStart(ctx, false)
		case setupIsMacOS():
			err = setupLaunchdStart(ctx)
		default:
			err = setupWindowsStart(ctx)
		}
		if err != nil {
			return err
		}
		con.PrintSuccess("Gateway service running (cron jobs + messaging platforms).")
		return errServiceReady
	}()
	if errors.Is(err, errServiceReady) {
		return true, nil
	}
	if errors.Is(err, errServiceDeclined) {
		return false, nil
	}
	var unavailable *UserSystemdUnavailableError
	var needsRoot *SystemScopeRequiresRootError
	var exit *SystemExitError
	switch {
	case errors.As(err, &unavailable):
		con.PrintWarning("Could not reach user systemd to start the gateway service:")
		printIndented(unavailable.Error(), con.PrintInfo)
	case errors.As(err, &needsRoot):
		con.PrintWarning("Gateway service needs root for this scope: " + needsRoot.Error())
		if rerr := printSystemScopeRemediation(ctx, con, "start"); rerr != nil {
			return false, rerr
		}
	case errors.As(err, &exit):
		con.PrintWarning("Gateway service install did not complete. Retry: icebath gateway install")
	default:
		pkgLog().Debug("gateway service setup failed", "err", err)
		con.PrintWarning("Gateway service setup failed: " + err.Error())
		con.PrintInfo("You can retry manually: icebath gateway install")
	}
	return false, nil
}

// Outcomes of the setup body that are not failures
var (
	errServiceReady    = errors.New("gateway service ready")
	errServiceDeclined = errors.New("gateway service left to the caller")
)

// The error when there is one, the outcome otherwise
func errOr(err, outcome error) error {
	if err != nil {
		return err
	}
	return outcome
}

// Pointer to a keyword boolean of the Windows installer
func optBool(b bool) *bool { return &b }

// Whether the gateway service is running, an active unit, a registered launchd job or a live gateway process
func IsServiceRunning(ctx context.Context) (bool, error) {
	if SupportsSystemdServices(ctx) {
		for _, system := range []bool{false, true} {
			active, err := SystemdUnitIsActive(ctx, system)
			if err != nil || active {
				return active, err
			}
		}
		return false, nil
	}
	if IsMacOS() {
		plist, err := GetLaunchdPlistPath(ctx)
		if err != nil {
			return false, err
		}
		if _, err := os.Stat(plist); err == nil {
			label, err := GetLaunchdLabel(ctx)
			if err != nil {
				return false, err
			}
			registered, err := LaunchdServiceRegistered(ctx, label, 10*time.Second)
			if errors.Is(err, ErrCommandTimeout) {
				return false, nil
			}
			return registered, err
		}
	}
	pids, err := FindGatewayPIDsFor(ctx, nil, false)
	return len(pids) > 0, err
}

// Whether a unit at either scope serves this home, a unit pinned to another install's home not counting
func systemdUnitInstalled(ctx context.Context) (bool, error) {
	if !SupportsSystemdServices(ctx) {
		return false, nil
	}
	for _, system := range []bool{false, true} {
		path, err := GetSystemdUnitPath(ctx, system)
		if err != nil {
			return false, err
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		foreign, err := ForeignSystemdUnitHome(ctx, system)
		if err != nil {
			return false, err
		}
		if foreign == "" {
			return true, nil
		}
	}
	return false, nil
}

// Installed service kind whose unit, plist or task exists, the Windows check run last
func installedServiceKindFor(ctx context.Context, windows func() (bool, error)) (string, error) {
	unit, err := systemdUnitInstalled(ctx)
	if err != nil {
		return "", err
	}
	if unit {
		return "systemd", nil
	}
	if IsMacOS() {
		plist, err := GetLaunchdPlistPath(ctx)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(plist); err == nil {
			return "launchd", nil
		}
	}
	ok, err := windows()
	if err != nil || !ok {
		return "", err
	}
	return "windows", nil
}

// Installed service kind of this home, stricter than the service backend since the unit, plist or task must exist
func InstalledGatewayServiceKind(ctx context.Context) (string, bool, error) {
	kind, err := installedServiceKindFor(ctx, func() (bool, error) {
		if !IsWindows() {
			return false, nil
		}
		return WindowsIsInstalled(ctx)
	})
	return kind, kind != "", err
}

// Whether a gateway service is installed for this home
func isServiceInstalled(ctx context.Context) (bool, error) {
	_, installed, err := InstalledGatewayServiceKind(ctx)
	return installed, err
}

// Whether user and system gateway units are both installed
func hasConflictingSystemdUnits(ctx context.Context) (bool, error) {
	scopes, err := GetInstalledSystemdScopes(ctx)
	return len(scopes) > 1, err
}

// Warns that user and system gateway units are both installed
func printSystemdScopeConflictWarning(ctx context.Context, w io.Writer) error {
	scopes, err := GetInstalledSystemdScopes(ctx)
	if err != nil || len(scopes) < 2 {
		return err
	}
	con := serviceConsole(w)
	con.PrintWarning("Both user and system gateway services are installed (" + strings.Join(scopes, " + ") + ").")
	con.PrintInfo("  This is confusing and can make start/stop/status behavior ambiguous.")
	con.PrintInfo("  Default gateway commands target the user service unless you pass --system.")
	con.PrintInfo("  Keep one of these:")
	con.PrintInfo("    icebath gateway uninstall")
	con.PrintInfo("    sudo icebath gateway uninstall --system")
	return nil
}

// Remediation printed when a system scope action needs root
func printSystemScopeRemediation(ctx context.Context, con *cliui.Console, action string) error {
	name, err := GetServiceName(ctx)
	if err != nil {
		return err
	}
	con.PrintWarning("Gateway is installed as a system-wide service — " + action + " requires root.")
	con.PrintInfo("  Options:")
	con.PrintInfo("    1. " + pystr.Capitalize(action) + " it this time:")
	con.PrintInfo("         sudo systemctl " + action + " " + name)
	con.PrintInfo("    2. Switch to a per-user service (recommended for personal use):")
	con.PrintInfo("         sudo icebath gateway uninstall --system")
	con.PrintInfo("         icebath gateway install")
	con.PrintInfo("         icebath gateway start")
	return nil
}

// Emits every line of text indented by two spaces
func printIndented(text string, emit func(string)) {
	for _, line := range textutil.Lines(text) {
		emit("  " + line)
	}
}

// The live host gateway when it serves the profile and was launched by this tenant, nil otherwise
func hostMultiplexerServing(ctx context.Context, profileName *string) *HostGateway {
	name := ""
	if profileName != nil {
		name = *profileName
	} else {
		current, err := currentProfileName(ctx)
		if err != nil {
			pkgLog().Debug("Host multiplexer probe failed", "err", err)
			return nil
		}
		name = current
	}
	if name == "" {
		name = "default"
	}
	g := hostGatewayServingProfile(name, 0)
	if g == nil {
		return nil
	}
	other, err := launchedByOtherTenant(g.Home, home.Dir(ctx))
	if err != nil {
		pkgLog().Debug("Host multiplexer probe failed", "err", err)
		return nil
	}
	if other {
		pkgLog().Debug(fmt.Sprintf("Host gateway %s belongs to another Icebath home; not ours", g.Describe()))
		return nil
	}
	return g
}

// The host gateway serving the profile when it is not this home's own process
func servedByAnotherHostGateway(ctx context.Context, profileName *string) *HostGateway {
	g := hostMultiplexerServing(ctx, profileName)
	if g == nil {
		return nil
	}
	same, err := SameIcebathHome(g.Home, processIcebathHome())
	if err != nil {
		pkgLog().Debug("Host multiplexer home comparison failed", "err", err)
	} else if same {
		return nil
	}
	return g
}

// Whether a live default multiplexer already serves this named profile, a name that cannot normalize failing
func namedProfileServedByRunningMultiplexer(ctx context.Context, profileName *string) (bool, error) {
	suffix := ""
	if profileName != nil {
		suffix = *profileName
	} else {
		current, err := currentProfileName(ctx)
		if err != nil {
			return false, nil
		}
		suffix = current
	}
	if suffix == "" || suffix == "default" {
		return false, nil
	}
	if g := hostMultiplexerServing(ctx, &suffix); g != nil {
		label, err := normalizedProfile(g.ProfileLabel())
		if err != nil {
			return false, err
		}
		want, err := normalizedProfile(suffix)
		if err != nil {
			return false, err
		}
		if label != want {
			return true, nil
		}
	}
	defaultRoot := home.Root()
	served, err := func() (bool, error) {
		_, live, err := LiveDefaultGatewayPid()
		if err != nil || !live {
			return false, err
		}
		recorded, ok, err := RecordedServedProfiles(defaultRoot)
		if err != nil {
			return false, err
		}
		if ok {
			want, err := normalizedProfile(suffix)
			if err != nil {
				return false, err
			}
			for _, p := range recorded {
				got, err := normalizedProfile(p)
				if err != nil {
					return false, err
				}
				if got == want {
					return true, nil
				}
			}
			return false, nil
		}
		flag, err := ExplicitMultiplexFlag(ctx, defaultRoot)
		return flag != nil && *flag, err
	}()
	if err != nil {
		pkgLog().Debug("Multiplexer-serving probe failed", "err", err)
		return false, nil
	}
	return served, nil
}

// Prints the refusal and reports true when a named profile must not get a gateway of its own
func namedProfileRefusedUnderMultiplexer(ctx context.Context, w io.Writer, force bool) (bool, error) {
	if force {
		return false, nil
	}
	con := serviceConsole(w)
	suffix, err := currentProfileName(ctx)
	if err != nil {
		return false, nil
	}
	h := home.Dir(ctx)
	name, named := ProfileNameForHome(h)
	isNamed := named && name != "" && name != "default"
	standalone := false
	if isNamed {
		if standalone, err = profiles.ProfileIsStandalone(ctx, h); err != nil {
			return false, nil
		}
	}
	newStandalone := false
	if isNamed {
		installed, err := isServiceInstalled(ctx)
		if err != nil {
			return false, nil
		}
		newStandalone = !installed
	}
	owner := servedByAnotherHostGateway(ctx, nil)
	served := owner != nil
	if !served {
		multiplexed, err := namedProfileServedByRunningMultiplexer(ctx, nil)
		if err != nil {
			return false, err
		}
		served = multiplexed
	}
	if standalone {
		if !served {
			return false, nil
		}
		con.PrintError(StandaloneRescanMessage(suffix))
		return true, nil
	}
	if !served && !newStandalone {
		return false, nil
	}
	if served {
		con.PrintError(fmt.Sprintf("The host gateway already serves profile '%s'.", suffix))
		if owner != nil {
			fmt.Fprintf(w, "  %s\n", owner.Describe())
		}
	} else {
		con.PrintError(fmt.Sprintf("Profile '%s' does not get a gateway of its own.", suffix))
	}
	fmt.Fprint(w, "  Exactly one gateway per host is the inbound process for every\n"+
		"  profile. Starting a separate gateway for this profile would\n"+
		"  double-bind its platforms (two pollers on one bot token, port\n"+
		"  conflicts).\n\n")
	if served {
		label := "default"
		if owner != nil {
			label = owner.ProfileLabel()
		}
		fmt.Fprintln(w, "  Manage the host gateway instead:")
		fmt.Fprintln(w)
		fmt.Fprintf(w, "    icebath -p %s gateway restart\n", label)
	} else {
		fmt.Fprintln(w, "  Install or start the host gateway from the default profile; it serves this one too:")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "    icebath gateway install")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Or fold an existing per-profile fleet onto one host gateway:")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "    icebath gateway migrate --multiplex")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  A separate per-profile gateway (for a fleet split across UNIX users or a")
	fmt.Fprintf(w, "  ICEBATH_HOME outside profiles/) needs --force:  icebath -p %s gateway install --force\n", suffix)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Temporary compatibility path while multiplexing gaps are closed: set")
	fmt.Fprintf(w, "  gateway.standalone: true in %s/config.yaml,\n", home.Display(h))
	fmt.Fprintln(w, "  then wait for the host gateway to rescan (<=30s) or send its rescan-profiles control verb.")
	fmt.Fprintf(w, "  (%s)\n", standaloneDeprecationNotice)
	return true, nil
}

// Prints the already served note and reports true when setup must not install a standalone service
func servedProfileNeedsNoService(ctx context.Context, w io.Writer) (bool, error) {
	multiplexed, err := namedProfileServedByRunningMultiplexer(ctx, nil)
	if err != nil {
		return false, err
	}
	if !multiplexed {
		return namedProfileRefusedUnderMultiplexer(ctx, w, false)
	}
	con := serviceConsole(w)
	name, err := currentProfileName(ctx)
	if err != nil {
		return false, err
	}
	standalone, err := profiles.ProfileIsStandalone(ctx, home.Dir(ctx))
	if err != nil {
		return false, err
	}
	if standalone {
		con.PrintInfo(StandaloneRescanMessage(name))
		return true, nil
	}
	con.PrintSuccess(fmt.Sprintf("Profile '%s' is already served by the default multiplexer.", name))
	con.PrintInfo("  (served now by the running multiplexed gateway — add its bot token and it connects)")
	con.PrintInfo("  No standalone gateway service was installed or started.")
	return true, nil
}

// What stops without a gateway service, the full build's line naming scheduled jobs and the other builds naming what they run
func serviceAbsentNotice(full string) string {
	switch {
	case linktier.Linked(linktier.Extras):
		return full
	case linktier.Linked(linktier.Plugins):
		return "Without a service, messaging stops at logout/reboot."
	}
	return "Without a service, the gateway stops at logout/reboot."
}
