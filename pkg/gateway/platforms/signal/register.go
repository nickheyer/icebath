package signal

import (
	"context"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Daemon URL and account both present in the home env file
var envConfigured = gateway.EnvIsConnected("SIGNAL_HTTP_URL", "SIGNAL_ACCOUNT")

// URL and account from the config or scoped env, else from the home env file a plain CLI never loads
func isConnected(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	valid, err := validateSignalConfig(ctx, config)
	if err != nil || valid {
		return valid, err
	}
	return envConfigured(ctx, config)
}

// Config validation with the warning the core instantiation path logs for a missing URL or account
func validateConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	valid, err := validateSignalConfig(ctx, config)
	if err != nil || valid {
		return valid, err
	}
	slog.Warn("Signal: SIGNAL_HTTP_URL or SIGNAL_ACCOUNT not configured")
	return false, nil
}

// System prompt platform hint, cached prompt bytes kept stable
const platformHint = "You are on Signal. Standard markdown (**bold**, *italic*, ~~strike~~, # headers, `code`) auto-converts to " +
	"Signal formatting; bullets render as •. No tables — use bullets or labeled lines. " +
	"You can send files natively: write MEDIA:/absolute/path/to/file in your response. " +
	"Images (.png, .jpg, .webp) send as photos, other files as documents; ![alt](url) sends as photos."

// Native group target of a group prefixed reference, not explicit for an empty id and nil otherwise
func parseSendTarget(ref string) any {
	stripped := pystr.Strip(ref)
	if !strings.HasPrefix(stripped, "group:") {
		return nil
	}
	groupID := pystr.Strip(stripped[len("group:"):])
	if groupID == "" {
		return gateway.NotExplicit
	}
	chatID := "group:" + groupID
	return &gateway.TargetRefParse{ChatID: &chatID}
}

// Builds the adapter for a platform config, the factory the registry and the adapter class share
func newAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
	adapter, err := NewSignalAdapter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return adapter, nil
}

// Signal platform plugin compiled into the binary under the bundled platforms/signal key
type signalPlatformPlugin struct{}

func (signalPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "signal-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/signal",
		Author: "NousResearch",
		Description: "Signal gateway adapter for Icebath Agent via a signal-cli daemon in HTTP mode (SSE inbound, JSON-RPC 2.0 outbound). " +
			"Supports DMs, allowlisted groups, attachments, reactions and home-channel cron delivery. Phone numbers are treated as PII in logs.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "SIGNAL_HTTP_URL", Description: "signal-cli HTTP daemon URL (e.g. http://127.0.0.1:8080)", URL: "https://github.com/AsamK/signal-cli"},
			{Name: "SIGNAL_ACCOUNT", Description: "Signal account phone number in E.164 format (e.g. +15551234567)"}}}
}

// Registers the Signal platform entry with its factory, checks, setup, target syntax and standalone sender
func (signalPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn":   parseSendTarget,
		"standalone_send":        &gateway.StandaloneSendPolicy{Label: "Signal", Media: true, EmptyListBetweenChunks: true, TextDirect: true},
		"dashboard_description":  "Connect through a signal-cli REST bridge.",
		"docs_url":               "https://github.com/bbernhard/signal-cli-rest-api",
		"dashboard_env_vars":     []string{"SIGNAL_HTTP_URL", "SIGNAL_ACCOUNT", "SIGNAL_ALLOWED_USERS"},
		"scoped_validate_config": validateConfig,
		"is_connected":           isConnected,
		"config_connected_fn":    gateway.ConfigConnectedExtra("http_url"),
		"setup_fn":               InteractiveSetup,
		"cron_deliver_env_var":   "SIGNAL_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     maxMessageLength,
		"emoji":                  "📡",
		"pii_safe":               true,
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "low",
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "signal", Label: "Signal",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return newAdapter(fctx, cfg)
		}),
		Check:       checkSignalRequirements,
		RequiredEnv: []string{"SIGNAL_HTTP_URL", "SIGNAL_ACCOUNT"},
		InstallHint: "Signal: runtime requirements not met",
		Extra:       extra,
	})
	return err
}

var _ gateway.PlatformAdapter = (*SignalAdapter)(nil)

func init() {
	gateway.RegisterAdapterModule(&gateway.AdapterClass{Name: "SignalAdapter", New: newAdapter, Traits: adapterTraits()})
	plugins.RegisterBundled(signalPlatformPlugin{})
}
