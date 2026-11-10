package gateway

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// The hermes_cli.setup functions platform wizards resolve at call time, a table tests replace like the patched module
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
	HermesHome      func(ctx context.Context) string
}

// The hermes_cli.setup module as the platform wizards see it
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
	GetEnvValue: func(ctx context.Context, key string) (string, error) {
		v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
		return v, err
	},
	SaveEnvValue: func(ctx context.Context, key, value string) error {
		return config.SaveEnvValueContext(ctx, home.Dir(ctx), key, value)
	},
	HermesHome: home.Dir,
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
	PromptAllowlist     func(ctx context.Context, envVar, question, successMsg, openMsg string, preset *string) (string, error)
	SavePort            func(ctx context.Context, envVar, value, def string) error
}

// The hermes_cli.setup_platforms module as the platform wizards see it
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
	setupRule          = strings.Repeat("━", 50)
)

// Whether a token has the Telegram bot token shape
func isValidTelegramBotToken(token string) bool { return telegramBotTokenRE.MatchString(token) }

// Profile name when the home is a profiles child, the path read with Windows or POSIX separators
func profileNameFromHermesHomeFlavor(p string, nt bool) (string, bool) {
	pure, base, dir := pypath.PosixPurePathStr(p), pypath.PosixBasename, pypath.PosixDirname
	if nt {
		pure, base, dir = pypath.NtPurePathStr(p), pypath.NtBasename, pypath.NtDirname
	}
	if base(dir(pure)) != "profiles" {
		return "", false
	}
	return base(pure), true
}

// Profile name when the active home is a profiles child, read with the host path flavor
func profileNameFromHermesHome(p string) (string, bool) {
	return profileNameFromHermesHomeFlavor(p, runtime.GOOS == "windows")
}

// Runs the managed QR bot creation for the active profile
func setupTelegramAutoResult(ctx context.Context) (*TelegramBotSetupResult, error) {
	var profileName *string
	if name, ok := profileNameFromHermesHome(WizardSetup.HermesHome(ctx)); ok {
		profileName = &name
	}
	return telegramAutoSetupFn(ctx, profileName)
}

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

// Text of str.format(value=value) on a message, the KeyError or ValueError of any other field
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
				return "", errors.New("ValueError: expected '}' before end of string")
			}
			field := msg[i+1 : i+end]
			if field != "value" {
				return "", fmt.Errorf("KeyError: '%s'", field)
			}
			b.WriteString(value)
			i += end
		case c == '}':
			return "", errors.New("ValueError: Single '}' encountered in format string")
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

