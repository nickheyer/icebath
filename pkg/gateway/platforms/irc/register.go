package irc

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Env var, extra key and conversion of every IRC value seeded from an env only setup
var envSeedKeys = []gateway.EnvSeedRow{
	{Env: "IRC_PORT", Key: "port", Conv: func(raw string) (any, error) {
		n, err := pyint.Int64(raw)
		if pe, ok := err.(*pyint.Error); ok {
			return nil, &gateway.PyError{Kind: pe.Kind, Msg: pe.Msg}
		}
		return n, err
	}},
	{Env: "IRC_NICKNAME", Key: "nickname"},
	{Env: "IRC_USE_TLS", Key: "use_tls", Conv: func(raw string) (any, error) { return truthyValues[pystr.Lower(raw)], nil }},
	{Env: "IRC_SERVER_PASSWORD", Key: "server_password"},
	{Env: "IRC_NICKSERV_PASSWORD", Key: "nickserv_password"},
}

// Whether the scoped env names both a server and a channel
func checkRequirements(ctx context.Context) bool {
	server, err := gateway.GetScopedSecret(ctx, "IRC_SERVER", "", false)
	if err != nil || server == "" {
		return false
	}
	channel, err := gateway.GetScopedSecret(ctx, "IRC_CHANNEL", "", false)
	return err == nil && channel != ""
}

// Whether the env or the config extra names both a server and a channel
func validateConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	server, channel, err := serverChannel(ctx, config)
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(server) && pyrepr.Truthy(channel), nil
}

// Whether IRC is configured through the env or the config extra
func isConnected(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	return validateConfig(ctx, config)
}

// Extra seeded from an env only setup with the home channel defaulting to the joined channel, nil when unconfigured
func envEnablement(ctx context.Context) map[string]any {
	read := func(name string) string {
		val, err := gateway.GetScopedSecret(ctx, name, "", false)
		if err != nil {
			panic(err)
		}
		return pystr.Strip(val)
	}
	server, channel := read("IRC_SERVER"), read("IRC_CHANNEL")
	if server == "" || channel == "" {
		return nil
	}
	seed, err := gateway.SeedExtraFromEnv(ctx, envSeedKeys, "IRC_HOME_CHANNEL", channel)
	if err != nil {
		panic(err)
	}
	out := map[string]any{"server": server, "channel": channel}
	for k, v := range seed {
		out[k] = v
	}
	return out
}

// Model facing description of IRC formatting and line limits
const platformHint = "You are chatting via IRC. IRC does not support markdown formatting " +
	"— use plain text only. Messages are limited to ~450 characters per " +
	"line (long messages are automatically split). In channels, users " +
	"address you by prefixing your nick. Keep responses concise and " +
	"conversational."

// IRC platform plugin compiled into the binary under the bundled platforms/irc key
type ircPlatformPlugin struct{}

func (ircPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "irc-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/irc",
		Author: "Nous Research",
		Description: "IRC gateway adapter for Icebath Agent. Connects to an IRC server and relays messages between an IRC channel " +
			"(or DMs) and the Icebath agent.  No external dependencies — uses the Go standard library net package for the " +
			"IRC protocol.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "IRC_SERVER", Description: "IRC server hostname (e.g. irc.libera.chat)"},
			{Name: "IRC_CHANNEL", Description: "Channel to join (e.g. #icebath — comma-separate for multiple)"},
			{Name: "IRC_NICKNAME", Description: "Bot nickname on IRC (default: icebath-bot)"}}}
}

// Adapter class the gateway reads the serial receive loop trait from
var ircAdapterClass = &gateway.AdapterClass{Name: "IRCAdapter", Traits: ircAdapterTraits(),
	New: func(ctx context.Context, config *gateway.PlatformConfig) (gateway.PlatformAdapter, error) {
		return NewIRCAdapter(ctx, config)
	}}

// Registers the IRC platform entry with its factory, gates, setup, env seeding, standalone sender and hint
func (ircPlatformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "irc", Label: "IRC",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewIRCAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"IRC_SERVER", "IRC_CHANNEL", "IRC_NICKNAME"},
		InstallHint: "No extra packages needed (stdlib only)",
		Extra: map[string]any{
			"dashboard_description":  "Relay messages between an IRC channel (or DMs) and Icebath.",
			"docs_url":               "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/irc",
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           isConnected,
			"setup_fn":               InteractiveSetup,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "IRC_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "IRC_ALLOWED_USERS",
			"allow_all_env":          "IRC_ALLOW_ALL_USERS",
			"max_message_length":     450,
			"emoji":                  "💬",
			"pii_safe":               false,
			"allow_update_command":   true,
			"platform_hint":          platformHint,
		},
	})
	return err
}

func init() {
	gateway.RegisterAdapterModule(ircAdapterClass)
	plugins.RegisterBundled(ircPlatformPlugin{})
}
