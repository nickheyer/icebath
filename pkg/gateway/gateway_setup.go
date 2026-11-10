package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var wizardBanner = []string{
	"┌─────────────────────────────────────────────────────────┐",
	"│             ☤ Gateway Setup                            │",
	"├─────────────────────────────────────────────────────────┤",
	"│  Configure messaging platforms and the gateway service. │",
	"│  Press Ctrl+C at any time to exit.                     │",
	"└─────────────────────────────────────────────────────────┘",
}

var wizardBackendLabels = map[string]string{"systemd": "systemd", "launchd": "launchd", "windows": "Scheduled Task"}

var wizardNoServiceLines = map[string][]string{
	"wsl": {
		"  WSL detected but systemd is not running.", "  Run in foreground: icebath gateway run",
		"  For persistence:   tmux new -s icebath 'icebath gateway run'",
		"  To enable systemd: add systemd=true to /etc/wsl.conf, then 'wsl --shutdown'",
	},
	"termux": {
		"  Termux does not use systemd/launchd services.", "  Run in foreground: icebath gateway run",
		"  Or start it manually in the background (best effort): nohup icebath gateway run >{home}/logs/gateway.log 2>&1 &",
	},
	"unsupported": {
		"  Service install not supported on this platform.", "  Run in foreground: icebath gateway run",
	},
}

// GatewaySetup runs icebath gateway setup, messaging platforms first then the service offer, printing to w
func GatewaySetup(ctx context.Context, w io.Writer) error {
	defer provider.WithSetupOutput(w)()
	h := home.Dir(ctx)
	if config.ManagedSystem(ctx, h) != "" {
		config.ManagedError(ctx, h, "run gateway setup")
		return nil
	}
	WizardPrintln("")
	con := provider.SetupConsole()
	for _, line := range wizardBanner {
		WizardPrintln(con.Color(line, cliui.Magenta))
	}
	if err := wizardServiceStatusBlock(ctx); err != nil {
		return err
	}
	if err := wizardPlatformLoop(ctx); err != nil {
		return err
	}
	progress := false
	for _, p := range AllPlatforms(ctx) {
		status, err := PlatformStatus(ctx, p)
		if err != nil {
			return err
		}
		if wizardStatusIsProgress(status) {
			progress = true
			break
		}
	}
	if progress {
		if err := wizardPostSetup(ctx); err != nil {
			return err
		}
	} else {
		WizardPrintln("")
		gatewayFacade.printInfo("No platforms configured. Run 'icebath gateway setup' when ready.")
	}
	WizardPrintln("")
	return nil
}

// Meaningful platform progress, neither unconfigured nor partial nor a disabled plugin
func wizardStatusIsProgress(status string) bool {
	s := pystr.Lower(status)
	return !(s == "not configured" || strings.HasPrefix(s, "partially") || strings.HasPrefix(s, "plugin disabled"))
}

// Pre platform service status with scope warnings, offering to start an installed but stopped service
func wizardServiceStatusBlock(ctx context.Context) error {
	w := provider.SetupOutput()
	WizardPrintln("")
	installed, err := setupServiceInstalled(ctx)
	if err != nil {
		return err
	}
	running, err := setupServiceRunning(ctx)
	if err != nil {
		return err
	}
	if setupSupportsSystemd(ctx) {
		conflict, err := setupConflictingUnits(ctx)
		if err != nil {
			return err
		}
		if conflict {
			if err := setupConflictWarning(ctx, w); err != nil {
				return err
			}
			WizardPrintln("")
		}
	}
	if setupSupportsSystemd(ctx) && installHooks.hasLegacy(ctx) {
		installHooks.legacyWarning(ctx, w)
		WizardPrintln("")
	}
	switch {
	case installed && running:
		gatewayFacade.printSuccess("Gateway service is installed and running.")
	case installed:
		gatewayFacade.printWarning("Gateway service is installed but not running.")
		return wizardOfferServiceAction(ctx, "start", "  Start it now?", "Failed to start", false)
	default:
		gatewayFacade.printInfo("Gateway service is not installed yet.")
		gatewayFacade.printInfo("You'll be offered to install it after configuring platforms.")
	}
	return nil
}

// Start or restart prompt of the wizard, the system scope remediation instead when it would need root
func wizardOfferServiceAction(ctx context.Context, action, question, failedLabel string, windows bool) error {
	w := provider.SetupOutput()
	if setupSupportsSystemd(ctx) {
		needsRoot, err := setupScopeNeedsRoot(ctx, false)
		if err != nil {
			return err
		}
		if needsRoot {
			return printSystemScopeRemediation(ctx, serviceConsole(w), action)
		}
	}
	if gatewayFacade.promptYesNo(question, true) {
		return setupServiceActionFn(ctx, w, action, failedLabel, windows, false)
	}
	return nil
}

