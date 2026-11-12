package slack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// First n code points of s, Python slicing
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Full Slack app manifest, display info, the registry slash list and the default scopes, events and settings
func BuildFullManifest(ctx context.Context, botName, botDescription string, includeAssistant bool, messagingExperience, longDescription *string) (*pyjson.Object, error) {
	experience := "none"
	if includeAssistant {
		experience = "assistant"
	}
	if messagingExperience != nil {
		experience = *messagingExperience
	}
	experience = pystr.Lower(pystr.Strip(experience))
	if experience != "assistant" && experience != "agent" && experience != "none" {
		return nil, errors.New("ValueError: messaging_experience must be one of: assistant, agent, none")
	}
	slashes, err := slashCommandEntries(ctx)
	if err != nil {
		return nil, err
	}
	features := pyjson.ObjectOf(
		"app_home", pyjson.ObjectOf("home_tab_enabled", false, "messages_tab_enabled", true, "messages_tab_read_only_enabled", false),
		"bot_user", pyjson.ObjectOf("display_name", truncateRunes(botName, 80), "always_online", true),
		"slash_commands", slashes)
	botScopes := []string{"app_mentions:read", "channels:history", "channels:read", "chat:write", "commands",
		"files:read", "files:write", "groups:history", "groups:read", "im:history", "im:read",
		"im:write", "mpim:history", "mpim:read", "reactions:read", "users:read"}
	botEvents := []string{"app_mention", "message.channels", "message.groups", "message.im", "message.mpim",
		"reaction_added", "reaction_removed"}
	switch experience {
	case "assistant":
		features.Set("assistant_view", pyjson.ObjectOf("assistant_description", "Chat with Icebath in threads and DMs."))
		botScopes = append(botScopes, "assistant:write")
		botEvents = append(botEvents, "assistant_thread_context_changed", "assistant_thread_started")
	case "agent":
		features.Set("agent_view", pyjson.ObjectOf("agent_description", "Chat with Icebath in Slack Messages."))
		botScopes = append(botScopes, "assistant:write")
		botEvents = append(botEvents, "app_context_changed", "app_home_opened")
	}
	sort.Strings(botScopes)
	sort.Strings(botEvents)
	if botDescription == "" {
		botDescription = "Your Icebath agent on Slack"
	}
	display := pyjson.ObjectOf("name", truncateRunes(botName, 35), "description", truncateRunes(botDescription, 140),
		"background_color", "#1a1a2e")
	if longDescription != nil {
		display.Set("long_description", *longDescription)
	}
	return pyjson.ObjectOf(
		"_metadata", pyjson.ObjectOf("major_version", 1, "minor_version", 1),
		"display_information", display,
		"features", features,
		"oauth_config", pyjson.ObjectOf("scopes", pyjson.ObjectOf("bot", toAny(botScopes))),
		"settings", pyjson.ObjectOf(
			"event_subscriptions", pyjson.ObjectOf("bot_events", toAny(botEvents)),
			"interactivity", pyjson.ObjectOf("is_enabled", true),
			"org_deploy_enabled", false,
			"socket_mode_enabled", true,
			"token_rotation_enabled", false)), nil
}

// Slash command entries of the manifest in key order, one per gateway command of the registry
func slashCommandEntries(ctx context.Context) ([]any, error) {
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, err
	}
	raw, _ := commands.SlackAppManifest(ctx, commands.Default, cfg, "")["features"].(map[string]any)["slash_commands"].([]map[string]any)
	slashes := make([]any, 0, len(raw))
	for _, e := range raw {
		entry := pyjson.ObjectOf("command", e["command"], "description", e["description"], "should_escape", e["should_escape"], "url", e["url"])
		if hint, ok := e["usage_hint"]; ok {
			entry.Set("usage_hint", hint)
		}
		slashes = append(slashes, entry)
	}
	return slashes, nil
}

