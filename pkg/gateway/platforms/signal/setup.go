package signal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Signals that the user cancelled the wizard at a prompt
var errSetupCancelled = errors.New("setup cancelled")

// Line of plain input, errSetupCancelled after printing the cancel line on Ctrl-C or end of input
func lineInput(prompt string) (string, error) {
	value, err := provider.SetupConsole().LineInput(prompt)
	if errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF) {
		gateway.WizardPrintln("\n  Setup cancelled.")
		return "", errSetupCancelled
	}
	return strings.TrimSpace(value), err
}

// Prints each line as an info line
func infoLines(lines ...string) {
	for _, line := range lines {
		gateway.WizardCLI.PrintInfo(line)
	}
}

// False when Signal is already configured and the user declines to reconfigure
func confirmReconfigure(ctx context.Context) (bool, error) {
	for _, name := range []string{"SIGNAL_HTTP_URL", "SIGNAL_ACCOUNT"} {
		v, err := gateway.WizardConfig.GetEnvValue(ctx, name)
		if err != nil {
			return false, err
		}
		if v == "" {
			return true, nil
		}
	}
	gateway.WizardPrintln("")
	gateway.WizardCLI.PrintSuccess("Signal is already configured.")
	return gateway.WizardCLI.PromptYesNo(ctx, "  Reconfigure Signal?", false), nil
}

// Signal wizard of icebath gateway setup writing the daemon URL, account and allowlists to the home .env
func InteractiveSetup(ctx context.Context) error {
	err := runSetup(ctx)
	if errors.Is(err, errSetupCancelled) {
		return nil
	}
	return err
}

