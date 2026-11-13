package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// One setup form variable of a built in platform
type SetupVar struct {
	Name, Prompt, Help    string
	Password, IsAllowlist bool
}

// One messaging platform row of the setup menu, a built in form or a registry plugin entry
type SetupPlatform struct {
	Key, Label, Emoji, TokenVar, InstallHint string
	SetupInstructions                        []string
	Vars                                     []SetupVar
	Entry                                    *PlatformEntry
}

// The hermes_cli.gateway names the setup wizard reads at call time, a table tests replace like the patched facade
type gatewayFacadeFns struct {
	printInfo, printSuccess, printWarning, printError func(text string)
	prompt                                            func(question, def string, password bool) string
	promptYesNo                                       func(question string, def bool) bool
	promptChoice                                      func(question string, choices []string, def int) int
	getEnvValue                                       func(ctx context.Context, key string) (string, error)
	saveEnvValue                                      func(ctx context.Context, key, value string) error
	writePlatformConfigField                          func(ctx context.Context, platformKey, fieldKey string, value any, raw bool) error
	windowsHost                                       func() bool
}

var gatewayFacade = gatewayFacadeFns{
	printInfo:    provider.SetupPrintInfo,
	printSuccess: provider.SetupPrintSuccess,
	printWarning: provider.SetupPrintWarning,
	printError:   provider.SetupPrintError,
	prompt:       provider.SetupPrompt,
	promptYesNo:  func(question string, def bool) bool { return provider.PromptYesNo(question, def, "") },
	promptChoice: func(question string, choices []string, def int) int {
		return provider.PromptChoice(question, choices, def, "")
	},
	getEnvValue: func(ctx context.Context, key string) (string, error) {
		v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
		return v, err
	},
	saveEnvValue: func(ctx context.Context, key, value string) error {
		return config.SaveEnvValueContext(ctx, home.Dir(ctx), key, value)
	},
	writePlatformConfigField: func(ctx context.Context, platformKey, fieldKey string, value any, raw bool) error {
		return config.WritePlatformConfigField(ctx, home.Dir(ctx), platformKey, fieldKey, value, raw)
	},
	windowsHost: func() bool { return serviceGOOS == "windows" },
}

// Prints each line through the facade print_info
func printInfoLines(lines ...string) {
	for _, line := range lines {
		gatewayFacade.printInfo(line)
	}
}

// Built in per platform setup forms, every other messaging platform comes from the plugin registry
var setupPlatformsTable = []SetupPlatform{{
	Key: "mattermost", Label: "Mattermost", Emoji: "💬", TokenVar: "MATTERMOST_TOKEN",
	SetupInstructions: []string{
		"1. In Mattermost: Integrations → Bot Accounts → Add Bot Account",
		"   (System Console → Integrations → Bot Accounts must be enabled)",
		"2. Give it a username (e.g. icebath) and copy the bot token",
		"3. Works with any self-hosted Mattermost instance — enter your server URL",
		"4. Find your user ID (the profile dialog does not show it):",
		"   Admins: System Console → User Management → Users → open your user.",
		"   Anyone: while logged in, open <server>/api/v4/users/me in a browser tab.",
		"   With the bot token: GET <server>/api/v4/users/username/<your-username>.",
		"   Copy the \"id\" field. /users/me with the bot token returns the bot's ID.",
		"   ⚠ This is NOT your username — it's a 26-character alphanumeric ID.",
		"5. To get a channel ID: click the channel name → View Info → copy the ID",
	},
	Vars: []SetupVar{
		{Name: "MATTERMOST_URL", Prompt: "Server URL (e.g. https://mm.example.com)",
			Help: "Your Mattermost server URL. Works with any self-hosted instance."},
		{Name: "MATTERMOST_TOKEN", Prompt: "Bot token", Password: true, Help: "Paste the bot token from step 2 above."},
		{Name: "MATTERMOST_ALLOWED_USERS", Prompt: "Allowed user IDs (comma-separated)", IsAllowlist: true,
			Help: "Your user ID from step 4 above (26 characters, not your @username)."},
		{Name: "MATTERMOST_HOME_CHANNEL",
			Prompt: "Home channel ID (for cron/notification delivery, or empty to set later with /set-home)",
			Help:   "Channel ID where Icebath delivers cron results and notifications."},
		{Name: "MATTERMOST_REPLY_MODE",
			Prompt: "Reply mode — 'off' for flat messages, 'thread' for threaded replies (default: off)",
			Help:   "off = flat channel messages, thread = replies nest under your message."},
	},
}}

