package irc

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const defaultNickname = "icebath-bot"

// Env values that switch TLS on
var truthyWords = map[string]bool{"1": true, "true": true, "yes": true}

// Connection settings of one IRC identity, port zero when none is configured
type settings struct {
	server           string
	channel          string
	nickname         string
	serverPassword   string
	nickservPassword string
	port             int
	portRaw          any
	useTLS           bool
}

// Text form of a config value, empty for nil
func textOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
	}
}

// Integer of a config value, false when it is not a whole number
func intOf(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		if x == float64(int(x)) {
			return int(x), true
		}
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err == nil
	}
	return 0, false
}

// Scoped env value when set and non empty, else the extra value, else the default
func envOrExtra(ctx context.Context, extra map[string]any, env, key string, def any) (any, error) {
	v, err := gateway.GetScopedSecret(ctx, env, "", false)
	if err != nil {
		return nil, err
	}
	if v != "" {
		return v, nil
	}
	if x, ok := extra[key]; ok && x != nil {
		return x, nil
	}
	return def, nil
}

// Port IRC networks listen on, 6697 with TLS and 6667 without
func defaultPort(useTLS bool) int {
	if useTLS {
		return 6697
	}
	return 6667
}

// Settings from the scoped env over extra, the port left zero when unset or not a number
func loadSettings(ctx context.Context, extra map[string]any) (settings, error) {
	var s settings
	text := func(env, key, def string) (string, error) {
		v, err := envOrExtra(ctx, extra, env, key, def)
		return strings.TrimSpace(textOf(v)), err
	}
	var err error
	if s.server, err = text("IRC_SERVER", "server", ""); err != nil {
		return s, err
	}
	if s.channel, err = text("IRC_CHANNEL", "channel", ""); err != nil {
		return s, err
	}
	if s.nickname, err = text("IRC_NICKNAME", "nickname", defaultNickname); err != nil {
		return s, err
	}
	if s.nickname == "" {
		s.nickname = defaultNickname
	}
	if s.serverPassword, err = text("IRC_SERVER_PASSWORD", "server_password", ""); err != nil {
		return s, err
	}
	if s.nickservPassword, err = text("IRC_NICKSERV_PASSWORD", "nickserv_password", ""); err != nil {
		return s, err
	}
	tlsEnv, err := gateway.GetScopedSecret(ctx, "IRC_USE_TLS", "", false)
	if err != nil {
		return s, err
	}
	if tlsEnv = strings.TrimSpace(tlsEnv); tlsEnv != "" {
		s.useTLS = truthyWords[strings.ToLower(tlsEnv)]
	} else {
		switch v := extra["use_tls"].(type) {
		case bool:
			s.useTLS = v
		case string:
			s.useTLS = truthyWords[strings.ToLower(strings.TrimSpace(v))]
		case nil:
			s.useTLS = true
		default:
			n, ok := intOf(v)
			s.useTLS = !ok || n != 0
		}
	}
	if s.portRaw, err = envOrExtra(ctx, extra, "IRC_PORT", "port", nil); err != nil {
		return s, err
	}
	if n, ok := intOf(s.portRaw); ok && n > 0 && n < 65536 {
		s.port = n
	}
	return s, nil
}

// Plugin gate requiring the server and channel in the scoped env
func checkRequirements(ctx context.Context) bool {
	for _, name := range []string{"IRC_SERVER", "IRC_CHANNEL"} {
		v, err := gateway.GetScopedSecret(ctx, name, "", false)
		if err != nil || strings.TrimSpace(v) == "" {
			return false
		}
	}
	return true
}

// Whether the scoped env or extra name both a server and a channel
func validateConfig(ctx context.Context, config *gateway.PlatformConfig) (bool, error) {
	var extra map[string]any
	if config != nil {
		extra = config.Extra
	}
	s, err := loadSettings(ctx, extra)
	if err != nil {
		return false, err
	}
	return s.server != "" && s.channel != "", nil
}

// Env var, extra key and conversion of every IRC value seeded from an env only setup
var envSeedRows = []gateway.EnvSeedRow{
	{Env: "IRC_NICKNAME", Key: "nickname"},
	{Env: "IRC_USE_TLS", Key: "use_tls", Conv: func(raw string) (any, error) { return truthyWords[strings.ToLower(raw)], nil }},
	{Env: "IRC_SERVER_PASSWORD", Key: "server_password"},
	{Env: "IRC_NICKSERV_PASSWORD", Key: "nickserv_password"},
}

// Seeds extra from an env only setup with the home channel defaulting to the joined channel, nil when unconfigured
func envEnablement(ctx context.Context) map[string]any {
	read := func(name string) string {
		v, err := gateway.GetScopedSecret(ctx, name, "", false)
		if err != nil {
			panic(err)
		}
		return strings.TrimSpace(v)
	}
	server, channel := read("IRC_SERVER"), read("IRC_CHANNEL")
	if server == "" || channel == "" {
		return nil
	}
	seed, err := gateway.SeedExtraFromEnv(ctx, envSeedRows, "IRC_HOME_CHANNEL", channel)
	if err != nil {
		panic(err)
	}
	out := map[string]any{"server": server, "channel": channel}
	if port, err := strconv.Atoi(read("IRC_PORT")); err == nil {
		out["port"] = port
	}
	for k, v := range seed {
		out[k] = v
	}
	return out
}

// Model facing description of IRC rendering, line limits and addressing
const platformHint = "You are chatting via IRC. IRC does not support markdown formatting " +
	"— use plain text only. Messages are limited to ~450 characters per " +
	"line (long messages are automatically split). In channels, users " +
	"address you by prefixing your nick. Keep responses concise and " +
	"conversational."

// IRC platform plugin compiled into the binary under the bundled platforms/irc key
type platformPlugin struct{}

func (platformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "irc-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/irc",
		Author: "heyer.app",
		Description: "IRC gateway adapter for Icebath. Connects to an IRC server over plain TCP or TLS and relays messages " +
			"between IRC channels or direct messages and the Icebath agent.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "IRC_SERVER", Description: "IRC server hostname (e.g. irc.libera.chat)"},
			{Name: "IRC_CHANNEL", Description: "Channel to join (e.g. #icebath — comma-separate for multiple)"},
			{Name: "IRC_NICKNAME", Description: "Bot nickname on IRC (default: icebath-bot)"}}}
}

// Registers the IRC platform with its factory, gates, wizard, env seeding, standalone sender and hint
func (platformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "irc", Label: "IRC",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"IRC_SERVER", "IRC_CHANNEL", "IRC_NICKNAME"},
		Extra: map[string]any{
			"dashboard_description":  "Relay messages between an IRC channel (or DMs) and Icebath.",
			"docs_url":               "https://libera.chat/guides/",
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           validateConfig,
			"setup_fn":               InteractiveSetup,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "IRC_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "IRC_ALLOWED_USERS",
			"allow_all_env":          "IRC_ALLOW_ALL_USERS",
			"max_message_length":     defaultMaxMessageLength,
			"emoji":                  "💬",
			"pii_safe":               false,
			"allow_update_command":   true,
			"platform_hint":          platformHint,
		},
	})
	return err
}

func init() {
	plugins.RegisterBundled(platformPlugin{})
}