func toAny(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Manifest builder of the wizard, the slack_cli._build_full_manifest name its tests replace
var buildFullManifestFn = BuildFullManifest

// Steps of creating the Slack app the wizard prints first
var setupSteps = []string{
	"Steps to create a Slack app:",
	"   1. Go to https://api.slack.com/apps → Create New App",
	"      Pick 'From an app manifest' — we'll generate one for you below.",
	"   2. Enable Socket Mode: Settings → Socket Mode → Enable",
	"      • Create an App-Level Token with 'connections:write' scope",
	"   3. Install to Workspace: Settings → Install App",
	"   4. After installing, invite the bot to channels: /invite @YourBot",
}

// Home channel guidance printed before the home channel prompt
var setupHomeChannelHelp = []string{
	"📬 Home Channel: where Icebath delivers cron job results,",
	"   cross-platform messages, and notifications.",
	"   To get a channel ID: open the channel in Slack, then right-click",
	"   the channel name → Copy link — the ID starts with C (e.g. C01ABC2DE3F).",
	"   You can also set this later by typing /set-home in a Slack channel.",
}

// Writes the app manifest under the home and prints the paste instructions, a failure only warning
func writeSlackManifestAndInstruct(ctx context.Context) {
	cli := gateway.WizardCLI
	err := func() error {
		manifest, err := buildFullManifestFn(ctx, "Icebath", "Your Icebath agent on Slack", true, nil, nil)
		if err != nil {
			return err
		}
		target := filepath.Join(home.Dir(ctx), "slack-manifest.json")
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		text, err := pyjson.Dumps(manifest, pyjson.Indent(2), pyjson.EnsureASCII(false))
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(text+"\n"), 0o666); err != nil {
			return err
		}
		cli.PrintSuccess("Slack app manifest written to: " + target)
		cli.PrintInfo("   Paste it into https://api.slack.com/apps → your app → Features " +
			"→ App Manifest → Edit, then Save.  Slack will prompt to " +
			"reinstall if scopes or slash commands changed.")
		cli.PrintInfo("   Re-run `icebath slack manifest --write` anytime to refresh after " +
			"Icebath adds new commands.")
		return nil
	}()
	if err != nil {
		cli.PrintWarning("Could not write Slack manifest: " + err.Error())
	}
}

// Slack wizard of icebath gateway setup writing the manifest, tokens, allowlist and home channel
func InteractiveSetup(ctx context.Context) error {
	cli, cfg := gateway.WizardCLI, gateway.WizardConfig
	cli.PrintHeader("Slack")
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "Slack", "Reconfigure Slack?", "SLACK_BOT_TOKEN")
	if err != nil {
		return err
	}
	if declined {
		if cli.PromptYesNo(ctx, "Regenerate the Slack app manifest with the latest command "+
			"list? (recommended after `icebath update`)", true) {
			writeSlackManifestAndInstruct(ctx)
		}
		return nil
	}
	for _, line := range setupSteps {
		cli.PrintInfo(line)
	}
	gateway.WizardPrintln("")
	cli.PrintInfo("   Full guide: https://hermes-agent.nousresearch.com/docs/user-guide/messaging/slack/")
	gateway.WizardPrintln("")
	writeSlackManifestAndInstruct(ctx)
	gateway.WizardPrintln("")
	botToken := cli.Prompt("Slack Bot Token (xoxb-...)", "", true)
	if botToken == "" {
		return nil
	}
	if err := cfg.SaveEnvValue(ctx, "SLACK_BOT_TOKEN", botToken); err != nil {
		return err
	}
	if appToken := cli.Prompt("Slack App Token (xapp-...)", "", true); appToken != "" {
		if err := cfg.SaveEnvValue(ctx, "SLACK_APP_TOKEN", appToken); err != nil {
			return err
		}
	}
	cli.PrintSuccess("Slack tokens saved")
	gateway.WizardPrintln("")
	cli.PrintInfo("🔒 Security: Restrict who can use your bot")
	cli.PrintInfo("   To find a Member ID: click a user's name → View full profile → ⋮ → Copy member ID")
	gateway.WizardPrintln("")
	if allowed := cli.Prompt("Allowed user IDs (comma-separated, leave empty to deny everyone except paired users)", "", false); allowed != "" {
		if err := cfg.SaveEnvValue(ctx, "SLACK_ALLOWED_USERS", strings.ReplaceAll(allowed, " ", "")); err != nil {
			return err
		}
		cli.PrintSuccess("Slack allowlist configured")
	} else {
		cli.PrintWarning("⚠️  No Slack allowlist set - unpaired users will be denied by default.")
		cli.PrintInfo("   Set SLACK_ALLOW_ALL_USERS=true or GATEWAY_ALLOW_ALL_USERS=true only if you intentionally want open workspace access.")
	}
	gateway.WizardPrintln("")
	for _, line := range setupHomeChannelHelp {
		cli.PrintInfo(line)
	}
	if channel := pystr.Strip(cli.Prompt("Home channel ID (leave empty to set later with /set-home)", "", false)); channel != "" {
		return cfg.SaveEnvValue(ctx, "SLACK_HOME_CHANNEL", channel)
	}
	removed, err := cfg.RemoveEnvValue(ctx, "SLACK_HOME_CHANNEL")
	if err == nil && removed {
		cli.PrintInfo("Home channel cleared.")
	}
	return err
}