// Built in forms plus registry plugin platforms, discovering plugins first and hiding Matrix on Windows
func AllPlatforms(ctx context.Context) []SetupPlatform {
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Debug(fmt.Sprintf("plugin discovery failed during platform enumeration: %v", r))
			}
		}()
		plugins.DiscoverPlugins(ctx, false)
	}()
	hideMatrix := gatewayFacade.windowsHost()
	platforms := []SetupPlatform{}
	byKey := map[string]bool{}
	for _, p := range setupPlatformsTable {
		if hideMatrix && p.Key == "matrix" {
			continue
		}
		platforms = append(platforms, p)
		byKey[p.Key] = true
	}
	for _, entry := range Registry.AllEntries(ctx) {
		if byKey[entry.Name] || (hideMatrix && entry.Name == "matrix") {
			continue
		}
		tokenVar := ""
		if len(entry.RequiredEnv) > 0 {
			tokenVar = entry.RequiredEnv[0]
		}
		platforms = append(platforms, SetupPlatform{Key: entry.Name, Label: entry.Label, Emoji: entry.Emoji, TokenVar: tokenVar,
			InstallHint: entry.InstallHint, Entry: entry})
	}
	return platforms
}

// Plain status text of a platform row, uncolored for menu width math
func PlatformStatus(ctx context.Context, p SetupPlatform) (string, error) {
	if entry := p.Entry; entry != nil {
		configured := func() (ok bool) {
			defer func() {
				if recover() != nil {
					ok = false
				}
			}()
			if entry.IsConnected != nil {
				cfg := NewPlatformConfig()
				cfg.Enabled = true
				connected, err := entry.IsConnected(ctx, cfg)
				return err == nil && connected
			}
			return entry.CheckFn != nil && entry.RunCheck(ctx)
		}()
		if configured {
			return "configured", nil
		}
		return "not configured", nil
	}
	if p.TokenVar == "" {
		return "not configured", nil
	}
	v, err := gatewayFacade.getEnvValue(ctx, p.TokenVar)
	if err != nil {
		return "", err
	}
	if v != "" {
		return "configured", nil
	}
	return "not configured", nil
}

// Persists a platform specific unauthorized DM policy in config.yaml
func setPlatformUnauthorizedDMBehavior(ctx context.Context, platformKey, behavior string) error {
	return gatewayFacade.writePlatformConfigField(ctx, platformKey, "unauthorized_dm_behavior", behavior, true)
}

// Prints the cyan platform setup rule after a blank line
func printSetupHeader(title string) {
	setupPrintln("")
	setupPrintln(setupColor(fmt.Sprintf("  ─── %s Setup ───", title), cliui.Cyan))
}

// False when every env var is already set and the user declines to reconfigure
func confirmReconfigure(ctx context.Context, label string, envVars ...string) (bool, error) {
	for _, name := range envVars {
		v, err := gatewayFacade.getEnvValue(ctx, name)
		if err != nil {
			return false, err
		}
		if v == "" {
			return true, nil
		}
	}
	setupPrintln("")
	gatewayFacade.printSuccess(label + " is already configured.")
	return gatewayFacade.promptYesNo(fmt.Sprintf("  Reconfigure %s?", label), false), nil
}

// Offers to persist the user id as the home channel variable
func offerHomeChannel(ctx context.Context, homeVar, userID, what string) error {
	if !gatewayFacade.promptYesNo(fmt.Sprintf("  Use %s (%s) as the home channel?", what, userID), true) {
		return nil
	}
	if err := gatewayFacade.saveEnvValue(ctx, homeVar, userID); err != nil {
		return err
	}
	gatewayFacade.printSuccess("  Home channel set to " + userID)
	return nil
}

// Default index and choices of the no allowlist access prompt, keyed by whether the platform is email
var unauthorizedAccessChoices = map[bool]struct {
	def     int
	choices []string
}{
	true: {3, []string{"Enable open access (any email sender can message the bot)",
		"Use DM pairing (unknown email senders receive a pairing code)",
		"Politely decline unknown senders (one-time message, then silence)",
		"Keep unknown senders silent"}},
	false: {1, []string{"Enable open access (anyone can message the bot)",
		"Use DM pairing (unknown users request access, you approve with 'icebath pairing approve')",
		"Politely decline unknown senders (one-time message, then silence)",
		"Skip for now (bot will deny all users until configured)"}},
}