// Messaging platform menu with live status, configuring the chosen platform until Done
func wizardPlatformLoop(ctx context.Context) error {
	for {
		WizardPrintln("")
		WizardSetup.PrintHeader("Messaging Platforms")
		platforms := AllPlatforms(ctx)
		items := make([]string, 0, len(platforms)+1)
		for _, p := range platforms {
			status, err := PlatformStatus(ctx, p)
			if err != nil {
				return err
			}
			items = append(items, fmt.Sprintf("%s %s  (%s)", p.Emoji, p.Label, status))
		}
		items = append(items, "Done")
		choice := gatewayFacade.promptChoice("Select a platform to configure:", items, len(items)-1)
		if choice == len(platforms) {
			return nil
		}
		if err := configurePlatform(ctx, platforms[choice]); err != nil {
			return err
		}
	}
}

// Fresh install from the wizard asking start now and start on login once, the Windows installer owning its start
func wizardSetupInstallService(ctx context.Context, backend string) error {
	w := provider.SetupOutput()
	wslNote := ""
	if hostIsWSL() {
		wslNote = " (note: services may not survive WSL restarts)"
	}
	startNow := gatewayFacade.promptYesNo("  Start the gateway now?", true)
	label, ok := wizardBackendLabels[backend]
	if !ok {
		return &PyError{"KeyError", pyrepr.Repr(backend)}
	}
	startOnLogin := gatewayFacade.promptYesNo(
		fmt.Sprintf("  Start the gateway automatically on login/boot as a %s service?%s", label, wslNote), true)
	if !(startNow || startOnLogin) {
		gatewayFacade.printInfo("  Skipped start and auto-start setup.")
		gatewayFacade.printInfo("  You can install later: icebath gateway install")
		if setupSupportsSystemd(ctx) {
			gatewayFacade.printInfo("  Or as a boot-time service: sudo icebath gateway install --system")
		}
		gatewayFacade.printInfo("  Or run in foreground:  icebath gateway run")
		return nil
	}
	err := func() error {
		scope, didInstall := "", true
		var err error
		switch backend {
		case "systemd":
			scope, didInstall, err = setupInstallLinuxScope(ctx, w, false, startOnLogin)
		case "launchd":
			err = setupLaunchdInstall(ctx, w, false, startNow, false)
		default:
			return setupWindowsInstall(ctx, WindowsInstallOptions{Force: false, StartNow: optBool(startNow), StartOnLogin: optBool(startOnLogin)})
		}
		if err != nil {
			return err
		}
		WizardPrintln("")
		if didInstall && startNow {
			return setupServiceActionFn(ctx, w, "start", "Start failed", true, scope == "system")
		}
		return nil
	}()
	var called *CalledProcessError
	var exit *SystemExitError
	switch {
	case errors.As(err, &called):
		gatewayFacade.printError("  Install failed: " + called.Error())
		gatewayFacade.printInfo("  You can try manually: icebath gateway install")
		return nil
	case errors.As(err, &exit):
		gatewayFacade.printWarning("  Gateway service install did not complete.")
		gatewayFacade.printInfo("  You can try manually: icebath gateway install")
		return nil
	}
	return err
}

// Service offer after platform progress, install, start or restart, or foreground guidance without a backend
func wizardPostSetup(ctx context.Context) error {
	w := provider.SetupOutput()
	WizardPrintln("")
	WizardPrintln(provider.SetupConsole().Color(strings.Repeat("─", 58), cliui.Dim))
	served, err := setupNeedsNoService(ctx, w)
	if err != nil || served {
		return err
	}
	installed, err := setupServiceInstalled(ctx)
	if err != nil {
		return err
	}
	running, err := setupServiceRunning(ctx)
	if err != nil {
		return err
	}
	switch {
	case running:
		return wizardOfferServiceAction(ctx, "restart", "  Restart the gateway to pick up changes?", "Restart failed", true)
	case installed:
		return wizardOfferServiceAction(ctx, "start", "  Start the gateway service?", "Start failed", true)
	}
	WizardPrintln("")
	if backend := serviceBackendName(ctx, true); backend != "" {
		return wizardSetupInstallService(ctx, backend)
	}
	reason, shown := "unsupported", ""
	switch {
	case hostIsWSL():
		reason = "wsl"
	case hostIsTermux():
		reason, shown = "termux", home.Display(home.Dir(ctx))
	}
	lines := make([]string, 0, len(wizardNoServiceLines[reason]))
	for _, line := range wizardNoServiceLines[reason] {
		lines = append(lines, strings.ReplaceAll(line, "{home}", shown))
	}
	printInfoLines(lines...)
	return nil
}
