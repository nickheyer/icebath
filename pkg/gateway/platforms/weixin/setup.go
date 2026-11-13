package weixin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// One env assignment the wizard persists
type envValue struct{ name, value string }

// Persists each env assignment in order
func saveEnvValues(ctx context.Context, values ...envValue) error {
	for _, v := range values {
		if err := gateway.WizardSetup.SaveEnvValue(ctx, v.name, v.value); err != nil {
			return err
		}
	}
	return nil
}

// Writes the lines through the setup info printer
func printInfoLines(lines ...string) {
	for _, line := range lines {
		gateway.WizardSetup.PrintInfo(line)
	}
}

// Prints the cyan platform setup rule after a blank line
func printSetupHeader(title string) {
	gateway.WizardPrintln("")
	gateway.WizardPrintln((&cliui.Console{Out: provider.SetupOutput()}).Color(fmt.Sprintf("  ─── %s Setup ───", title), cliui.Cyan))
}

// False when every env var is already set and the user declines to reconfigure
func confirmReconfigure(ctx context.Context, label string, envVars ...string) (bool, error) {
	for _, name := range envVars {
		v, err := gateway.WizardSetup.GetEnvValue(ctx, name)
		if err != nil {
			return false, err
		}
		if v == "" {
			return true, nil
		}
	}
	gateway.WizardPrintln("")
	gateway.WizardSetup.PrintSuccess(label + " is already configured.")
	return gateway.WizardSetup.PromptYesNo(fmt.Sprintf("  Reconfigure %s?", label), false), nil
}

// Comma separated id prompt with spaces removed
func promptCSV(question, def string) string {
	return strings.ReplaceAll(gateway.WizardSetup.Prompt(question, def, false), " ", "")
}

// DM policy, allow all flag, message level and message of each non allowlist menu choice
var dmPolicyChoices = map[int]struct {
	policy, allowAll string
	success          bool
	message          string
}{
	0: {"pairing", "false", true, "  DM pairing enabled."},
	1: {"open", "true", false, "  Open DM access enabled for Weixin."},
	3: {"disabled", "false", false, "  Direct messages disabled."},
}

var groupNote = []string{
	"  QR login connects an iLink bot identity (e.g. ...@im.bot), not a",
	"  scriptable personal WeChat account. Ordinary WeChat groups typically cannot",
	"  invite an @im.bot identity, and iLink does not deliver ordinary-group events",
	"  to most bot accounts. The settings below only apply when iLink actually",
	"  delivers group events for your account type — otherwise DM remains the only",
	"  working channel regardless of this choice.",
}

// Weixin wizard of icebath gateway setup, QR login then DM, group and home channel policy
func interactiveSetup(ctx context.Context) error {
	printSetupHeader("💬 Weixin / WeChat")
	gateway.WizardPrintln("")
	printInfoLines(
		"  1. Icebath will open Tencent iLink QR login in this terminal.",
		"  2. Use WeChat to scan and confirm the QR code.",
		fmt.Sprintf("  3. Icebath will store the returned account_id/token in %s/.env.", home.Display(home.Dir(ctx))),
		"  4. This adapter supports native text, image, video, and document delivery.",
	)
	proceed, err := confirmReconfigure(ctx, "Weixin", "WEIXIN_ACCOUNT_ID", "WEIXIN_TOKEN")
	if err != nil || !proceed {
		return err
	}
	gateway.WizardPrintln("")
	if !gateway.WizardSetup.PromptYesNo("  Start QR login now?", true) {
		gateway.WizardSetup.PrintInfo("  Cancelled.")
		return nil
	}
	creds, err := qrLogin(ctx, home.Dir(ctx))
	switch {
	case errors.Is(err, gateway.ErrKeyboardInterrupt):
		gateway.WizardPrintln("")
		gateway.WizardSetup.PrintWarning("  Weixin setup cancelled.")
		return nil
	case err != nil:
		gateway.WizardSetup.PrintError(fmt.Sprintf("  QR login failed: %v", err))
		return nil
	case creds == nil:
		gateway.WizardSetup.PrintWarning("  QR login did not complete.")
		return nil
	}
	values := []envValue{{"WEIXIN_ACCOUNT_ID", creds.AccountID}, {"WEIXIN_TOKEN", creds.Token}}
	if creds.BaseURL != "" {
		values = append(values, envValue{"WEIXIN_BASE_URL", creds.BaseURL})
	}
	cdn, err := gateway.WizardSetup.GetEnvValue(ctx, "WEIXIN_CDN_BASE_URL")
	if err != nil {
		return err
	}
	if cdn == "" {
		cdn = defaultCDNBaseURL
	}
	if err := saveEnvValues(ctx, append(values, envValue{"WEIXIN_CDN_BASE_URL", cdn})...); err != nil {
		return err
	}
	if err := setupDMAccess(ctx, creds.UserID); err != nil {
		return err
	}
	if err := setupGroupAccess(ctx); err != nil {
		return err
	}
	if creds.UserID != "" {
		gateway.WizardPrintln("")
		if gateway.WizardSetup.PromptYesNo(fmt.Sprintf("  Use your Weixin user ID (%s) as the home channel?", creds.UserID), true) {
			if err := gateway.WizardSetup.SaveEnvValue(ctx, "WEIXIN_HOME_CHANNEL", creds.UserID); err != nil {
				return err
			}
			gateway.WizardSetup.PrintSuccess("  Home channel set to " + creds.UserID)
		}
	}
	gateway.WizardPrintln("")
	gateway.WizardSetup.PrintSuccess("Weixin configured!")
	gateway.WizardSetup.PrintInfo("  Account ID: " + creds.AccountID)
	if creds.UserID != "" {
		gateway.WizardSetup.PrintInfo("  User ID: " + creds.UserID)
	}
	return nil
}