// Wizard steps, errSetupCancelled when the user leaves at a prompt
func runSetup(ctx context.Context) error {
	gateway.WizardCLI.PrintHeader("📡 Signal Setup")
	existingURL, err := gateway.WizardConfig.GetEnvValue(ctx, "SIGNAL_HTTP_URL")
	if err != nil {
		return err
	}
	existingAccount, err := gateway.WizardConfig.GetEnvValue(ctx, "SIGNAL_ACCOUNT")
	if err != nil {
		return err
	}
	if proceed, err := confirmReconfigure(ctx); err != nil || !proceed {
		return err
	}
	gateway.WizardPrintln("")
	if _, err := exec.LookPath("signal-cli"); err == nil {
		gateway.WizardCLI.PrintSuccess("signal-cli found on PATH.")
	} else {
		gateway.WizardCLI.PrintWarning("signal-cli not found on PATH.")
		infoLines("  Signal requires signal-cli running as an HTTP daemon.", "  Install options:",
			"    Linux:  download from https://github.com/AsamK/signal-cli/releases",
			"    macOS:  brew install signal-cli", "    Docker: bbernhard/signal-cli-rest-api")
		gateway.WizardPrintln("")
		infoLines("  After installing, link your account and start the daemon:", `    signal-cli link -n "icebath"`,
			"    signal-cli --account +YOURNUMBER daemon --http 127.0.0.1:8080")
		gateway.WizardPrintln("")
	}
	gateway.WizardPrintln("")
	gateway.WizardCLI.PrintInfo("  Enter the URL where signal-cli HTTP daemon is running.")
	defaultURL := existingURL
	if defaultURL == "" {
		defaultURL = defaultHTTPURL
	}
	rawURL, err := lineInput(fmt.Sprintf("  HTTP URL [%s]: ", defaultURL))
	if err != nil {
		return err
	}
	if rawURL == "" {
		rawURL = defaultURL
	}
	gateway.WizardCLI.PrintInfo("  Testing connection...")
	probe := &rpcClient{baseURL: strings.TrimRight(rawURL, "/"), http: gateway.LimitedHTTPClient(gateway.DefaultHTTPXLimits(), false)}
	probeErr := probe.check(ctx)
	probe.http.CloseIdleConnections()
	var status *checkStatusError
	switch {
	case probeErr == nil:
		gateway.WizardCLI.PrintSuccess("  signal-cli daemon is reachable!")
	case errors.As(probeErr, &status):
		gateway.WizardCLI.PrintWarning(fmt.Sprintf("  signal-cli responded with status %d.", status.status))
		if !gateway.WizardCLI.PromptYesNo(ctx, "  Continue anyway?", false) {
			return nil
		}
	default:
		gateway.WizardCLI.PrintWarning(fmt.Sprintf("  Could not reach signal-cli at %s: %v", rawURL, probeErr))
		if !gateway.WizardCLI.PromptYesNo(ctx, "  Save this URL anyway? (you can start signal-cli later)", true) {
			return nil
		}
	}
	if err := gateway.WizardConfig.SaveEnvValue(ctx, "SIGNAL_HTTP_URL", rawURL); err != nil {
		return err
	}
	gateway.WizardPrintln("")
	infoLines("  Enter your Signal account phone number in E.164 format.", "  Example: +15551234567")
	prompt := "  Account number: "
	if existingAccount != "" {
		prompt = fmt.Sprintf("  Account number [%s]: ", existingAccount)
	}
	account, err := lineInput(prompt)
	if err != nil {
		return err
	}
	if account == "" {
		account = existingAccount
	}
	if account == "" {
		gateway.WizardCLI.PrintError("  Account number is required.")
		return nil
	}
	if err := gateway.WizardConfig.SaveEnvValue(ctx, "SIGNAL_ACCOUNT", account); err != nil {
		return err
	}
	gateway.WizardPrintln("")
	infoLines("  The gateway DENIES all users by default for security.",
		"  Enter phone numbers or UUIDs of allowed users (comma-separated).")
	defaultAllowed, err := gateway.WizardConfig.GetEnvValue(ctx, "SIGNAL_ALLOWED_USERS")
	if err != nil {
		return err
	}
	if defaultAllowed == "" {
		defaultAllowed = account
	}
	allowed, err := lineInput(fmt.Sprintf("  Allowed users [%s]: ", defaultAllowed))
	if err != nil {
		return err
	}
	if allowed == "" {
		allowed = defaultAllowed
	}
	if err := gateway.WizardConfig.SaveEnvValue(ctx, "SIGNAL_ALLOWED_USERS", allowed); err != nil {
		return err
	}
	gateway.WizardPrintln("")
	if gateway.WizardCLI.PromptYesNo(ctx, "  Enable group messaging? (disabled by default for security)", false) {
		gateway.WizardPrintln("")
		gateway.WizardCLI.PrintInfo("  Enter group IDs to allow, or * for all groups.")
		existingGroups, err := gateway.WizardConfig.GetEnvValue(ctx, "SIGNAL_GROUP_ALLOWED_USERS")
		if err != nil {
			return err
		}
		shown := existingGroups
		if shown == "" {
			shown = "*"
		}
		groups, err := lineInput(fmt.Sprintf("  Group IDs [%s]: ", shown))
		if err != nil {
			return err
		}
		if groups == "" {
			groups = shown
		}
		if err := gateway.WizardConfig.SaveEnvValue(ctx, "SIGNAL_GROUP_ALLOWED_USERS", groups); err != nil {
			return err
		}
	}
	groupsEnabled, err := gateway.WizardConfig.GetEnvValue(ctx, "SIGNAL_GROUP_ALLOWED_USERS")
	if err != nil {
		return err
	}
	groupState := "disabled"
	if groupsEnabled != "" {
		groupState = "enabled"
	}
	gateway.WizardPrintln("")
	gateway.WizardCLI.PrintSuccess("Signal configured!")
	infoLines("  URL: "+rawURL, "  Account: "+account, "  DM auth: via SIGNAL_ALLOWED_USERS + DM pairing", "  Groups: "+groupState)
	return nil
}