// Asks how senders outside an empty allowlist are handled and persists the answer
func promptUnauthorizedAccess(ctx context.Context, platformKey string) error {
	isEmail := platformKey == "email"
	setupPrintln("")
	row := unauthorizedAccessChoices[isEmail]
	switch gatewayFacade.promptChoice("  How should unauthorized users be handled?", row.choices, row.def) {
	case 0:
		name := "GATEWAY_ALLOW_ALL_USERS"
		if isEmail {
			name = "EMAIL_ALLOW_ALL_USERS"
		}
		if err := gatewayFacade.saveEnvValue(ctx, name, "true"); err != nil {
			return err
		}
		gatewayFacade.printWarning("  Open access enabled — anyone can use your bot!")
	case 1:
		if isEmail {
			if err := setPlatformUnauthorizedDMBehavior(ctx, "email", "pair"); err != nil {
				return err
			}
		}
		gatewayFacade.printSuccess("  DM pairing mode — users will receive a code to request access.")
		gatewayFacade.printInfo("  Approve with: icebath pairing approve <platform> <code>")
	case 2:
		if err := setPlatformUnauthorizedDMBehavior(ctx, platformKey, "decline"); err != nil {
			return err
		}
		gatewayFacade.printSuccess("  Unknown senders get one polite decline, then silence (unauthorized_dm_behavior: decline).")
	default:
		if isEmail {
			gatewayFacade.printSuccess("  Unknown email senders will be ignored.")
		} else {
			gatewayFacade.printInfo("  Skipped — configure later with 'icebath gateway setup'")
		}
	}
	return nil
}

// Offers the managed bot QR flow, reporting whether the token was saved and the detected owner id
func telegramAutoSetup(ctx context.Context, tokenVar string) (bool, string, error) {
	setupPrintln("")
	printInfoLines("  Telegram can be configured automatically with a managed bot:",
		"  [1] Automatic (scan QR → confirm in Telegram → done)", "  [2] Manual BotFather token")
	if pystr.Strip(gatewayFacade.prompt("  Choice [1/2]", "1", false)) != "1" {
		return false, "", nil
	}
	result, err := telegramAutoSetupFn(ctx, nil)
	if err != nil {
		return false, "", err
	}
	if result != nil && isValidTelegramBotToken(result.Token) {
		if err := gatewayFacade.saveEnvValue(ctx, tokenVar, result.Token); err != nil {
			return false, "", err
		}
		gatewayFacade.printSuccess("  Saved TELEGRAM_BOT_TOKEN")
		return true, result.OwnerUserID, nil
	}
	if result != nil {
		gatewayFacade.printWarning("  Automatic setup returned an invalid Telegram token.")
	}
	setupPrintln("")
	gatewayFacade.printInfo("  Falling back to manual setup...")
	return false, "", nil
}

// Strips the user prefix and the mention brackets Discord copies carry from a comma separated id list
func cleanDiscordIDs(cleaned string) string {
	var parts []string
	for _, uid := range strings.Split(cleaned, ",") {
		uid = pystr.Strip(uid)
		if strings.HasPrefix(uid, "<@") && strings.HasSuffix(uid, ">") {
			uid = pystr.RstripChars(pystr.LstripChars(uid, "<@!"), ">")
		}
		if strings.HasPrefix(pystr.Lower(uid), "user:") {
			uid = string([]rune(uid)[5:])
		}
		if uid != "" {
			parts = append(parts, uid)
		}
	}
	return strings.Join(parts, ",")
}

// Allowlist prompt of one form variable, the saved value or false when the open access prompt ran instead
func promptAllowlistVar(ctx context.Context, v SetupVar, platformKey, autoOwnerUserID string) (string, bool, error) {
	if strings.Contains(v.Name, "TELEGRAM") && autoOwnerUserID != "" {
		gatewayFacade.printSuccess("  Detected your Telegram user ID: " + autoOwnerUserID)
		if gatewayFacade.promptYesNo("  Allow this Telegram account to use the bot?", true) {
			extra := gatewayFacade.prompt("  Additional allowed user IDs (comma-separated, optional)", "", false)
			ids := []string{autoOwnerUserID}
			for _, uid := range strings.Split(strings.ReplaceAll(extra, " ", ""), ",") {
				if uid != "" && !containsString(ids, uid) {
					ids = append(ids, uid)
				}
			}
			cleaned := strings.Join(ids, ",")
			if err := gatewayFacade.saveEnvValue(ctx, v.Name, cleaned); err != nil {
				return "", false, err
			}
			gatewayFacade.printSuccess("  Saved — only these users can interact with the bot.")
			return cleaned, true, nil
		}
	}
	printInfoLines("  The gateway DENIES all users by default for security.",
		"  Enter user IDs to create an allowlist, or leave empty", "  and you'll be asked about open access next.")
	value := gatewayFacade.prompt("  "+v.Prompt, "", false)
	if value == "" {
		return "", false, promptUnauthorizedAccess(ctx, platformKey)
	}
	cleaned := strings.ReplaceAll(value, " ", "")
	if strings.Contains(v.Name, "DISCORD") {
		cleaned = cleanDiscordIDs(cleaned)
	}
	if err := gatewayFacade.saveEnvValue(ctx, v.Name, cleaned); err != nil {
		return "", false, err
	}
	gatewayFacade.printSuccess("  Saved — only these users can interact with the bot.")
	return cleaned, true, nil
}

