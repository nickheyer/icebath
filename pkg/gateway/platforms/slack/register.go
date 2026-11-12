package slack

import (
	"context"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// YAML key, env var and encoding kind of every slack block key bridged into the environment
var yamlBridge = []gateway.YAMLBridgeRow{
	{Key: "require_mention", Env: "SLACK_REQUIRE_MENTION", Kind: "lower"}, {Key: "strict_mention", Env: "SLACK_STRICT_MENTION", Kind: "lower"},
	{Key: "ignore_other_user_mentions", Env: "SLACK_IGNORE_OTHER_USER_MENTIONS", Kind: "lower"},
	{Key: "thread_require_mention", Env: "SLACK_THREAD_REQUIRE_MENTION", Kind: "lower"}, {Key: "allow_bots", Env: "SLACK_ALLOW_BOTS", Kind: "lower"},
	{Key: "thread_followup_silence", Env: "SLACK_THREAD_FOLLOWUP_SILENCE", Kind: "lower"},
	{Key: "reactions", Env: "SLACK_REACTIONS", Kind: "lower"}, {Key: "disable_dms", Env: "SLACK_DISABLE_DMS", Kind: "lower"},
	{Key: "free_response_channels", Env: "SLACK_FREE_RESPONSE_CHANNELS", Kind: "csv"},
	{Key: "require_mention_channels", Env: "SLACK_REQUIRE_MENTION_CHANNELS", Kind: "csv"},
	{Key: "reaction_triggers", Env: "SLACK_REACTION_TRIGGERS", Kind: "csv"}, {Key: "reaction_trigger_target", Env: "SLACK_REACTION_TRIGGER_TARGET", Kind: "str"},
	{Key: "allowed_channels", Env: "SLACK_ALLOWED_CHANNELS", Kind: "csv"}, {Key: "ignored_channels", Env: "SLACK_IGNORED_CHANNELS", Kind: "csv"},
}

// Bridges slack YAML keys into SLACK env vars, explicit env winning, and returns the extra they seed
func applyYAMLConfig(ctx context.Context, _ map[string]any, slackCfg map[string]any) (map[string]any, error) {
	return gateway.ApplyYAMLBridge(ctx, slackCfg, yamlBridge)
}

// Slack conversation, user, mention and handle target forms, the thread form ahead of the bare id
var sendForms = []struct {
	re        string
	template  string
	hasThread bool
}{
	{`^\s*([CGD][A-Z0-9]{8,}):([^\s:]+)\s*$`, "%s", true},
	{`^\s*([CGD][A-Z0-9]{8,})\s*$`, "%s", false},
	{`^\s*(U[A-Z0-9]{8,})\s*$`, "user:%s", false},
	{`^\s*<@(U[A-Z0-9]{8,})(?:\|[^>]+)?>\s*$`, "user:%s", false},
	{`^\s*@([A-Za-z0-9._-]{1,80})\s*$`, "user_name:%s", false},
}

var sendFormRes = func() []*regexp2.Regexp {
	out := make([]*regexp2.Regexp, len(sendForms))
	for i, f := range sendForms {
		out[i] = mustPyRe(`\A(?:` + f.re + `)\Z`)
	}
	return out
}()

// Chat id and thread id of a Slack send target, nil when no form matches
func parseSendTarget(ref string) any {
	for i, f := range sendForms {
		m, ok := search(sendFormRes[i], ref)
		if !ok {
			continue
		}
		chatID := fmtTemplate(f.template, m.groups[1])
		parsed := &gateway.TargetRefParse{ChatID: &chatID}
		if f.hasThread {
			thread := m.groups[2]
			parsed.ThreadID = &thread
		}
		return parsed
	}
	return nil
}

// Template with its value filled in
func fmtTemplate(template, value string) string {
	if template == "%s" {
		return value
	}
	return template[:len(template)-2] + value
}

// Slack platform plugin compiled into the binary under the bundled platforms/slack key
type slackPlatformPlugin struct{}

func (slackPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "slack-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/slack",
		Author: "NousResearch",
		Description: "Slack gateway adapter for Icebath Agent. Connects to Slack via slack-go in Socket Mode and relays messages " +
			"between Slack channels/DMs and the Icebath agent. Supports slash commands, threads, mrkdwn rendering, approval blocks, " +
			"free-response channels, mention gating, and channel skill bindings.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "SLACK_BOT_TOKEN", Description: "Slack bot token (xoxb-...)", URL: "https://api.slack.com/apps", Secret: true},
			{Name: "SLACK_APP_TOKEN", Description: "Slack app-level token for Socket Mode (xapp-..., scope connections:write)", URL: "https://api.slack.com/apps", Secret: true}}}
}

// Registers the Slack platform entry with its factory, setup, YAML bridge, standalone sender and card
func (slackPlatformPlugin) Register(ctx *plugins.Context) error {
	var card gateway.PlatformEntry
	applyDashboardCard(&card)
	extra := map[string]any{
		"target_ref_syntax_fn":  parseSendTarget,
		"standalone_send":       &gateway.StandaloneSendPolicy{Label: "Slack", Media: true, Captionable: true, EmptyListBetweenChunks: true},
		"dashboard_description": card.DashboardDescription, "docs_url": card.DocsURL,
		"dashboard_env_vars": card.DashboardEnvVars, "env_value_rules": card.EnvValueRules,
		"ensure_deps_fn":         checkSlackRequirements,
		"is_connected":           gateway.EnvIsConnected("SLACK_BOT_TOKEN"),
		"setup_fn":               InteractiveSetup,
		"apply_yaml_config_fn":   applyYAMLConfig,
		"cron_deliver_env_var":   "SLACK_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     maxMessageLength,
		"emoji":                  "💼",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "medium",
		"display_overrides":      map[string]any{"tool_progress": "off", "long_running_notifications": false, "busy_ack_detail": false},
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "slack", Label: "Slack",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewSlackAdapter(fctx, cfg), nil
		}),
		Check:       slackDepsPresent,
		RequiredEnv: []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"},
		InstallHint: "Run `icebath setup` to install Slack support.",
		Extra:       extra,
	})
	return err
}

// Active dependency check, always satisfied since slack-go is compiled into the binary
func checkSlackRequirements(context.Context) (bool, error) { return true, nil }

func init() { plugins.RegisterBundled(slackPlatformPlugin{}) }
