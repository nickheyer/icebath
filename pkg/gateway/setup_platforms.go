package gateway

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

// The functions platform wizards resolve at call time, a table tests replace like the patched module
type SetupFns struct {
	Prompt          func(question, def string, password bool) string
	PromptYesNo     func(question string, def bool) bool
	PromptChoice    func(question string, choices []string, def int) int
	PromptChecklist func(title string, items []string, preSelected []int) []int
	PrintHeader     func(title string)
	Info            func(lines ...any)
	PrintInfo       func(text string)
	PrintSuccess    func(text string)
	PrintWarning    func(text string)
	PrintError      func(text string)
	GetEnvValue     func(ctx context.Context, key string) (string, error)
	SaveEnvValue    func(ctx context.Context, key, value string) error
	IcebathHome     func(ctx context.Context) string
}

// The module as the platform wizards see it
var WizardSetup = SetupFns{
	Prompt:      provider.SetupPrompt,
	PromptYesNo: func(question string, def bool) bool { return provider.PromptYesNo(question, def, "") },
	PromptChoice: func(question string, choices []string, def int) int {
		return provider.PromptChoice(question, choices, def, "")
	},
	PromptChecklist: provider.PromptChecklist,
	PrintHeader:     func(title string) { provider.PrintSetupHeader(title, false) },
	Info:            provider.SetupInfo,
	PrintInfo:       provider.SetupPrintInfo,
	PrintSuccess:    provider.SetupPrintSuccess,
	PrintWarning:    provider.SetupPrintWarning,
	PrintError:      provider.SetupPrintError,
	GetEnvValue:     setupValue,
	SaveEnvValue:    saveSetupValue,
	IcebathHome:     home.Dir,
}

// Value a setup flow reads under an env name: the setting the name overrides, else the profile .env
func setupValue(ctx context.Context, key string) (string, error) {
	if e := envSetting(key); e != nil {
		return envText(ctx, e), nil
	}
	v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
	return v, err
}

// Saves a value a setup flow asked for under an env name: the setting the name overrides, else the profile .env
func saveSetupValue(ctx context.Context, key, value string) error {
	if e := envSetting(key); e != nil {
		var stored any = value
		if on, isBool := e.(*settings.Bool); isBool && on.EnvInvert {
			coerced, err := on.Coerce(value)
			if err != nil {
				return err
			}
			if said, ok := coerced.(bool); ok {
				stored = !said
			}
		}
		_, err := settings.Set(SettingsContext(ctx), e.Info().Key, stored, settings.Global)
		return err
	}
	return config.SaveEnvValueContext(ctx, home.Dir(ctx), key, value)
}

// Removes a value a setup flow names by env name: the setting the name overrides goes back to its default, else the .env line goes
func removeSetupValue(ctx context.Context, key string) (bool, error) {
	if e := envSetting(key); e != nil {
		change, err := settings.Reset(SettingsContext(ctx), e.Info().Key, settings.Global)
		return err == nil && change.Old.Source != settings.SourceDefault, err
	}
	return config.RemoveHomeEnvValue(ctx, home.Dir(ctx), key)
}

// Keyword options of save_prompted, the success message may name {value}
type SavePromptedOptions struct {
	Password            bool
	SuccessMsg, SkipMsg string
	Transform           func(string) string
}

// The setup_platforms helpers platform wizards share, resolved at call time so tests can observe them
type PlatformSetupFns struct {
	DeclinesReconfigure func(ctx context.Context, label, question string, envVars ...string) (bool, error)
	SavePrompted        func(ctx context.Context, envVar, question string, o SavePromptedOptions) (string, error)
	PromptAllowlist     func(ctx context.Context, envVar, question, successMsg, openMsg string) (string, error)
	SavePort            func(ctx context.Context, envVar, value, def string) error
}

// The module as the platform wizards see it
var PlatformSetup = PlatformSetupFns{
	DeclinesReconfigure: declinesReconfigure,
	SavePrompted:        savePrompted,
	PromptAllowlist:     promptAllowlist,
	SavePort:            savePort,
}

// Prints a line on the setup console like a bare print
func setupPrintln(text string) { fmt.Fprintln(provider.SetupOutput(), text) }

