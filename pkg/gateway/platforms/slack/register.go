package slack

import (
	"context"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

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
		Author: "Icebath",
		Description: "Slack gateway adapter for Icebath Agent. Connects to Slack via slack-go in Socket Mode and relays messages " +
			"between Slack channels/DMs and the Icebath agent. Supports slash commands, threads, mrkdwn rendering, approval blocks, " +
			"free-response channels, mention gating, and channel skill bindings.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "SLACK_BOT_TOKEN", Description: "Slack bot token (xoxb-...)", URL: "https://api.slack.com/apps", Secret: true},
			{Name: "SLACK_APP_TOKEN", Description: "Slack app-level token for Socket Mode (xapp-..., scope connections:write)", URL: "https://api.slack.com/apps", Secret: true}}}
}

// Registers the Slack platform entry with its factory, setup, YAML bridge and standalone sender
func (slackPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn":   parseSendTarget,
		"standalone_send":        &gateway.StandaloneSendPolicy{Label: "Slack", Media: true, Captionable: true, EmptyListBetweenChunks: true},
		"ensure_deps_fn":         checkSlackRequirements,
		"is_connected":           gateway.EnvIsConnected("SLACK_BOT_TOKEN"),
		"setup_fn":               InteractiveSetup,
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
		InstallHint: "Start `icebath` and type `/setup chat-apps` to install Slack support.",
		Extra:       extra,
	})
	return err
}

// Active dependency check, always satisfied since slack-go is compiled into the binary
func checkSlackRequirements(context.Context) (bool, error) { return true, nil }

func init() { plugins.RegisterBundled(slackPlatformPlugin{}) }