// Interactive form of a built in platform, its instructions, token, allowlist and the Telegram home channel offer
func setupStandardPlatform(ctx context.Context, p SetupPlatform) error {
	printSetupHeader(p.Emoji + " " + p.Label)
	if len(p.SetupInstructions) > 0 {
		setupPrintln("")
		lines := make([]string, 0, len(p.SetupInstructions))
		for _, line := range p.SetupInstructions {
			lines = append(lines, "  "+line)
		}
		printInfoLines(lines...)
	}
	proceed, err := confirmReconfigure(ctx, p.Label, p.TokenVar)
	if err != nil || !proceed {
		return err
	}
	autoTokenSaved, autoOwnerUserID := false, ""
	if p.Key == "telegram" {
		if autoTokenSaved, autoOwnerUserID, err = telegramAutoSetup(ctx, p.TokenVar); err != nil {
			return err
		}
	}
	allowedValSet := ""
	for _, v := range p.Vars {
		if !(v.Name == p.TokenVar || v.IsAllowlist || !provider.IsSetupHiddenEnv(v.Name)) {
			continue
		}
		setupPrintln("")
		gatewayFacade.printInfo("  " + v.Help)
		existing, err := gatewayFacade.getEnvValue(ctx, v.Name)
		if err != nil {
			return err
		}
		if existing != "" && v.Name != p.TokenVar {
			gatewayFacade.printInfo("  Current: " + existing)
		}
		if autoTokenSaved && v.Name == p.TokenVar {
			gatewayFacade.printInfo("  Token saved by automatic setup.")
			continue
		}
		if v.IsAllowlist {
			saved, ok, err := promptAllowlistVar(ctx, v, p.Key, autoOwnerUserID)
			if err != nil {
				return err
			}
			if ok {
				allowedValSet = saved
			}
			continue
		}
		value := gatewayFacade.prompt("  "+v.Prompt, "", v.Password)
		switch {
		case value != "":
			if err := gatewayFacade.saveEnvValue(ctx, v.Name, value); err != nil {
				return err
			}
			gatewayFacade.printSuccess("  Saved " + v.Name)
		case v.Name == p.TokenVar:
			gatewayFacade.printWarning(fmt.Sprintf("  Skipped — %s won't work without this.", p.Label))
			return nil
		default:
			gatewayFacade.printInfo("  Skipped (can configure later)")
		}
	}
	homeVar := pystr.Upper(p.Label) + "_HOME_CHANNEL"
	homeVal, err := gatewayFacade.getEnvValue(ctx, homeVar)
	if err != nil {
		return err
	}
	if allowedValSet != "" && homeVal == "" && p.Label == "Telegram" {
		first, _, _ := strings.Cut(allowedValSet, ",")
		if first = pystr.Strip(first); first != "" {
			if err := offerHomeChannel(ctx, homeVar, first, "your user ID"); err != nil {
				return err
			}
		}
	}
	setupPrintln("")
	gatewayFacade.printSuccess(fmt.Sprintf("%s %s configured!", p.Emoji, p.Label))
	return nil
}

// Setup function of a built in platform by key, nil when the platform has none
func builtinSetupFn(key string) func(ctx context.Context) error {
	if key == "webhooks" {
		return setupWebhooks
	}
	return nil
}

// Runs the plugin setup function, the built in one, the standard form, or prints the env var hint
func configurePlatform(ctx context.Context, p SetupPlatform) error {
	var fn func(ctx context.Context) error
	if p.Entry != nil {
		fn = p.Entry.SetupFn
	}
	if fn == nil {
		fn = builtinSetupFn(p.Key)
	}
	if fn != nil {
		return fn(ctx)
	}
	if len(p.Vars) > 0 {
		return setupStandardPlatform(ctx, p)
	}
	printSetupHeader(p.Emoji + " " + p.Label)
	var required []string
	if p.Entry != nil {
		required = p.Entry.RequiredEnv
	}
	if len(required) > 0 {
		gatewayFacade.printInfo("  Set these env vars in ~/.icebath/.env: " + strings.Join(required, ", "))
	} else {
		gatewayFacade.printInfo(fmt.Sprintf("  Configure %s in config.yaml under gateway.platforms.%s", p.Label, p.Key))
	}
	if p.InstallHint != "" {
		gatewayFacade.printInfo("  " + p.InstallHint)
	}
	return nil
}
