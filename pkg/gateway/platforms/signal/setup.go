package signal

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Wizard seams tests replace, the line reader, the PATH probe and the daemon health probe
var (
	wizardLineInput = func(prompt string) (string, error) { return provider.SetupConsole().LineInput(prompt) }
	wizardWhich     = func(cmd string) bool {
		_, ok := pyshutil.Which(cmd, nil)
		return ok
	}
	wizardCheck = func(ctx context.Context, rawURL string) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return 0, err
		}
		client := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), CheckRedirect: pyhttpx.RedirectPolicy(false)}
		defer client.CloseIdleConnections()
		resp, err := pyhttpx.Do(client, req, 10*time.Second)
		if err != nil {
			return 0, &pyErr{"ConnectError", pyhttpx.ErrorText(err)}
		}
		return resp.StatusCode, nil
	}
)

// One stripped wizard line, false after printing the cancel line on end of input or Ctrl-C
func signalLineInput(promptText string) (string, bool) {
	value, err := wizardLineInput(promptText)
	if err != nil {
		gateway.WizardPrintln("\n  Setup cancelled.")
		return "", false
	}
	return pystr.Strip(value), true
}

// Prints each line through the wizard info printer
func printInfoLines(lines ...string) {
	for _, line := range lines {
		gateway.WizardSetup.PrintInfo(line)
	}
}

// Signal flow of icebath gateway setup, the daemon URL, account, allowed users and group policy
func InteractiveSetup(ctx context.Context) error {
	w, env := gateway.WizardSetup, gateway.WizardConfig
	gateway.WizardGateway.PrintSetupHeader("📡 Signal")
	existingURL, err := env.GetEnvValue(ctx, "SIGNAL_HTTP_URL")
	if err != nil {
		return err
	}
	existingAccount, err := env.GetEnvValue(ctx, "SIGNAL_ACCOUNT")
	if err != nil {
		return err
	}
	proceed, err := gateway.WizardGateway.ConfirmReconfigure(ctx, "Signal", "SIGNAL_HTTP_URL", "SIGNAL_ACCOUNT")
	if err != nil || !proceed {
		return err
	}
	gateway.WizardPrintln("")
	if wizardWhich("signal-cli") {
		w.PrintSuccess("signal-cli found on PATH.")
	} else {
		w.PrintWarning("signal-cli not found on PATH.")
		printInfoLines("  Signal requires signal-cli running as an HTTP daemon.", "  Install options:",
			"    Linux:  download from https://github.com/AsamK/signal-cli/releases",
			"    macOS:  brew install signal-cli", "    Docker: bbernhard/signal-cli-rest-api")
		gateway.WizardPrintln("")
		printInfoLines("  After installing, link your account and start the daemon:",
			`    signal-cli link -n "IcebathAgent"`,
			"    signal-cli --account +YOURNUMBER daemon --http 127.0.0.1:8080")
		gateway.WizardPrintln("")
	}
	gateway.WizardPrintln("")
	w.PrintInfo("  Enter the URL where signal-cli HTTP daemon is running.")
	defaultURL := existingURL
	if defaultURL == "" {
		defaultURL = "http://127.0.0.1:8080"
	}
	url, ok := signalLineInput(fmt.Sprintf("  HTTP URL [%s]: ", defaultURL))
	if !ok {
		return nil
	}
	if url == "" {
		url = defaultURL
	}
	w.PrintInfo("  Testing connection...")
	status, err := wizardCheck(ctx, pystr.RstripChars(url, "/")+"/api/v1/check")
	switch {
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		w.PrintWarning(fmt.Sprintf("  Could not reach signal-cli at %s: %s", url, errText(err)))
		if !w.PromptYesNo("  Save this URL anyway? (you can start signal-cli later)", true) {
			return nil
		}
	case status == 200:
		w.PrintSuccess("  signal-cli daemon is reachable!")
	default:
		w.PrintWarning(fmt.Sprintf("  signal-cli responded with status %d.", status))
		if !w.PromptYesNo("  Continue anyway?", false) {
			return nil
		}
	}
	if err := env.SaveEnvValue(ctx, "SIGNAL_HTTP_URL", url); err != nil {
		return err
	}
	gateway.WizardPrintln("")
	printInfoLines("  Enter your Signal account phone number in E.164 format.", "  Example: +15551234567")
	accountPrompt := "  Account number: "
	if existingAccount != "" {
		accountPrompt = fmt.Sprintf("  Account number [%s]: ", existingAccount)
	}
	account, ok := signalLineInput(accountPrompt)
	if !ok {
		return nil
	}
	if account == "" {
		account = existingAccount
	}
	if account == "" {
		w.PrintError("  Account number is required.")
		return nil
	}
	if err := env.SaveEnvValue(ctx, "SIGNAL_ACCOUNT", account); err != nil {
		return err
	}
	gateway.WizardPrintln("")
	printInfoLines("  The gateway DENIES all users by default for security.",
		"  Enter phone numbers or UUIDs of allowed users (comma-separated).")
	defaultAllowed, err := env.GetEnvValue(ctx, "SIGNAL_ALLOWED_USERS")
	if err != nil {
		return err
	}
	if defaultAllowed == "" {
		defaultAllowed = account
	}
	allowed, ok := signalLineInput(fmt.Sprintf("  Allowed users [%s]: ", defaultAllowed))
	if !ok {
		return nil
	}
	if allowed == "" {
		allowed = defaultAllowed
	}
	if err := env.SaveEnvValue(ctx, "SIGNAL_ALLOWED_USERS", allowed); err != nil {
		return err
	}
	gateway.WizardPrintln("")
	if w.PromptYesNo("  Enable group messaging? (disabled by default for security)", false) {
		gateway.WizardPrintln("")
		w.PrintInfo("  Enter group IDs to allow, or * for all groups.")
		existingGroups, err := env.GetEnvValue(ctx, "SIGNAL_GROUP_ALLOWED_USERS")
		if err != nil {
			return err
		}
		shown := existingGroups
		if shown == "" {
			shown = "*"
		}
		groups, ok := signalLineInput(fmt.Sprintf("  Group IDs [%s]: ", shown))
		if !ok {
			return nil
		}
		if groups == "" {
			groups = shown
		}
		if err := env.SaveEnvValue(ctx, "SIGNAL_GROUP_ALLOWED_USERS", groups); err != nil {
			return err
		}
	}
	gateway.WizardPrintln("")
	w.PrintSuccess("Signal configured!")
	groupsSet, err := env.GetEnvValue(ctx, "SIGNAL_GROUP_ALLOWED_USERS")
	if err != nil {
		return err
	}
	groupsState := "disabled"
	if groupsSet != "" {
		groupsState = "enabled"
	}
	printInfoLines("  URL: "+url, "  Account: "+account, "  DM auth: via SIGNAL_ALLOWED_USERS + DM pairing", "  Groups: "+groupsState)
	return nil
}