// Text in the ANSI codes when the setup console takes color
func setupColor(text string, codes ...string) string {
	return (&cliui.Console{Out: provider.SetupOutput()}).Color(text, codes...)
}

var (
	telegramBotTokenRE = regexp.MustCompile(`^\p{Nd}+:[A-Za-z0-9_-]{30,}\n?$`)
	// Whether a system scope service action of the wizard needs root, the seam tests replace
	setupScopeNeedsRoot = systemScopeWizardWouldNeedRoot
)

// Whether a token has the Telegram bot token shape
func isValidTelegramBotToken(token string) bool { return telegramBotTokenRE.MatchString(token) }

// True when an env var is already set and the user declines to reconfigure, the gate every wizard shares
func declinesReconfigure(ctx context.Context, label, question string, envVars ...string) (bool, error) {
	set := false
	for _, name := range envVars {
		v, err := WizardSetup.GetEnvValue(ctx, name)
		if err != nil {
			return false, err
		}
		if v != "" {
			set = true
			break
		}
	}
	if !set {
		return false, nil
	}
	WizardSetup.PrintInfo(label + ": already configured")
	return !WizardSetup.PromptYesNo(question, false), nil
}

// Message with each value field filled in, an error for any other field or an unpaired brace
func formatValue(msg, value string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		switch {
		case c == '{' && i+1 < len(msg) && msg[i+1] == '{':
			b.WriteByte('{')
			i++
		case c == '}' && i+1 < len(msg) && msg[i+1] == '}':
			b.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(msg[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("message template %s has a { without a closing }", msg)
			}
			field := msg[i+1 : i+end]
			if field != "value" {
				return "", fmt.Errorf("message template %s names field %s, only {value} is filled in", msg, field)
			}
			b.WriteString(value)
			i += end
		case c == '}':
			return "", fmt.Errorf("message template %s has a } without an opening {", msg)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// Prompts, persists the optionally transformed answer when non empty and reports either way, returning the raw answer
func savePrompted(ctx context.Context, envVar, question string, o SavePromptedOptions) (string, error) {
	value := WizardSetup.Prompt(question, "", o.Password)
	switch {
	case value != "":
		saved := value
		if o.Transform != nil {
			saved = o.Transform(value)
		}
		if err := WizardSetup.SaveEnvValue(ctx, envVar, saved); err != nil {
			return "", err
		}
		if o.SuccessMsg != "" {
			msg, err := formatValue(o.SuccessMsg, value)
			if err != nil {
				return "", err
			}
			WizardSetup.PrintSuccess(msg)
		}
	case o.SkipMsg != "":
		WizardSetup.PrintWarning(o.SkipMsg)
	}
	return value, nil
}

// Strips spaces, persists the allowlist and confirms
func saveAllowlist(ctx context.Context, envVar, users, successMsg string) error {
	if err := WizardSetup.SaveEnvValue(ctx, envVar, strings.ReplaceAll(users, " ", "")); err != nil {
		return err
	}
	WizardSetup.PrintSuccess(successMsg)
	return nil
}

// Persists the prompted answer as an allowlist, warning when it stays open
func promptAllowlist(ctx context.Context, envVar, question, successMsg, openMsg string) (string, error) {
	users := WizardSetup.Prompt(question, "", false)
	if users != "" {
		if err := saveAllowlist(ctx, envVar, users, successMsg); err != nil {
			return "", err
		}
	} else {
		WizardSetup.PrintInfo(openMsg)
	}
	return strings.ReplaceAll(users, " ", ""), nil
}

// Persists the value as an int port, warning and keeping the default when it is not one
func savePort(ctx context.Context, envVar, value, def string) error {
	if value == "" {
		return nil
	}
	n, ok := pyint.Parse(value)
	if !ok {
		WizardSetup.PrintWarning("Invalid port number, using default " + def)
		return nil
	}
	if err := WizardSetup.SaveEnvValue(ctx, envVar, n.String()); err != nil {
		return err
	}
	WizardSetup.PrintSuccess("Webhook port set to " + value)
	return nil
}

// Prompts until the answer is empty or has the bot token shape, empty meaning none
func promptTelegramBotToken() string {
	WizardSetup.PrintInfo("Create a bot via @BotFather on Telegram")
	for {
		token := WizardSetup.Prompt("Telegram bot token", "", true)
		if token == "" || isValidTelegramBotToken(token) {
			return token
		}
		WizardSetup.PrintError("Invalid token format. Expected: <numeric_id>:<alphanumeric_hash> " +
			"(e.g., 123456789:ABCdefGHI-jklMNOpqrSTUvwxYZ)")
	}
}

// Warns about a kept Telegram config with no user allowlist and offers to add one
func telegramAllowlistNudge(ctx context.Context) error {
	v, err := WizardSetup.GetEnvValue(ctx, "TELEGRAM_ALLOWED_USERS")
	if err != nil || v != "" {
		return err
	}
	WizardSetup.PrintInfo("⚠️  Telegram has no user allowlist - anyone can use your bot!")
	if !WizardSetup.PromptYesNo("Add allowed users now?", true) {
		return nil
	}
	WizardSetup.PrintInfo("   To find your Telegram user ID: message @userinfobot")
	if allowed := WizardSetup.Prompt("Allowed user IDs (comma-separated)", "", false); allowed != "" {
		return saveAllowlist(ctx, "TELEGRAM_ALLOWED_USERS", allowed, "Telegram allowlist configured")
	}
	return nil
}

// Configures the Telegram bot token, allowlist and home channel
func setupTelegram(ctx context.Context) error {
	WizardSetup.PrintHeader("Telegram")
	declined, err := PlatformSetup.DeclinesReconfigure(ctx, "Telegram", "Reconfigure Telegram?", "TELEGRAM_BOT_TOKEN")
	if err != nil {
		return err
	}
	if declined {
		return telegramAllowlistNudge(ctx)
	}
	token := promptTelegramBotToken()
	if token == "" {
		return nil
	}
	if err := WizardSetup.SaveEnvValue(ctx, "TELEGRAM_BOT_TOKEN", token); err != nil {
		return err
	}
	WizardSetup.PrintSuccess("Telegram token saved")
	WizardSetup.Info(nil, "🔒 Security: Restrict who can use your bot",
		"   To find your Telegram user ID:",
		"   1. Message @userinfobot on Telegram",
		"   2. It will reply with your numeric ID (e.g., 123456789)", nil)
	allowed, err := PlatformSetup.PromptAllowlist(ctx, "TELEGRAM_ALLOWED_USERS",
		"Allowed user IDs (comma-separated, leave empty for open access)",
		"Telegram allowlist configured - only listed users can use the bot",
		"⚠️  No allowlist set - anyone who finds your bot can use it!")
	if err != nil {
		return err
	}
	WizardSetup.Info(nil, "📬 Home Channel: where Icebath delivers cron job results,",
		"   cross-platform messages, and notifications.",
		"   For Telegram DMs, this is your user ID (same as above).")
	first := ""
	if allowed != "" {
		head, _, _ := strings.Cut(allowed, ",")
		first = strings.TrimSpace(head)
	}
	switch {
	case first == "":
		WizardSetup.PrintInfo("   You can also set this later by typing /set-home in your Telegram chat.")
		_, err = PlatformSetup.SavePrompted(ctx, "TELEGRAM_HOME_CHANNEL", "Home channel ID (leave empty to set later)", SavePromptedOptions{})
	case WizardSetup.PromptYesNo(fmt.Sprintf("Use your user ID (%s) as the home channel?", first), true):
		if err = WizardSetup.SaveEnvValue(ctx, "TELEGRAM_HOME_CHANNEL", first); err == nil {
			WizardSetup.PrintSuccess("Telegram home channel set to " + first)
		}
	default:
		_, err = PlatformSetup.SavePrompted(ctx, "TELEGRAM_HOME_CHANNEL",
			"Home channel ID (or leave empty to set later with /set-home in Telegram)", SavePromptedOptions{})
	}
	return err
}

// Enables the webhook platform with its port and global HMAC secret
func setupWebhooks(ctx context.Context) error {
	WizardSetup.PrintHeader("Webhooks")
	declined, err := PlatformSetup.DeclinesReconfigure(ctx, "Webhooks", "Reconfigure webhooks?", "WEBHOOK_ENABLED")
	if err != nil || declined {
		return err
	}
	setupPrintln("")
	WizardSetup.PrintWarning("⚠  Webhook and SMS platforms require exposing gateway ports to the")
	WizardSetup.PrintWarning("   internet. For security, run the gateway in a sandboxed environment")
	WizardSetup.PrintWarning("   (Docker, VM, etc.) to limit blast radius from prompt injection.")
	setupPrintln("")
	WizardSetup.Info("   Full guide: icebath webhook --help", nil)
	if err := PlatformSetup.SavePort(ctx, "WEBHOOK_PORT", WizardSetup.Prompt("Webhook port (default 8644)", "", false), "8644"); err != nil {
		return err
	}
	if _, err := PlatformSetup.SavePrompted(ctx, "WEBHOOK_SECRET", "Global HMAC secret (shared across all routes)", SavePromptedOptions{
		Password: true, SuccessMsg: "Webhook secret saved",
		SkipMsg: "No secret set — you must configure per-route secrets in config.yaml"}); err != nil {
		return err
	}
	if err := WizardSetup.SaveEnvValue(ctx, "WEBHOOK_ENABLED", "true"); err != nil {
		return err
	}
	setupPrintln("")
	WizardSetup.PrintSuccess("Webhooks enabled! Next steps:")
	WizardSetup.Info(fmt.Sprintf("   1. Define webhook routes in %s/config.yaml", home.Display(home.Dir(ctx))),
		"   2. Point your service (GitHub, GitLab, etc.) at:",
		"      http://your-server:8644/webhooks/<route-name>", nil,
		"   Route configuration guide:",
		"   icebath webhook subscribe --help",
		nil,
		"   Open config in your editor:  icebath config edit",
		"   Open config in your editor:  icebath config edit")
	return nil
}

// Platform label, credential env var and the home channel vars any one of which satisfies it
var homeChannelChecks = []struct {
	label, tokenVar string
	homeVars        []string
}{
	{"Telegram", "TELEGRAM_BOT_TOKEN", []string{"TELEGRAM_HOME_CHANNEL"}},
	{"Discord", "DISCORD_BOT_TOKEN", []string{"DISCORD_HOME_CHANNEL"}},
	{"Slack", "SLACK_BOT_TOKEN", []string{"SLACK_HOME_CHANNEL"}},
	{"BlueBubbles", "BLUEBUBBLES_SERVER_URL", []string{"BLUEBUBBLES_HOME_CHANNEL"}},
	{"QQBot", "QQ_APP_ID", []string{"QQBOT_HOME_CHANNEL", "QQ_HOME_CHANNEL"}},
}

// Warns about platforms with a credential but no home channel
func warnMissingHomeChannels(ctx context.Context) error {
	var missing []string
	for _, check := range homeChannelChecks {
		token, err := WizardSetup.GetEnvValue(ctx, check.tokenVar)
		if err != nil {
			return err
		}
		if token == "" {
			continue
		}
		hasHome := false
		for _, name := range check.homeVars {
			v, err := WizardSetup.GetEnvValue(ctx, name)
			if err != nil {
				return err
			}
			if v != "" {
				hasHome = true
				break
			}
		}
		if !hasHome {
			missing = append(missing, check.label)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	setupPrintln("")
	WizardSetup.PrintWarning("No home channel set for: " + strings.Join(missing, ", "))
	lines := []any{"   Without a home channel, cron jobs and cross-platform",
		"   messages can't be delivered to those platforms.",
		"   Set one later with /set-home in your chat, or:"}
	for _, label := range missing {
		lines = append(lines, fmt.Sprintf("     icebath config set %s_HOME_CHANNEL <channel_id>", strings.ToUpper(label)))
	}
	WizardSetup.Info(lines...)
	return nil
}

// Telegram credentials and allowlist wizard of the CLI setup
func SetupTelegram(ctx context.Context) error { return setupTelegram(ctx) }