// Asks how direct messages are authorized and persists the policy and allowlist
func setupDMAccess(ctx context.Context, userID string) error {
	gateway.WizardPrintln("")
	idx := gateway.WizardSetup.PromptChoice("  How should direct messages be authorized?", []string{
		"Use DM pairing approval (recommended)", "Allow all direct messages", "Only allow listed user IDs", "Disable direct messages",
	}, 0)
	if idx == 2 {
		allowlist := promptCSV("  Allowed Weixin user IDs (comma-separated)", userID)
		if err := saveEnvValues(ctx, envValue{"WEIXIN_DM_POLICY", "allowlist"}, envValue{"WEIXIN_ALLOW_ALL_USERS", "false"},
			envValue{"WEIXIN_ALLOWED_USERS", allowlist}); err != nil {
			return err
		}
		gateway.WizardSetup.PrintSuccess("  Weixin allowlist saved.")
		return nil
	}
	choice, ok := dmPolicyChoices[idx]
	if !ok {
		choice = dmPolicyChoices[3]
	}
	if err := saveEnvValues(ctx, envValue{"WEIXIN_DM_POLICY", choice.policy}, envValue{"WEIXIN_ALLOW_ALL_USERS", choice.allowAll},
		envValue{"WEIXIN_ALLOWED_USERS", ""}); err != nil {
		return err
	}
	if choice.success {
		gateway.WizardSetup.PrintSuccess(choice.message)
	} else {
		gateway.WizardSetup.PrintWarning(choice.message)
	}
	if idx == 0 {
		gateway.WizardSetup.PrintInfo("  Unknown DM users can request access and you approve them with `icebath pairing approve`.")
	}
	return nil
}

// Asks how group chats are handled and persists the group policy and allowlist
func setupGroupAccess(ctx context.Context) error {
	gateway.WizardPrintln("")
	printInfoLines(groupNote...)
	idx := gateway.WizardSetup.PromptChoice("  How should group chats be handled?", []string{
		"Disable group chats (recommended)", "Allow all group chats", "Only allow listed group chat IDs",
	}, 0)
	switch idx {
	case 0:
		if err := saveEnvValues(ctx, envValue{"WEIXIN_GROUP_POLICY", "disabled"}, envValue{"WEIXIN_GROUP_ALLOWED_USERS", ""}); err != nil {
			return err
		}
		gateway.WizardSetup.PrintInfo("  Group chats disabled.")
	case 1:
		if err := saveEnvValues(ctx, envValue{"WEIXIN_GROUP_POLICY", "open"}, envValue{"WEIXIN_GROUP_ALLOWED_USERS", ""}); err != nil {
			return err
		}
		gateway.WizardSetup.PrintWarning("  All group chats enabled (only takes effect if iLink delivers group events).")
	default:
		groups := promptCSV("  Allowed group chat IDs (comma-separated, not member user IDs)", "")
		if err := saveEnvValues(ctx, envValue{"WEIXIN_GROUP_POLICY", "allowlist"}, envValue{"WEIXIN_GROUP_ALLOWED_USERS", groups}); err != nil {
			return err
		}
		gateway.WizardSetup.PrintSuccess("  Group allowlist saved (only takes effect if iLink delivers group events).")
	}
	return nil
}