// Persists the preset or the prompted answer as an allowlist, warning when it stays open
func promptAllowlist(ctx context.Context, envVar, question, successMsg, openMsg string, preset *string) (string, error) {
	var users string
	if preset == nil {
		users = WizardSetup.Prompt(question, "", false)
	} else {
		users = *preset
	}
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

// Token and setup result, the automatic flow first when chosen and the manual paste otherwise
func obtainTelegramToken(ctx context.Context) (string, *TelegramBotSetupResult, error) {
	WizardSetup.Info("How would you like to create your Telegram bot?", nil,
		"  [1] Automatic (recommended)",
		"      Scan a QR code → confirm in Telegram → done.",
		"      No token copy-paste needed.", nil,
		"  [2] Manual",
		"      Create a bot via @BotFather yourself and paste the token.", nil)
	token := ""
	var result *TelegramBotSetupResult
	if pystr.Strip(WizardSetup.Prompt("Choice [1/2]", "1", false)) == "1" {
		var err error
		if result, err = setupTelegramAutoResult(ctx); err != nil {
			return "", nil, err
		}
		if result != nil {
			token = result.Token
			if !isValidTelegramBotToken(token) {
				WizardSetup.PrintError("Automatic setup returned an invalid Telegram bot token.")
				token, result = "", nil
			}
		}
		if token == "" {
			WizardSetup.Info(nil, "Falling back to manual setup...", nil)
		}
	}
	if token == "" {
		token = promptTelegramBotToken()
	}
	return token, result, nil
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
	token, result, err := obtainTelegramToken(ctx)
	if err != nil || token == "" {
		return err
	}
	if err := WizardSetup.SaveEnvValue(ctx, "TELEGRAM_BOT_TOKEN", token); err != nil {
		return err
	}
	WizardSetup.PrintSuccess("Telegram token saved")
	WizardSetup.Info(nil, "🔒 Security: Restrict who can use your bot",
		"   To find your Telegram user ID:",
		"   1. Message @userinfobot on Telegram",
		"   2. It will reply with your numeric ID (e.g., 123456789)", nil)
	var preset *string
	detected := ""
	if result != nil {
		detected = result.OwnerUserID
	}
	if detected != "" {
		WizardSetup.PrintSuccess("Detected your Telegram user ID: " + detected)
		if WizardSetup.PromptYesNo("Allow this Telegram account to use the bot?", true) {
			extra := WizardSetup.Prompt("Additional allowed user IDs (comma-separated, optional)", "", false)
			ids := []string{detected}
			for _, uid := range strings.Split(strings.ReplaceAll(extra, " ", ""), ",") {
				if uid != "" && !containsString(ids, uid) {
					ids = append(ids, uid)
				}
			}
			joined := strings.Join(ids, ",")
			preset = &joined
		}
	}
	allowed, err := PlatformSetup.PromptAllowlist(ctx, "TELEGRAM_ALLOWED_USERS",
		"Allowed user IDs (comma-separated, leave empty for open access)",
		"Telegram allowlist configured - only listed users can use the bot",
		"⚠️  No allowlist set - anyone who finds your bot can use it!", preset)
	if err != nil {
		return err
	}
	WizardSetup.Info(nil, "📬 Home Channel: where Icebath delivers cron job results,",
		"   cross-platform messages, and notifications.",
		"   For Telegram DMs, this is your user ID (same as above).")
	first := ""
	if allowed != "" {
		head, _, _ := strings.Cut(allowed, ",")
		first = pystr.Strip(head)
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
	WizardSetup.Info("   Full guide: https://hermes-agent.nousresearch.com/docs/user-guide/messaging/webhooks/", nil)
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
		"   https://hermes-agent.nousresearch.com/docs/user-guide/messaging/webhooks/#configuring-routes",
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

// Whether a platform status counts as configured
func isProgress(status string) bool {
	s := pystr.Lower(status)
	return !(s == "not configured" || strings.HasPrefix(s, "partially") || strings.HasPrefix(s, "plugin disabled"))
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
		lines = append(lines, fmt.Sprintf("     icebath config set %s_HOME_CHANNEL <channel_id>", pystr.Upper(label)))
	}
	WizardSetup.Info(lines...)
	return nil
}

// Restart and platform seams of the setup flow, the gateway module names its tests replace
var (
	setupSystemdRestart = SystemdRestart
	setupLaunchdRestart = LaunchdRestart
	setupWindowsRestart = WindowsRestart
	setupScopeNeedsRoot = systemScopeWizardWouldNeedRoot
	setupPlatformSystem = func() string {
		switch serviceGOOS {
		case "darwin":
			return "Darwin"
		case "windows":
			return "Windows"
		case "linux":
			return "Linux"
		}
		return serviceGOOS
	}
)

// Offers a restart of the running gateway after a pass that may have changed platform config
func restartRunningGateway(ctx context.Context, anyMessaging, supportsSystemd bool) error {
	con := serviceConsole(provider.SetupOutput())
	if supportsSystemd {
		needsRoot, err := setupScopeNeedsRoot(ctx, false)
		if err != nil {
			return err
		}
		if needsRoot {
			return printSystemScopeRemediation(ctx, con, "restart")
		}
	}
	if !(anyMessaging && WizardSetup.PromptYesNo("  Restart the gateway to pick up changes?", true)) {
		return nil
	}
	var err error
	switch {
	case supportsSystemd:
		err = setupSystemdRestart(ctx, false)
	case setupPlatformSystem() == "Darwin":
		err = setupLaunchdRestart(ctx)
	case setupPlatformSystem() == "Windows":
		err = setupWindowsRestart(ctx)
	}
	if err == nil {
		return nil
	}
	var unavailable *UserSystemdUnavailableError
	var needsRoot *SystemScopeRequiresRootError
	var exit *SystemExitError
	switch {
	case errors.As(err, &exit):
		return err
	case errors.As(err, &unavailable):
		WizardSetup.PrintError("  Restart failed — user systemd not reachable:")
		for _, line := range pystr.Splitlines(unavailable.Error(), false) {
			setupPrintln("  " + line)
		}
	case errors.As(err, &needsRoot):
		WizardSetup.PrintError("  Restart failed: " + needsRoot.Error())
		return printSystemScopeRemediation(ctx, con, "restart")
	default:
		WizardSetup.PrintError("  Restart failed: " + err.Error())
	}
	return nil
}

// Configures messaging platforms from a checklist, then restarts or offers to install the gateway service
func SetupGateway(ctx context.Context, cfg config.Tree) error {
	WizardSetup.PrintHeader("Messaging Platforms")
	WizardSetup.Info("Connect to messaging platforms to chat with Icebath from anywhere.", "Toggle with Space, confirm with Enter.", nil)
	platforms := AllPlatforms(ctx)
	items := make([]string, 0, len(platforms))
	var preSelected []int
	for i, p := range platforms {
		status, err := PlatformStatus(ctx, p)
		if err != nil {
			return err
		}
		items = append(items, fmt.Sprintf("%s %s  (%s)", p.Emoji, p.Label, status))
		if status == "configured" {
			preSelected = append(preSelected, i)
		}
	}
	selected := WizardSetup.PromptChecklist("Select platforms to configure:", items, preSelected)
	if len(selected) == 0 {
		WizardSetup.PrintInfo("No platforms selected. Run 'icebath setup gateway' later to configure.")
	}
	for _, idx := range selected {
		at, ok := pyIndex(idx, len(platforms))
		if !ok {
			return errors.New("IndexError: list index out of range")
		}
		if err := configurePlatform(ctx, platforms[at]); err != nil {
			return err
		}
	}
	anyMessaging := false
	for _, p := range AllPlatforms(ctx) {
		status, err := PlatformStatus(ctx, p)
		if err != nil {
			return err
		}
		if isProgress(status) {
			anyMessaging = true
			break
		}
	}
	if anyMessaging {
		setupPrintln("")
		WizardSetup.PrintInfo(setupRule)
		WizardSetup.PrintSuccess("Messaging platforms configured!")
		if err := warnMissingHomeChannels(ctx); err != nil {
			return err
		}
	}
	supportsSystemd := setupSupportsSystemd(ctx)
	setupPrintln("")
	running, err := setupServiceRunning(ctx)
	if err != nil {
		return err
	}
	if running {
		if err := restartRunningGateway(ctx, anyMessaging, supportsSystemd); err != nil {
			return err
		}
	} else if _, err := EnsureGatewayService(ctx, provider.SetupOutput(), ServiceSetupOptions{Context: "setup", Interactive: true,
		Config: map[string]any(cfg)}); err != nil {
		return err
	}
	WizardSetup.PrintInfo(setupRule)
	return nil
}

// Python sequence index of a menu answer, negative counting from the end, false past either end
func pyIndex(idx, n int) (int, bool) {
	if idx < 0 {
		idx += n
	}
	return idx, idx >= 0 && idx < n
}
